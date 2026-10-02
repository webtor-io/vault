package services

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// Fake credentials shaped like the real ones: the platform key is a UUID,
// the token a JWT.
const (
	testAPIKey = "0b9f7c1e-1111-4222-8333-444455556666"
	testToken  = "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uSUQiOiJ0ZXN0In0.c2lnbmF0dXJlLXRlc3Q"
)

func exportURLFor(base string) string {
	return base + "/08ada5a7a6183aae1e09d831df6748d566095a10/dir/file.bin?api-key=" + testAPIKey + "&token=" + testToken + "&user-id=42&download=true"
}

func assertNoCredentials(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range []string{testAPIKey, testToken} {
		if strings.Contains(s, secret) {
			t.Errorf("%s keeps a credential: %s", what, s)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "export URL: values gone, names and the rest kept",
			in:   exportURLFor("https://api.example.com"),
			want: "https://api.example.com/08ada5a7a6183aae1e09d831df6748d566095a10/dir/file.bin?api-key=<redacted>&token=<redacted>&user-id=42&download=true",
		},
		{
			name: "credentials first and last",
			in:   "http://h/x?token=" + testToken + "&api-key=" + testAPIKey,
			want: "http://h/x?token=<redacted>&api-key=<redacted>",
		},
		{
			name: "percent-encoded inside another URL",
			in:   "http://h/x?u=http%3A%2F%2Fh%2Fy%3Fapi-key%3D" + testAPIKey + "%26token%3D" + testToken,
			want: "http://h/x?u=http%3A%2F%2Fh%2Fy%3Fapi-key%3D<redacted>%26token%3D<redacted>",
		},
		{
			name: "JWT as a path segment",
			in:   "http://h/" + testToken + "/file.bin",
			want: "http://h/<redacted>/file.bin",
		},
		{
			name: "as *url.Error quotes it",
			in:   `Get "http://h/x?api-key=` + testAPIKey + `&token=` + testToken + `": dial tcp 127.0.0.1:1: connect: connection refused`,
			want: `Get "http://h/x?api-key=<redacted>&token=<redacted>": dial tcp 127.0.0.1:1: connect: connection refused`,
		},
		{
			name: "nothing to hide",
			in:   "http://h/08ada5a7a6183aae1e09d831df6748d566095a10/file.bin?download=true",
			want: "http://h/08ada5a7a6183aae1e09d831df6748d566095a10/file.bin?download=true",
		},
		{name: "empty", in: "", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactURL(tt.in); got != tt.want {
				t.Errorf("redactURL(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
		})
	}
}

// closedAddr is a local address nothing listens on, so a request to it
// fails in http.Client.Do with a *url.Error.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestRedactErrorWrappedURLError(t *testing.T) {
	req, err := http.NewRequest("GET", exportURLFor("http://"+closedAddr(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, doErr := http.DefaultClient.Do(req)
	var ue *url.Error
	if !stderrors.As(doErr, &ue) {
		t.Fatalf("Do() error = %#v, want a *url.Error", doErr)
	}
	// The premise: net/http puts the query, credentials included, into
	// the error text.
	if !strings.Contains(doErr.Error(), testAPIKey) {
		t.Fatalf("premise broken: *url.Error no longer quotes the URL: %v", doErr)
	}

	wrapped := errors.Wrapf(doErr, "failed to open download")
	got := redactError(wrapped)
	assertNoCredentials(t, "redactError", got.Error())
	if !strings.Contains(got.Error(), "api-key=<redacted>&token=<redacted>") ||
		!strings.Contains(got.Error(), "failed to open download") {
		t.Errorf("redactError lost what makes the message debuggable: %v", got)
	}
	if !stderrors.As(got, &ue) {
		t.Errorf("errors.As(*url.Error) fails after redaction: %#v", got)
	}
	if errors.Cause(got) != errors.Cause(wrapped) {
		t.Errorf("errors.Cause = %#v, want %#v", errors.Cause(got), errors.Cause(wrapped))
	}

	// A cancelled request must still read as cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", exportURLFor("http://"+closedAddr(t)), nil)
	_, doErr = http.DefaultClient.Do(req)
	if got := redactError(doErr); !stderrors.Is(got, context.Canceled) {
		t.Errorf("errors.Is(context.Canceled) = false after redaction: %v", got)
	}

	if redactError(nil) != nil {
		t.Error("redactError(nil) != nil")
	}
	plain := errors.New("connection reset")
	if redactError(plain) != plain {
		t.Error("redactError replaced an error with nothing to redact")
	}
}

// captureLog sends the standard logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.StandardLogger().Out
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestDownloadWithRangeKeepsNoCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	a := &Api{cl: srv.Client()}

	t.Run("unexpected status", func(t *testing.T) {
		buf := captureLog(t)
		_, err := a.DownloadWithRange(context.Background(), exportURLFor(srv.URL), 0, 1023)
		if err == nil {
			t.Fatal("DownloadWithRange() error = nil, want the 502")
		}
		assertNoCredentials(t, "error", err.Error())
		if !strings.Contains(buf.String(), "downloading with range") {
			t.Fatalf("premise broken: no \"downloading with range\" line in %q", buf.String())
		}
		assertNoCredentials(t, "log", buf.String())
	})

	t.Run("request fails", func(t *testing.T) {
		buf := captureLog(t)
		_, err := a.DownloadWithRange(context.Background(), exportURLFor("http://"+closedAddr(t)), 0, -1)
		if err == nil {
			t.Fatal("DownloadWithRange() error = nil, want a dial error")
		}
		assertNoCredentials(t, "error", err.Error())
		assertNoCredentials(t, "log", buf.String())
	})

	t.Run("bad URL", func(t *testing.T) {
		buf := captureLog(t)
		_, err := a.DownloadWithRange(context.Background(), "http://h/x%zz?api-key="+testAPIKey+"&token="+testToken, 0, -1)
		if err == nil {
			t.Fatal("DownloadWithRange() error = nil, want a parse error")
		}
		assertNoCredentials(t, "error", err.Error())
		assertNoCredentials(t, "log", buf.String())
	})
}

func TestDownloadWithRangeInternalBadURL(t *testing.T) {
	a := &Api{cl: http.DefaultClient, useInternalTorrentHTTPProxy: true, torrentHTTPProxyHost: "thp", torrentHTTPProxyPort: 80}
	buf := captureLog(t)
	_, err := a.DownloadWithRange(context.Background(), "http://h/x%zz?api-key="+testAPIKey+"&token="+testToken, 0, -1)
	if err == nil {
		t.Fatal("DownloadWithRange() error = nil, want a parse error")
	}
	assertNoCredentials(t, "error", err.Error())
	assertNoCredentials(t, "log", buf.String())
}

func TestFetchTorrentKeepsNoCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found")
	}))
	defer srv.Close()
	a := &Api{cl: srv.Client()}
	const ih = "08ada5a7a6183aae1e09d831df6748d566095a10"

	for _, tt := range []struct {
		name string
		u    string
	}{
		{"unexpected status", exportURLFor(srv.URL)},
		{"request fails", exportURLFor("http://" + closedAddr(t))},
		{"bad URL", "http://h/x?api-key=" + testAPIKey + "&token=" + testToken + "#%zz"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.FetchTorrent(context.Background(), tt.u, ih)
			if err == nil {
				t.Fatal("FetchTorrent() error = nil")
			}
			assertNoCredentials(t, "error", err.Error())
		})
	}
}
