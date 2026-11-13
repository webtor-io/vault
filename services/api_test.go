package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Package test values; the uid fallback tests use them too.
var (
	uid0 = strings.Repeat("0", 40)
	uid1 = strings.Repeat("1", 40)
	uid2 = strings.Repeat("2", 40)

	privateRefusal = `{"error":"private torrent: only a signed-in user who added it with their own tracker can play it","code":"private_torrent","reason":"no_record"}`
)

func TestClaimsUid(t *testing.T) {
	b, _ := json.Marshal(&Claims{Role: "vault", Uid: uid1})
	if !strings.Contains(string(b), `"uid":"`+uid1+`"`) {
		t.Fatalf("uid claim missing: %s", b)
	}
	b, _ = json.Marshal(&Claims{Role: "vault"})
	if strings.Contains(string(b), `"uid"`) {
		t.Fatalf("a token without uid must not carry the key: %s", b)
	}
}

func TestDoRequestPrivateTorrent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		private bool
	}{
		{"private_torrent", 403, privateRefusal, true},
		{"ban without code", 403, `{"error":"forbidden"}`, false},
		{"empty 403", 403, ``, false},
		{"private_unavailable", 403, `{"error":"x","code":"private_unavailable"}`, false},
		{"store_outdated", 503, `{"error":"x","code":"store_outdated"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			a := &Api{url: srv.URL, cl: srv.Client(), prepareRequest: func(r *http.Request, _ *Claims) (*http.Request, error) { return r, nil }}
			_, err := a.ListResourceContent(context.Background(), &Claims{Role: "vault", Uid: uid0}, "ih", &ListResourceContentArgs{})
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrPrivateTorrent); got != tc.private {
				t.Fatalf("errors.Is(err, ErrPrivateTorrent) = %v, want %v (%v)", got, tc.private, err)
			}
			if tc.private && !strings.Contains(err.Error(), "reason=no_record") {
				t.Fatalf("reason lost: %v", err)
			}
		})
	}
}

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
