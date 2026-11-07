package services

import (
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestFileOffsetInTorrentPaths(t *testing.T) {
	single := &metainfo.Info{Name: "a.mkv", Length: 10}
	multi := &metainfo.Info{Name: "dir", Files: []metainfo.FileInfo{{Path: []string{"x"}, Length: 3}, {Path: []string{"y"}, Length: 4}}}
	for _, c := range []struct {
		mi   *metainfo.Info
		path string
		want int64
	}{
		{single, "/a.mkv", 0},
		{single, "/a.mkv/a.mkv", 0}, // v2/hybrid listing
		{single, "/b.mkv", -1},
		{single, "/a.mkv/b.mkv", -1},
		{multi, "/dir/y", 3},
		{multi, "/y", 3},
		{multi, "/dir/z", -1},
	} {
		size := int64(10)
		if c.mi == multi {
			size = 4
		}
		if got := fileOffsetInTorrent(c.mi, c.path, size); got != c.want {
			t.Errorf("%q: got %d, want %d", c.path, got, c.want)
		}
	}
}
