package services

import (
	"context"
	"crypto/sha1"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

// testFile pairs a path component with its bytes — used by buildTestTorrent
// to assemble a single- or multi-file test torrent.
type testFile struct {
	name  string
	bytes []byte
}

// buildTestTorrent assembles a metainfo.Info with correct piece SHA-1s for
// the concatenated file contents and returns the per-file torrent global
// offsets. For len(files)==1 it builds a single-file torrent (Info.Length);
// otherwise a multi-file torrent (Info.Files) under Name "t".
func buildTestTorrent(t *testing.T, pieceLen int64, files []testFile) (*metainfo.Info, []int64, []byte) {
	t.Helper()
	var concat []byte
	offsets := make([]int64, len(files))
	for i, f := range files {
		offsets[i] = int64(len(concat))
		concat = append(concat, f.bytes...)
	}
	var pieces []byte
	for off := int64(0); off < int64(len(concat)); off += pieceLen {
		end := off + pieceLen
		if end > int64(len(concat)) {
			end = int64(len(concat))
		}
		h := sha1.Sum(concat[off:end])
		pieces = append(pieces, h[:]...)
	}
	info := &metainfo.Info{
		PieceLength: pieceLen,
		Pieces:      pieces,
		Name:        "t",
	}
	if len(files) == 1 {
		info.Length = int64(len(files[0].bytes))
	} else {
		miFiles := make([]metainfo.FileInfo, len(files))
		for i, f := range files {
			miFiles[i] = metainfo.FileInfo{
				Length: int64(len(f.bytes)),
				Path:   []string{f.name},
			}
		}
		info.Files = miFiles
	}
	return info, offsets, concat
}

// fakeFetcher returns a byteFetcher backed by an in-memory map.
func fakeFetcher(store map[string][]byte) byteFetcher {
	return func(_ context.Context, h string, start, end int64) ([]byte, error) {
		b, ok := store[h]
		if !ok {
			return nil, &fakeFetcherErr{h: h}
		}
		if start < 0 || end > int64(len(b)) || end < start {
			return nil, &fakeFetcherErr{h: h}
		}
		out := make([]byte, end-start)
		copy(out, b[start:end])
		return out, nil
	}
}

type fakeFetcherErr struct{ h string }

func (e *fakeFetcherErr) Error() string { return "fake fetcher: missing or bad range for " + e.h }

// fillPattern returns n bytes filled with byte b.
func fillPattern(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestPieceVerifier_SingleFile_PieceAligned_Clean(t *testing.T) {
	const pieceLen = 100
	bytes := append(append(fillPattern('A', pieceLen), fillPattern('B', pieceLen)...), fillPattern('C', pieceLen)...)
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", bytes}})

	v := newPieceVerifier(info, 0, int64(len(bytes)), nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := v.Feed(context.Background(), bytes); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_SingleFile_CorruptMiddlePiece(t *testing.T) {
	const pieceLen = 100
	bytes := append(append(fillPattern('A', pieceLen), fillPattern('B', pieceLen)...), fillPattern('C', pieceLen)...)
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", bytes}})

	corrupt := make([]byte, len(bytes))
	copy(corrupt, bytes)
	corrupt[pieceLen+5] ^= 0xFF // flip a bit inside piece 1

	v := newPieceVerifier(info, 0, int64(len(corrupt)), nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	err := v.Feed(context.Background(), corrupt)
	if err == nil {
		t.Fatal("expected mismatch on piece 1, got nil")
	}
	if !strings.Contains(err.Error(), "piece 1 sha1 mismatch") {
		t.Fatalf("expected piece 1 mismatch, got %v", err)
	}
}

func TestPieceVerifier_SingleFile_TruncatedLastPiece(t *testing.T) {
	const pieceLen = 100
	bytes := append(append(fillPattern('A', pieceLen), fillPattern('B', pieceLen)...), fillPattern('C', 50)...) // 250 total → 3 pieces (100,100,50)
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", bytes}})

	v := newPieceVerifier(info, 0, int64(len(bytes)), nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := v.Feed(context.Background(), bytes); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_MultiFile_LeftBoundary_Clean(t *testing.T) {
	const pieceLen = 100
	file1 := append(fillPattern('A', 50), fillPattern('B', 100)...) // 150 bytes
	file2 := append(fillPattern('C', 50), fillPattern('D', 100)...) // 150 bytes
	info, offsets, _ := buildTestTorrent(t, pieceLen, []testFile{{"a", file1}, {"b", file2}})

	file1Hash := "file1"
	store := map[string][]byte{file1Hash: file1}
	prev := []prevFileInfo{{torrentOff: offsets[0], length: int64(len(file1)), hash: file1Hash}}

	v := newPieceVerifier(info, offsets[1], int64(len(file2)), prev, fakeFetcher(store))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := v.Feed(context.Background(), file2); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_MultiFile_LeftBoundary_PrevCorrupt(t *testing.T) {
	const pieceLen = 100
	file1 := append(fillPattern('A', 50), fillPattern('B', 100)...)
	file2 := append(fillPattern('C', 50), fillPattern('D', 100)...)
	info, offsets, _ := buildTestTorrent(t, pieceLen, []testFile{{"a", file1}, {"b", file2}})

	corruptFile1 := make([]byte, len(file1))
	copy(corruptFile1, file1)
	corruptFile1[120] ^= 0xFF // flip in [100, 150) which feeds the boundary piece

	file1Hash := "file1"
	store := map[string][]byte{file1Hash: corruptFile1}
	prev := []prevFileInfo{{torrentOff: offsets[0], length: int64(len(file1)), hash: file1Hash}}

	v := newPieceVerifier(info, offsets[1], int64(len(file2)), prev, fakeFetcher(store))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	err := v.Feed(context.Background(), file2)
	if err == nil {
		t.Fatal("expected boundary piece mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "piece 1 sha1 mismatch") {
		t.Fatalf("expected piece 1 mismatch (boundary), got %v", err)
	}
}

func TestPieceVerifier_MultiFile_RightBoundarySkipped(t *testing.T) {
	const pieceLen = 100
	file1 := append(fillPattern('A', 50), fillPattern('B', 100)...) // 150 bytes; piece 1 spans [100,200) but file1 ends at 150
	file2 := fillPattern('C', 100)                                  // 100 bytes
	info, offsets, _ := buildTestTorrent(t, pieceLen, []testFile{{"a", file1}, {"b", file2}})

	v := newPieceVerifier(info, offsets[0], int64(len(file1)), nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := v.Feed(context.Background(), file1); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_PieceLargerThanFile_Skipped(t *testing.T) {
	const pieceLen = 100
	tiny := fillPattern('A', 30) // 30 bytes — single piece spans entire file
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", tiny}})

	v := newPieceVerifier(info, 0, int64(len(tiny)), nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := v.Feed(context.Background(), tiny); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_MultiFile_PieceSpansThreeFiles(t *testing.T) {
	const pieceLen = 100
	a := fillPattern('A', 30)
	b := fillPattern('B', 40)
	c := fillPattern('C', 130) // pieces: 0=[0,100) covers a+b+30 of c; 1=[100,200) covers c[30..130]
	info, offsets, _ := buildTestTorrent(t, pieceLen, []testFile{{"a", a}, {"b", b}, {"c", c}})

	aH, bH := "a-h", "b-h"
	store := map[string][]byte{aH: a, bH: b}
	prev := []prevFileInfo{
		{torrentOff: offsets[0], length: int64(len(a)), hash: aH},
		{torrentOff: offsets[1], length: int64(len(b)), hash: bH},
	}

	v := newPieceVerifier(info, offsets[2], int64(len(c)), prev, fakeFetcher(store))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := v.Feed(context.Background(), c); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_Resume_SkipsResumePiece_Clean(t *testing.T) {
	const pieceLen = 100
	bytes := append(append(fillPattern('A', pieceLen), fillPattern('B', pieceLen)...), fillPattern('C', pieceLen)...)
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", bytes}})

	v := newPieceVerifier(info, 0, int64(len(bytes)), nil, fakeFetcher(nil))
	const resumeFrom = int64(150) // mid piece 1 — verifier must skip piece 1

	if err := v.Bootstrap(context.Background(), resumeFrom); err != nil {
		t.Fatalf("Bootstrap resume: %v", err)
	}
	// Feed remaining bytes from the resume offset onwards. Piece 1 is
	// skipped (we cannot read in-flight multipart bytes back). Piece 2
	// is hashed and verified normally.
	if err := v.Feed(context.Background(), bytes[resumeFrom:]); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_Resume_DetectsCorruptionInLaterPiece(t *testing.T) {
	const pieceLen = 100
	bytes := append(append(fillPattern('A', pieceLen), fillPattern('B', pieceLen)...), fillPattern('C', pieceLen)...)
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", bytes}})

	corrupt := make([]byte, len(bytes))
	copy(corrupt, bytes)
	corrupt[210] ^= 0xFF // flip inside piece 2 (post-resume, must be caught inline)

	v := newPieceVerifier(info, 0, int64(len(corrupt)), nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), int64(pieceLen)); err != nil {
		t.Fatalf("Bootstrap resume: %v", err)
	}
	err := v.Feed(context.Background(), corrupt[pieceLen:])
	if err == nil {
		t.Fatal("expected piece 2 mismatch on resumed feed, got nil")
	}
	if !strings.Contains(err.Error(), "piece 2 sha1 mismatch") {
		t.Fatalf("expected piece 2 mismatch, got %v", err)
	}
}

func TestPieceVerifier_Resume_PieceAligned(t *testing.T) {
	const pieceLen = 100
	bytes := append(append(fillPattern('A', pieceLen), fillPattern('B', pieceLen)...), fillPattern('C', pieceLen)...)
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", bytes}})

	v := newPieceVerifier(info, 0, int64(len(bytes)), nil, fakeFetcher(nil))
	// Resume exactly on a piece boundary — pieces 1 and 2 are hashed
	// normally because no piece is mid-flight.
	if err := v.Bootstrap(context.Background(), int64(pieceLen)); err != nil {
		t.Fatalf("Bootstrap resume: %v", err)
	}
	if err := v.Feed(context.Background(), bytes[pieceLen:]); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}

func TestPieceVerifier_FedInChunks(t *testing.T) {
	const pieceLen = 100
	bytes := append(append(fillPattern('A', pieceLen), fillPattern('B', pieceLen)...), fillPattern('C', pieceLen)...)
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"f", bytes}})

	v := newPieceVerifier(info, 0, int64(len(bytes)), nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// Feed in 17-byte chunks to exercise piece-boundary handling across calls.
	for off := 0; off < len(bytes); off += 17 {
		end := off + 17
		if end > len(bytes) {
			end = len(bytes)
		}
		if err := v.Feed(context.Background(), bytes[off:end]); err != nil {
			t.Fatalf("Feed at %d: %v", off, err)
		}
	}
}

func TestPieceVerifier_ZeroByteFile(t *testing.T) {
	const pieceLen = 100
	info, _, _ := buildTestTorrent(t, pieceLen, []testFile{{"a", fillPattern('A', 100)}, {"b", nil}})
	// Verify file b (zero-byte) — Bootstrap and Feed should both noop.
	v := newPieceVerifier(info, 100, 0, nil, fakeFetcher(nil))
	if err := v.Bootstrap(context.Background(), 0); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := v.Feed(context.Background(), nil); err != nil {
		t.Fatalf("Feed: %v", err)
	}
}
