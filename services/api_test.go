package services

import (
	"context"
	"testing"
)

// TestMakeTorrentHTTPProxyRequestPathPrefix covers the path-prefix stripping
// added to makeTorrentHTTPProxyRequest: when going internal to
// torrent-http-proxy, the external-facing edge prefix baked into export URLs
// (self-hosted's nginx routes to torrent-http-proxy by path, e.g.
// "/torrent-http-proxy/") must be stripped, because torrent-http-proxy's own
// router expects the infohash as the first path segment and has no notion
// of that prefix.
func TestMakeTorrentHTTPProxyRequestPathPrefix(t *testing.T) {
	cases := []struct {
		name       string
		pathPrefix string
		inputURL   string
		wantPath   string
	}{
		{
			name:       "prefix set and URL carries it: stripped",
			pathPrefix: "/torrent-http-proxy/",
			inputURL:   "https://example.com/torrent-http-proxy/abc123/source.torrent",
			wantPath:   "/abc123/source.torrent",
		},
		{
			name:       "prefix set and URL does not carry it: unchanged",
			pathPrefix: "/torrent-http-proxy/",
			inputURL:   "https://example.com/abc123/source.torrent",
			wantPath:   "/abc123/source.torrent",
		},
		{
			name:       "prefix empty: unchanged",
			pathPrefix: "",
			inputURL:   "https://example.com/torrent-http-proxy/abc123/source.torrent",
			wantPath:   "/torrent-http-proxy/abc123/source.torrent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Api{
				useInternalTorrentHTTPProxy: true,
				torrentHTTPProxyHost:        "127.0.0.1",
				torrentHTTPProxyPort:        8090,
				torrentHTTPProxyPathPrefix:  tc.pathPrefix,
			}

			req, err := s.makeTorrentHTTPProxyRequest(context.Background(), tc.inputURL)
			if err != nil {
				t.Fatalf("makeTorrentHTTPProxyRequest returned error: %v", err)
			}

			if got := req.URL.Path; got != tc.wantPath {
				t.Errorf("path = %q, want %q", got, tc.wantPath)
			}

			if got := req.URL.Host; got != "127.0.0.1:8090" {
				t.Errorf("host = %q, want %q (internal proxy rewrite must still happen)", got, "127.0.0.1:8090")
			}
		})
	}
}
