package services

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"hash"
	"io"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/pkg/errors"
)

// prevFileInfo tells the verifier where to find a previously-stored file in
// torrent global offsets — needed to bootstrap a left-boundary piece by
// pulling its prefix bytes from S3.
type prevFileInfo struct {
	torrentOff int64
	length     int64
	hash       string
}

// byteFetcher reads [start, end) from a stored object identified by hash.
// In production it issues an S3 GetObject with a Range header; tests
// substitute an in-memory implementation.
type byteFetcher func(ctx context.Context, hash string, start, end int64) ([]byte, error)

// newS3ByteFetcher builds a byteFetcher backed by an S3 GET with Range.
func newS3ByteFetcher(s3Cl *awss3.S3, bucket string) byteFetcher {
	return func(ctx context.Context, hash string, start, end int64) ([]byte, error) {
		if end <= start {
			return nil, nil
		}
		out, err := s3Cl.GetObjectWithContext(ctx, &awss3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(hash),
			Range:  aws.String(fmt.Sprintf("bytes=%d-%d", start, end-1)),
		})
		if err != nil {
			return nil, err
		}
		defer out.Body.Close()
		buf := make([]byte, end-start)
		if _, err := io.ReadFull(out.Body, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
}

// pieceVerifier streams uploaded bytes through SHA-1 piece hashers and
// compares each completed piece against the torrent's metainfo.
//
// Pieces fully contained within the file are verified inline as bytes flow
// through Feed. A left-boundary piece (one that starts before this file's
// first byte) is bootstrapped from previous files' S3 objects so it can be
// verified inline too. A right-boundary piece (extending past the file's
// last byte) is intentionally skipped here — the NEXT file in torrent order
// will pick up the same piece as ITS left-boundary and verify it then. By
// induction every inter-file boundary is checked exactly once.
type pieceVerifier struct {
	mi       *metainfo.Info
	fileOff  int64
	fileLen  int64
	pieceLen int64
	prev     []prevFileInfo
	fetch    byteFetcher

	bytesSeen int64     // bytes from this file consumed via Feed
	curPiece  int       // index of the current piece
	curHash   hash.Hash // nil when current piece is being skipped
}

// newPieceVerifier constructs a verifier for one file. fetch is invoked for
// left-boundary prefix reads and for resume self-reads. prev must list every
// already-stored file in the resource in torrent order (offsets and lengths
// match the torrent's piece sequence).
func newPieceVerifier(mi *metainfo.Info, fileOff, fileLen int64, prev []prevFileInfo, fetch byteFetcher) *pieceVerifier {
	return &pieceVerifier{
		mi:       mi,
		fileOff:  fileOff,
		fileLen:  fileLen,
		pieceLen: mi.PieceLength,
		prev:     prev,
		fetch:    fetch,
	}
}

// Bootstrap initializes hashing for the first piece that the upload stream
// will produce. When the upload is resuming (resumeFrom > 0), the piece
// overlapping that offset is left unhashed: the bytes we'd need to seed
// the hasher live in an in-flight multipart upload, which S3 GetObject
// can't read back until CompleteMultipartUpload finalises the object.
// All pieces strictly after the resume point are hashed normally as they
// flow through Feed.
func (v *pieceVerifier) Bootstrap(ctx context.Context, resumeFrom int64) error {
	if v.fileLen == 0 {
		return nil
	}
	v.bytesSeen = resumeFrom
	v.curPiece = int((v.fileOff + resumeFrom) / v.pieceLen)
	if resumeFrom > 0 {
		v.curHash = nil
		return nil
	}
	return v.beginPiece(ctx)
}

// Feed consumes len(p) bytes that are about to be uploaded to S3. It returns
// an error on the first piece SHA-1 mismatch; the caller should abort the
// multipart upload when that happens.
func (v *pieceVerifier) Feed(ctx context.Context, p []byte) error {
	pos := 0
	for pos < len(p) {
		pieceLength := v.mi.Piece(v.curPiece).Length()
		pieceGlobalStart := int64(v.curPiece) * v.pieceLen
		pieceGlobalEnd := pieceGlobalStart + pieceLength
		fileEnd := v.fileOff + v.fileLen

		globalNow := v.fileOff + v.bytesSeen
		pieceFileEnd := pieceGlobalEnd
		if pieceFileEnd > fileEnd {
			pieceFileEnd = fileEnd
		}
		remaining := pieceFileEnd - globalNow
		n := int64(len(p) - pos)
		if n > remaining {
			n = remaining
		}

		if v.curHash != nil {
			v.curHash.Write(p[pos : pos+int(n)])
		}
		v.bytesSeen += n
		pos += int(n)

		if globalNow+n == pieceFileEnd {
			if v.curHash != nil {
				if err := v.finalizeCurrentPiece(); err != nil {
					return err
				}
			}
			v.curPiece++
			v.curHash = nil
			if v.bytesSeen >= v.fileLen {
				return nil
			}
			if err := v.beginPiece(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// beginPiece sets up hashing state for the current piece. It either:
//   - skips the piece (curHash remains nil) when the piece extends past
//     the file's last byte (right boundary or piece-fully-spans-file);
//   - starts a fresh hasher and pre-loads any left-boundary prefix bytes
//     pulled from previous files' S3 objects.
func (v *pieceVerifier) beginPiece(ctx context.Context) error {
	pieceLength := v.mi.Piece(v.curPiece).Length()
	pieceGlobalStart := int64(v.curPiece) * v.pieceLen
	pieceGlobalEnd := pieceGlobalStart + pieceLength
	fileEnd := v.fileOff + v.fileLen

	if pieceGlobalEnd > fileEnd {
		v.curHash = nil
		return nil
	}

	v.curHash = sha1.New()
	if pieceGlobalStart < v.fileOff {
		if err := v.fetchPrefixInto(ctx, v.curHash, pieceGlobalStart, v.fileOff); err != nil {
			return errors.Wrapf(err, "fetch prefix for piece %d [%d, %d)", v.curPiece, pieceGlobalStart, v.fileOff)
		}
	}
	return nil
}

// fetchPrefixInto walks prev files covering [start, end) in torrent global
// offsets and copies their bytes into w. The prev list must be exhaustive
// for the requested range; a gap is reported as an error rather than
// silently skipped, since that would let bad bytes pass verification.
func (v *pieceVerifier) fetchPrefixInto(ctx context.Context, w io.Writer, start, end int64) error {
	pos := start
	for pos < end {
		pf := v.prevFileAt(pos)
		if pf == nil {
			return errors.Errorf("no prev file covers torrent offset %d", pos)
		}
		startInF := pos - pf.torrentOff
		endInF := pf.length
		if pf.torrentOff+endInF > end {
			endInF = end - pf.torrentOff
		}
		data, err := v.fetch(ctx, pf.hash, startInF, endInF)
		if err != nil {
			return errors.Wrapf(err, "fetch %s [%d, %d)", pf.hash, startInF, endInF)
		}
		if int64(len(data)) != endInF-startInF {
			return errors.Errorf("fetch %s [%d, %d) returned %d bytes", pf.hash, startInF, endInF, len(data))
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		pos = pf.torrentOff + endInF
	}
	return nil
}

func (v *pieceVerifier) prevFileAt(off int64) *prevFileInfo {
	for i := range v.prev {
		f := &v.prev[i]
		if off >= f.torrentOff && off < f.torrentOff+f.length {
			return f
		}
	}
	return nil
}

func (v *pieceVerifier) finalizeCurrentPiece() error {
	got := v.curHash.Sum(nil)
	wantOpt := v.mi.Piece(v.curPiece).V1Hash()
	if !wantOpt.Ok {
		return errors.Errorf("piece %d has no v1 hash in metainfo", v.curPiece)
	}
	want := wantOpt.Value
	if !bytes.Equal(got, want[:]) {
		return errors.Errorf("piece %d sha1 mismatch (got=%x want=%x)", v.curPiece, got, want[:])
	}
	return nil
}
