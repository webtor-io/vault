// Package uploads classifies resource ids. An upload is a resource whose
// bytes live only on the seeder of its home node: it has no .torrent.
package uploads

import "strings"

// HomeHosted reports whether id is the id of an upload: 40 lowercase hex
// characters starting with the upload prefix. It takes the id as received,
// without case folding.
func HomeHosted(id string) bool {
	if len(id) != 40 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return strings.HasPrefix(id, "9c5db5752b7c")
}
