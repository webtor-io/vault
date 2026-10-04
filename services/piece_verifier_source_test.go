package services

import (
	"context"
	"crypto/sha1"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

// sliceSource serves torrent-global ranges from the concatenated torrent bytes.
func sliceSource(concat []byte) sourceFetcher {
	return func(_ context.Context, start, end int64) ([]byte, error) {
		out := make([]byte, end-start)
		copy(out, concat[start:end])
		return out, nil
	}
}

// buildHybridTorrent lays files out the way BEP 52 hybrids do: every file but
// the last is followed by a BEP 47 padding file up to the next piece boundary.
// Returns the info, each real file's torrent offset and the padded v1 bytes.
func buildHybridTorrent(t *testing.T, pieceLen int64, files []testFile) (*metainfo.Info, []int64, []byte) {
	t.Helper()
	var concat []byte
	var v1 []metainfo.FileInfo
	offsets := make([]int64, len(files))
	tree := metainfo.FileTree{Dir: map[string]metainfo.FileTree{}}
	for i, f := range files {
		offsets[i] = int64(len(concat))
		concat = append(concat, f.bytes...)
		v1 = append(v1, metainfo.FileInfo{Length: int64(len(f.bytes)), Path: []string{f.name}})
		tree.Dir[f.name] = metainfo.FileTree{File: metainfo.FileTreeFile{
			Length: int64(len(f.bytes)), PiecesRoot: string(make([]byte, 32))}}
		if pad := (pieceLen - int64(len(concat))%pieceLen) % pieceLen; pad > 0 && i < len(files)-1 {
			concat = append(concat, make([]byte, pad)...)
			v1 = append(v1, metainfo.FileInfo{Length: pad, Path: []string{".pad", strconv.FormatInt(pad, 10)}, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}})
		}
	}
	var pieces []byte
	for off := int64(0); off < int64(len(concat)); off += pieceLen {
		end := min(off+pieceLen, int64(len(concat)))
		h := sha1.Sum(concat[off:end])
		pieces = append(pieces, h[:]...)
	}
	return &metainfo.Info{PieceLength: pieceLen, Pieces: pieces, Name: "t", Files: v1,
		FileTree: tree, MetaVersion: 2}, offsets, concat
}

func feedAll(t *testing.T, v *pieceVerifier, resumeFrom int64, data []byte) error {
	t.Helper()
	if err := v.Bootstrap(context.Background(), resumeFrom); err != nil {
		return err
	}
	return v.Feed(context.Background(), data[resumeFrom:])
}

// A hybrid torrent's last piece of a padded file is the file tail plus pad
// zeros in v1; piece Length() is the v2 per-file length there. Hashing only the
// tail made every such file fail on every retry (production, 2026-10).
func TestPieceVerifier_Hybrid_PaddedTailVerified(t *testing.T) {
	a, b := fillPattern('A', 130), fillPattern('B', 100)
	info, offsets, concat := buildHybridTorrent(t, 100, []testFile{{"a", a}, {"b", b}})
	if !info.HasV2() || info.Piece(1).Length() == info.Piece(1).V1Length() {
		t.Fatal("fixture is not a hybrid with a padded piece")
	}
	v := newPieceVerifier(info, offsets[0], int64(len(a)), nil, fakeFetcher(nil)).withSource(sliceSource(concat))
	if err := feedAll(t, v, 0, a); err != nil {
		t.Fatalf("clean hybrid file: %v", err)
	}
	bad := append([]byte(nil), a...)
	bad[110] ^= 0xFF
	v = newPieceVerifier(info, offsets[0], int64(len(a)), nil, fakeFetcher(nil)).withSource(sliceSource(concat))
	if err := feedAll(t, v, 0, bad); err == nil || !strings.Contains(err.Error(), "piece 1 sha1 mismatch") {
		t.Fatalf("corrupt padded tail: want piece 1 mismatch, got %v", err)
	}
}

// The right-boundary piece is verified while this file is uploaded, with the
// next file's head from the source — not left for the next file to find later.
func TestPieceVerifier_RightBoundaryVerifiedWithSource(t *testing.T) {
	a := append(fillPattern('A', 50), fillPattern('B', 100)...) // piece 1 = a[100:150] + b[0:50]
	b := fillPattern('C', 100)
	info, offsets, concat := buildTestTorrent(t, 100, []testFile{{"a", a}, {"b", b}})
	v := newPieceVerifier(info, offsets[0], int64(len(a)), nil, fakeFetcher(nil)).withSource(sliceSource(concat))
	if err := feedAll(t, v, 0, a); err != nil {
		t.Fatalf("clean: %v", err)
	}
	bad := append([]byte(nil), a...)
	bad[120] = 0
	v = newPieceVerifier(info, offsets[0], int64(len(a)), nil, fakeFetcher(nil)).withSource(sliceSource(concat))
	err := feedAll(t, v, 0, bad)
	var m *pieceMismatchError
	if !errors.As(err, &m) || m.Piece != 1 || m.Start != 100 || m.End != 200 {
		t.Fatalf("corrupt tail: want piece 1 [100,200) mismatch, got %v", err)
	}
}

// Resuming mid-piece seeds the piece prefix from the source instead of
// skipping the piece, so the resumed piece is verified too.
func TestPieceVerifier_ResumeSeedsPieceFromSource(t *testing.T) {
	data := append(append(fillPattern('A', 100), fillPattern('B', 100)...), fillPattern('C', 100)...)
	info, _, concat := buildTestTorrent(t, 100, []testFile{{"f", data}})
	bad := append([]byte(nil), data...)
	bad[170] ^= 0xFF // after the resume point, inside the resumed piece 1
	v := newPieceVerifier(info, 0, int64(len(data)), nil, fakeFetcher(nil)).withSource(sliceSource(concat))
	if err := feedAll(t, v, 150, bad); err == nil || !strings.Contains(err.Error(), "piece 1 sha1 mismatch") {
		t.Fatalf("want piece 1 mismatch on the resumed piece, got %v", err)
	}
}

// Bytes after the file the source cannot name (e.g. a file missing from the
// listing) leave that one piece unverified rather than failing the upload.
func TestPieceVerifier_RightBoundarySourceUnavailableSkips(t *testing.T) {
	a := append(fillPattern('A', 50), fillPattern('B', 100)...)
	info, offsets, _ := buildTestTorrent(t, 100, []testFile{{"a", a}, {"b", fillPattern('C', 100)}})
	none := func(context.Context, int64, int64) ([]byte, error) { return nil, errSourceUnavailable }
	v := newPieceVerifier(info, offsets[0], int64(len(a)), nil, fakeFetcher(nil)).withSource(none)
	if err := feedAll(t, v, 0, a); err != nil {
		t.Fatalf("want skip, got %v", err)
	}
}
