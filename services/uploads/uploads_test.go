package uploads

import (
	"strings"
	"testing"
)

func TestPredicates(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"9c5db5752b7c0123456789abcdef0123456789ab", true},
		{"9c5db5752b7c1123456789abcdef0123456789ab", true},
		{"9c5db5752b7cf123456789abcdef0123456789ab", true},
		{strings.ToUpper("9c5db5752b7c0123456789abcdef0123456789ab"), false},
		{strings.ToUpper("9c5db5752b7c1123456789abcdef0123456789ab"), false},
		{strings.ToUpper("9c5db5752b7cf123456789abcdef0123456789ab"), false},
		{"9c5db5752b7cA123456789abcdef0123456789ab", false},
		{"9c5db5752b7c/../x", false},
		{"9c5db5752b7c0123456789abcdef0123456789a", false},
		{"0123456789abcdef0123456789abcdef01234567", false},
	} {
		if got := HomeHosted(tc.id); got != tc.want {
			t.Errorf("HomeHosted(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
