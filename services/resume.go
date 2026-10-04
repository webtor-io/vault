package services

// contiguousParts counts the leading multipart parts 1..k that are present with
// their expected size (partSize, or the remainder for the final part). Parts
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
		if k*partSize >= total {
			return k
		}
	}
}

// resumeOffset is the file offset an upload restarts from after `stored` bytes
// of contiguous parts: the start of the part holding the first byte of the
// piece that the stored prefix cuts. That piece is then streamed whole again
// and verified; the piece before it, verified when it was first streamed, is
// the only one whose prefix is seeded from the source.
func resumeOffset(stored, fileOff, pieceLen, partSize int64) int64 {
	if stored <= 0 {
		return 0
	}
	pieceStart := (fileOff+stored)/pieceLen*pieceLen - fileOff
	if pieceStart < 0 {
		pieceStart = 0
	}
	return pieceStart / partSize * partSize
}
