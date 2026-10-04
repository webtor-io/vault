package services

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/pkg/errors"
	ra "github.com/webtor-io/rest-api/services"
)

// torrentSpan is one v1 file of the torrent at its global offset.
type torrentSpan struct {
	off, length int64
	pad         bool         // BEP 47 padding: zeros, never listed or stored
	item        *ra.ListItem // nil when the listing has no such file
}

func isPadFile(f metainfo.FileInfo) bool {
	return strings.Contains(f.Attr, "p") || (len(f.Path) > 0 && f.Path[0] == ".pad")
}

// torrentSpans lays the listing's files over the torrent's v1 file sequence,
// matching paths the way fileOffsetInTorrent does, in one pass.
func torrentSpans(mi *metainfo.Info, items []ra.ListItem) []torrentSpan {
	byKey := make(map[string]*ra.ListItem, len(items))
	for i := range items {
		it := &items[i]
		trimmed := strings.TrimPrefix(it.PathStr, "/")
		byKey[trimmed+"\x00"+itoa(it.Size)] = it
		byKey[strings.TrimPrefix(trimmed, mi.Name+"/")+"\x00"+itoa(it.Size)] = it
	}
	if len(mi.Files) == 0 {
		return []torrentSpan{{off: 0, length: mi.Length, item: byKey[mi.Name+"\x00"+itoa(mi.Length)]}}
	}
	spans := make([]torrentSpan, 0, len(mi.Files))
	var off int64
	for _, f := range mi.Files {
		sp := torrentSpan{off: off, length: f.Length, pad: isPadFile(f)}
		if !sp.pad {
			sp.item = byKey[strings.Join(f.Path, "/")+"\x00"+itoa(f.Length)]
		}
		spans = append(spans, sp)
		off += f.Length
	}
	return spans
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// newSourceFetcher reads torrent-global byte ranges from the content source:
// each listed file through its export URL (thp), BEP 47 padding as zeros. A
// range touching a file the listing does not have is errSourceUnavailable.
func newSourceFetcher(api *Api, cla *Claims, id string, mi *metainfo.Info, items []ra.ListItem) sourceFetcher {
	spans := torrentSpans(mi, items)
	var mu sync.Mutex
	urls := map[string]string{}
	exportURL := func(ctx context.Context, it *ra.ListItem) (string, error) {
		mu.Lock()
		u, ok := urls[it.ID]
		mu.Unlock()
		if ok {
			return u, nil
		}
		ei, err := api.ExportResourceContent(ctx, cla, id, it.ID)
		if err != nil {
			return "", errors.Wrap(err, "export for verification source")
		}
		u = ei.ExportItems["download"].URL
		mu.Lock()
		urls[it.ID] = u
		mu.Unlock()
		return u, nil
	}
	return func(ctx context.Context, start, end int64) ([]byte, error) {
		out := make([]byte, 0, end-start)
		for _, sp := range spans {
			from, to := max(start, sp.off), min(end, sp.off+sp.length)
			if from >= to {
				continue
			}
			if sp.pad {
				out = append(out, make([]byte, to-from)...)
				continue
			}
			if sp.item == nil {
				return nil, errSourceUnavailable
			}
			u, err := exportURL(ctx, sp.item)
			if err != nil {
				return nil, err
			}
			data, err := readRange(ctx, api, u, from-sp.off, to-sp.off)
			if err != nil {
				return nil, err
			}
			out = append(out, data...)
		}
		if int64(len(out)) != end-start {
			return nil, errSourceUnavailable // range runs past the torrent
		}
		return out, nil
	}
}

// readRange downloads [start, end) of one file, retrying a short read.
func readRange(ctx context.Context, api *Api, u string, start, end int64) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		r, err := api.DownloadWithRange(ctx, u, int(start), int(end-1))
		if err == nil {
			buf := make([]byte, end-start)
			_, err = io.ReadFull(r, buf)
			_ = r.Close()
			if err == nil {
				return buf, nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return nil, errors.Wrapf(lastErr, "read source range [%d, %d)", start, end)
}
