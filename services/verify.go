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
// "/"-joined path components. Single-file torrents return 0 when the
// trimmed path matches Name.
func fileOffsetInTorrent(mi *metainfo.Info, pathStr string, length int64) int64 {
	trimmed := strings.TrimPrefix(pathStr, "/")
	if len(mi.Files) == 0 {
		if trimmed == mi.Name {
			return 0
		}
		return -1
	}
	var off int64
	for _, f := range mi.Files {
		joined := strings.Join(f.Path, "/")
		if joined == trimmed && f.Length == length {
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
func verifyFileAgainstMetainfo(ctx context.Context, s3Cl *awss3.S3, bucket, key string, mi *metainfo.Info, fileOffset, fileLen int64) error {
	fileEnd := fileOffset + fileLen
	verified := 0
	skipped := 0

	for idx := 0; idx < mi.NumPieces(); idx++ {
		piece := mi.Piece(idx)
		pStart := piece.Offset()
		pEnd := pStart + piece.Length()

		if pEnd <= fileOffset || pStart >= fileEnd {
			continue
		}
		if pStart < fileOffset || pEnd > fileEnd {
			skipped++
			continue
		}

		rangeStart := pStart - fileOffset
		rangeEnd := pEnd - fileOffset - 1
		out, err := s3Cl.GetObjectWithContext(ctx, &awss3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Range:  aws.String(fmt.Sprintf("bytes=%d-%d", rangeStart, rangeEnd)),
		})
		if err != nil {
			return errors.Wrapf(err, "verify: GET piece %d range bytes=%d-%d", idx, rangeStart, rangeEnd)
		}
		h := sha1.New()
		n, copyErr := io.Copy(h, out.Body)
		_ = out.Body.Close()
		if copyErr != nil {
			return errors.Wrapf(copyErr, "verify: read piece %d", idx)
		}
		if n != piece.Length() {
			return errors.Errorf("verify: piece %d short read got=%d want=%d", idx, n, piece.Length())
		}
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
