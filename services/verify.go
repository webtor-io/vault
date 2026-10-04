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
)

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
		// V1Length: the hash below is the v1 hash, and the fork's Length()
		// walks the v2 file tree, which panics on a malformed one.
		if piece.V1Length() != pieceLen {
			return errors.Errorf("verify: piece %d expected length %d, got %d", idx, pieceLen, piece.V1Length())
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
			return &pieceMismatchError{Piece: idx, Start: pieceGlobal, End: pieceGlobal + pieceLen, Got: got, Want: want[:]}
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

// matchesTorrentPiece reports whether the stored object holds this torrent's
// bytes, by hashing the first piece that lies wholly inside the file. A file
// with no such piece cannot be matched this way and reports false.
func matchesTorrentPiece(ctx context.Context, s3Cl *awss3.S3, bucket, key string, mi *metainfo.Info, fileOff, fileLen int64) (bool, error) {
	pl := mi.PieceLength
	if fileOff < 0 || pl <= 0 {
		return false, nil
	}
	first := (fileOff + pl - 1) / pl * pl
	if first+pl > fileOff+fileLen {
		return false, nil
	}
	want := mi.Piece(int(first / pl)).V1Hash()
	if !want.Ok {
		return false, nil
	}
	data, err := newS3ByteFetcher(s3Cl, bucket)(ctx, key, first-fileOff, first-fileOff+pl)
	if err != nil {
		return false, err
	}
	got := sha1.Sum(data)
	return bytes.Equal(got[:], want.Value[:]), nil
}

// blameStoredFiles explains a piece mismatch on bytes read from stored S3
// objects. If the whole piece read from the torrent instead hashes right, every
// stored file whose bytes in the piece differ from the torrent's is corrupt
// (stored before boundary pieces were verified). If the torrent's bytes do not
// hash right either, the source is not trustworthy right now: nothing is blamed.
func blameStoredFiles(ctx context.Context, s3Cl *awss3.S3, bucket string, files []prevFileInfo, src sourceFetcher, m *pieceMismatchError) ([]prevFileInfo, error) {
	if src == nil {
		return nil, nil
	}
	fresh, err := src(ctx, m.Start, m.End)
	if errors.Is(err, errSourceUnavailable) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sum := sha1.Sum(fresh); !bytes.Equal(sum[:], m.Want) {
		return nil, nil
	}
	fetch := newS3ByteFetcher(s3Cl, bucket)
	var bad []prevFileInfo
	for _, pf := range files {
		from, to := max(m.Start, pf.torrentOff), min(m.End, pf.torrentOff+pf.length)
		if from >= to {
			continue
		}
		stored, err := fetch(ctx, pf.hash, from-pf.torrentOff, to-pf.torrentOff)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(stored, fresh[from-m.Start:to-m.Start]) {
			bad = append(bad, pf)
		}
	}
	return bad, nil
}
