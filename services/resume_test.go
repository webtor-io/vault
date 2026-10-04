package services

import "testing"

// Parts upload in parallel, so an interrupted upload can hold parts 1-10 and
// 12 without 11. Counting all parts resumed at part 12's offset as part 13:
// part 11's bytes were lost and part 12's stored twice, silently.
func TestContiguousParts(t *testing.T) {
	const p = 10
	for _, c := range []struct {
		name  string
		sizes map[int64]int64
		total int64
		want  int64
	}{
		{"none", map[int64]int64{}, 55, 0},
		{"gap after two", map[int64]int64{1: p, 2: p, 4: p}, 55, 2},
		{"short middle part breaks the run", map[int64]int64{1: p, 2: 3, 3: p}, 55, 1},
		{"complete with a larger final part", map[int64]int64{1: p, 2: p, 3: p, 4: 25}, 55, 4},
		{"single small part", map[int64]int64{1: 7}, 7, 1},
	} {
		if got := contiguousParts(c.sizes, p, c.total); got != c.want {
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
		if got := resumeOffset(c.stored, c.fileOff, c.pieceLen, c.partLen); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
