package services

import "testing"

// Parts upload in parallel, so an interrupted upload can hold parts 1-10 and
// 12 without 11. Counting all parts resumed at part 12's offset as part 13:
// part 11's bytes were lost and part 12's stored twice, silently. The result is
// bytes: a complete upload ends with the merged last part, which counted as a
// whole part would resume short of the end and store its tail twice.
func TestContiguousStored(t *testing.T) {
	const p = 10
	for _, c := range []struct {
		name  string
		sizes map[int64]int64
		total int64
		want  int64
	}{
		{"none", map[int64]int64{}, 55, 0},
		{"gap after two", map[int64]int64{1: p, 2: p, 4: p}, 55, 20},
		{"short middle part breaks the run", map[int64]int64{1: p, 2: 3, 3: p}, 55, 10},
		{"complete with a merged final part", map[int64]int64{1: p, 2: p, 3: p, 4: p, 5: 15}, 55, 55},
		{"final part merges the remainder", map[int64]int64{1: p, 2: 17}, 27, 27},
		{"single small part", map[int64]int64{1: 7}, 7, 7},
	} {
		if got := contiguousStored(c.sizes, p, c.total); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// Resume restarts at the start of the part holding the first byte of the
// piece the stored prefix cut, so that piece is streamed whole again.
func TestResumeOffset(t *testing.T) {
	for _, c := range []struct {
		name                               string
		stored, fileOff, pieceLen, partLen int64
		want                               int64
	}{
		{"nothing stored", 0, 0, 4, 10, 0},
		{"piece-aligned prefix resumes in place", 20, 0, 4, 10, 20},
		{"cut piece starts in the previous part", 20, 2, 4, 10, 10},
		{"cut piece starts in the last stored part", 20, 2, 3, 10, 10},
		{"piece larger than a part reaches back two parts", 30, 0, 25, 10, 20},
		{"cut piece starts before the file", 10, 5, 100, 10, 0},
	} {
		if got := resumeOffset(c.stored, c.fileOff, c.pieceLen, c.partLen, 1000); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// The upload loop merges the remainder into the last part when less than two
// parts are left, so parts are not all partSize: a file of 12.6 parts' worth
// of 1-sized parts... concretely 27 bytes in 10-byte parts is [0,10) [10,27).
// A rewind past the last part's start must land on it, or bytes go missing.
func TestPartStart(t *testing.T) {
	for _, c := range []struct {
		name             string
		off, part, total int64
		want             int64
	}{
		{"inside a regular part", 13, 10, 55, 10},
		{"inside the merged last part", 47, 10, 55, 40},
		{"merged last part starts early", 23, 10, 27, 10},
		{"file smaller than two parts is one part", 15, 10, 19, 0},
		{"exactly two parts", 15, 10, 20, 10},
	} {
		if got := partStart(c.off, c.part, c.total); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	if got := resumeOffset(20, 0, 7, 10, 27); got != 10 {
		t.Errorf("resume into the merged last part: got %d, want 10", got)
	}
}
