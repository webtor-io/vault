package services

// The upload loop cuts a file into partSize parts, except that once less than
// two parts' worth remains the rest goes into one last part. So the last part
// starts at lastPartStart and runs to the end, between partSize and 2*partSize
// long (or the whole file, when it is smaller than two parts).
func lastPartStart(total, partSize int64) int64 {
	if total < 2*partSize {
		return 0
	}
	return ((total-2*partSize)/partSize + 1) * partSize
}

// partStart is the start of the part holding file offset off.
func partStart(off, partSize, total int64) int64 {
	return min(off/partSize*partSize, lastPartStart(total, partSize))
}

// contiguousParts counts the leading multipart parts 1..k that are present with
// their expected size (partSize, or the remainder for the last part). Parts
// upload in parallel, so an interrupted upload can hold parts after a gap;
// counting every part would resume past the gap and drop its bytes.
func contiguousParts(sizes map[int64]int64, partSize, total int64) int64 {
	var k int64
	for {
		sz, ok := sizes[k+1]
		if !ok || (sz != partSize && k*partSize+sz != total) {
			return k
		}
		k++
		if k*partSize >= total || k*partSize+sz >= total {
			return k
		}
	}
}

// resumeOffset is the file offset an upload restarts from after `stored` bytes
// of contiguous parts: the start of the part holding the first byte of the
// piece that the stored prefix cuts. That piece is then streamed whole again
// and verified; the piece before it, verified when it was first streamed, is
// the only one whose prefix is seeded from the source.
func resumeOffset(stored, fileOff, pieceLen, partSize, total int64) int64 {
	if stored <= 0 {
		return 0
	}
	pieceStart := (fileOff+stored)/pieceLen*pieceLen - fileOff
	if pieceStart < 0 {
		pieceStart = 0
	}
	return partStart(pieceStart, partSize, total)
}
