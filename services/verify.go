package services

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	ra "github.com/webtor-io/rest-api/services"
)

// verifyExistingS3Object runs piece-hash verification against an S3 object
// that was previously stored. Returns nil on success, error on mismatch or
// inability to verify. Caller treats any error as "drop and re-upload".
func (s *Worker) verifyExistingS3Object(ctx context.Context, hash string, item ra.ListItem, mi *metainfo.Info) error {
	fileOff := fileOffsetInTorrent(mi, item.PathStr, item.Size)
	if fileOff < 0 {
		return errors.Errorf("verify: file %q (size %d) not found in torrent metainfo", item.PathStr, item.Size)
	}
	return verifyFileAgainstMetainfo(ctx, s.s3.Get(), s.bucket, hash, mi, fileOff, item.Size)
}

// fileOffsetInTorrent returns the byte offset at which the given file
// starts inside the torrent's global piece sequence, or -1 if not found.
// pathStr is the rest-api's ListItem.PathStr — leading "/" plus
// "/"-joined path components. For multi-file torrents the path also
// carries the root directory (Info.Name) as its first component, so we
// strip that before matching against Info.Files[i].Path. Single-file
// torrents match the trimmed path directly against Info.Name.
func fileOffsetInTorrent(mi *metainfo.Info, pathStr string, length int64) int64 {
	trimmed := strings.TrimPrefix(pathStr, "/")
	if len(mi.Files) == 0 {
		if trimmed == mi.Name {
			return 0
		}
		return -1
	}
	withoutRoot := strings.TrimPrefix(trimmed, mi.Name+"/")
	var off int64
	for _, f := range mi.Files {
		joined := strings.Join(f.Path, "/")
		if (joined == withoutRoot || joined == trimmed) && f.Length == length {
			return off
		}
		off += f.Length
	}
	return -1
}

// parseMetainfo decodes a bencode'd .torrent file and returns its Info dict.
func parseMetainfo(data []byte) (*metainfo.Info, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return nil, errors.Wrap(err, "failed to load metainfo")
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal info")
	}
	return &info, nil
}

// verifyFileAgainstMetainfo recomputes BitTorrent piece SHA-1 hashes for the
// S3 object at (bucket, key) and compares them with the torrent's piece
// hashes. Only pieces fully contained in this file are checked: pieces that
// span a file boundary need bytes from neighboring files and aren't covered
// here. Boundary pieces are typically at most two per file (start and end);
// for piece-aligned files there are zero. The intermediate (fully-contained)
// pieces detect the seeder's eviction-during-read bug, which zeroes whole
// pieces — so even partial coverage catches the failure mode in practice.
//
// Performance: stream the entire file once with a single S3 GET and hash
// each piece as bytes pass through. This is bandwidth-bound rather than
// round-trip-bound, an order of magnitude faster than per-piece Range GETs.
func verifyFileAgainstMetainfo(ctx context.Context, s3Cl *awss3.S3, bucket, key string, mi *metainfo.Info, fileOffset, fileLen int64) error {
	if fileLen == 0 {
		return nil
	}
	pieceLen := mi.PieceLength
	if pieceLen <= 0 {
		return errors.Errorf("verify: invalid piece length %d", pieceLen)
	}
	fileEnd := fileOffset + fileLen

	// First fully-contained piece (rounded up to next piece boundary in the
	// torrent's global offset space) and last fully-contained piece.
	firstFullGlobal := ((fileOffset + pieceLen - 1) / pieceLen) * pieceLen
	lastFullGlobal := (fileEnd / pieceLen) * pieceLen
	if firstFullGlobal >= lastFullGlobal {
		log.WithFields(log.Fields{
			"key":              key,
			"verified_pieces":  0,
			"skipped_boundary": 2,
		}).Info("integrity verification: file too small to contain a full piece")
		return nil
	}

	startInFile := firstFullGlobal - fileOffset
	endInFile := lastFullGlobal - fileOffset

	out, err := s3Cl.GetObjectWithContext(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", startInFile, endInFile-1)),
	})
	if err != nil {
		return errors.Wrapf(err, "verify: GET key=%s range=%d-%d", key, startInFile, endInFile-1)
	}
	defer out.Body.Close()

	skipped := 0
	if firstFullGlobal > fileOffset {
		skipped++
	}
	if lastFullGlobal < fileEnd {
		skipped++
	}

	verified := 0
	buf := make([]byte, pieceLen)
	for pieceGlobal := firstFullGlobal; pieceGlobal < lastFullGlobal; pieceGlobal += pieceLen {
		idx := int(pieceGlobal / pieceLen)
		piece := mi.Piece(idx)
		if piece.Length() != pieceLen {
			return errors.Errorf("verify: piece %d expected length %d, got %d", idx, pieceLen, piece.Length())
		}
		if _, err := io.ReadFull(out.Body, buf); err != nil {
			return errors.Wrapf(err, "verify: read piece %d from stream", idx)
		}
		h := sha1.New()
		_, _ = h.Write(buf)
		got := h.Sum(nil)
		wantOpt := piece.V1Hash()
		if !wantOpt.Ok {
			return errors.Errorf("verify: piece %d has no v1 hash in metainfo", idx)
		}
		want := wantOpt.Value
		if !bytes.Equal(got, want[:]) {
			return errors.Errorf("verify: piece %d sha1 mismatch (got=%x want=%x)", idx, got, want[:])
		}
		verified++
	}

	log.WithFields(log.Fields{
		"key":              key,
		"verified_pieces":  verified,
		"skipped_boundary": skipped,
		"total_pieces":     mi.NumPieces(),
	}).Info("integrity verification passed")
	return nil
}
