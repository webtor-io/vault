package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/urfave/cli"

	ra "github.com/webtor-io/rest-api/services"

	"github.com/golang-jwt/jwt/v5"
)

const (
	apiKeyFlag                      = "webtor-key"
	apiSecretFlag                   = "webtor-secret"
	apiSecureFlag                   = "webtor-rest-api-secure"
	apiHostFlag                     = "webtor-rest-api-host"
	apiPortFlag                     = "webtor-rest-api-port"
	apiExpireFlag                   = "webtor-rest-api-expire"
	useInternalTorrentHTTPProxyFlag = "use-internal-torrent-http-proxy"
	torrentHTTPProxyHostFlag        = "torrent-http-proxy-host"
	torrentHTTPProxyPortFlag        = "torrent-http-proxy-port"
	torrentHTTPProxyPathPrefixFlag  = "torrent-http-proxy-path-prefix"
)

func RegisterApiFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   apiHostFlag,
			Usage:  "webtor rest-api host",
			EnvVar: "REST_API_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   apiPortFlag,
			Usage:  "webtor rest-api port",
			EnvVar: "REST_API_SERVICE_PORT",
			Value:  80,
		},
		cli.BoolFlag{
			Name:   apiSecureFlag,
			Usage:  "webtor rest-api secure (https)",
			EnvVar: "REST_API_SECURE",
		},
		cli.IntFlag{
			Name:   apiExpireFlag,
			Usage:  "webtor rest-api expire in days",
			EnvVar: "REST_API_EXPIRE",
			Value:  1,
		},
		cli.StringFlag{
			Name:   apiKeyFlag,
			Usage:  "webtor api key",
			Value:  "",
			EnvVar: "WEBTOR_API_KEY",
		},
		cli.StringFlag{
			Name:   apiSecretFlag,
			Usage:  "webtor api secret",
			Value:  "",
			EnvVar: "WEBTOR_API_SECRET",
		},
		cli.BoolFlag{
			Name:   useInternalTorrentHTTPProxyFlag,
			Usage:  "use internal torrent http proxy",
			EnvVar: "USE_INTERNAL_TORRENT_HTTP_PROXY",
		},
		cli.StringFlag{
			Name:   torrentHTTPProxyHostFlag,
			Usage:  "torrent http proxy host",
			EnvVar: "TORRENT_HTTP_PROXY_SERVICE_HOST",
		},
		cli.IntFlag{
			Name:   torrentHTTPProxyPortFlag,
			Usage:  "torrent http proxy port",
			EnvVar: "TORRENT_HTTP_PROXY_SERVICE_PORT",
			Value:  80,
		},
		cli.StringFlag{
			Name:   torrentHTTPProxyPathPrefixFlag,
			Usage:  "path prefix to strip when talking to torrent-http-proxy directly (its own router has no notion of the prefix an edge nginx uses to route to it by path instead of by subdomain)",
			EnvVar: "TORRENT_HTTP_PROXY_PATH_PREFIX",
		},
	)
}

type Claims struct {
	jwt.RegisteredClaims
	Rate          string `json:"rate,omitempty"`
	Role          string `json:"role,omitempty"`
	SessionID     string `json:"sessionID"`
	Agent         string `json:"agent"`
	RemoteAddress string `json:"remoteAddress"`
}

type Api struct {
	url                         string
	prepareRequest              func(r *http.Request, c *Claims) (*http.Request, error)
	cl                          *http.Client
	expire                      int
	useInternalTorrentHTTPProxy bool
	torrentHTTPProxyHost        string
	torrentHTTPProxyPort        int
	torrentHTTPProxyPathPrefix  string
}

type ListResourceContentOutputType string

const (
	OutputList ListResourceContentOutputType = "list"
)

type ListResourceContentArgs struct {
	Limit  uint
	Offset uint
	Path   string
	Output ListResourceContentOutputType
}

func (s *ListResourceContentArgs) ToQuery() url.Values {
	q := url.Values{}
	limit := uint(10)
	offset := s.Offset
	path := "/"
	output := OutputList
	if s.Limit > 0 {
		limit = s.Limit
	}
	if s.Path != "" {
		path = s.Path
	}
	if s.Output != "" {
		output = s.Output
	}
	q.Set("limit", strconv.Itoa(int(limit)))
	q.Set("offset", strconv.Itoa(int(offset)))
	q.Set("path", path)
	q.Set("output", string(output))
	return q
}

func NewApi(c *cli.Context, cl *http.Client) *Api {
	host := c.String(apiHostFlag)
	port := c.Int(apiPortFlag)
	secure := c.Bool(apiSecureFlag)
	secret := c.String(apiSecretFlag)
	expire := c.Int(apiExpireFlag)
	key := c.String(apiKeyFlag)
	protocol := "http"
	if secure {
		protocol = "https"
	}
	u := fmt.Sprintf("%v://%v:%v", protocol, host, port)
	prepareRequest := func(r *http.Request, cl *Claims) (*http.Request, error) {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, cl)
		tokenString, err := token.SignedString([]byte(secret))
		if err != nil {
			return nil, err
		}
		r.Header.Set("X-Token", tokenString)
		r.Header.Set("X-Api-Key", key)
		return r, nil
	}
	log.Infof("api endpoint %v", u)
	return &Api{
		url:                         u,
		cl:                          cl,
		prepareRequest:              prepareRequest,
		expire:                      expire,
		useInternalTorrentHTTPProxy: c.Bool(useInternalTorrentHTTPProxyFlag),
		torrentHTTPProxyHost:        c.String(torrentHTTPProxyHostFlag),
		torrentHTTPProxyPort:        c.Int(torrentHTTPProxyPortFlag),
		torrentHTTPProxyPathPrefix:  c.String(torrentHTTPProxyPathPrefixFlag),
	}
}

func (s *Api) ListResourceContent(ctx context.Context, c *Claims, infohash string, args *ListResourceContentArgs) (e *ra.ListResponse, err error) {
	u := s.url + "/resource/" + infohash + "/list?" + args.ToQuery().Encode()
	e = &ra.ListResponse{}
	err = s.doRequest(ctx, c, u, "GET", nil, e)
	return
}

func (s *Api) doRequestRaw(ctx context.Context, c *Claims, url string, method string, data []byte) (res *http.Response, err error) {
	var payload io.Reader

	if data != nil {
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)

	if err != nil {
		return
	}

	req, err = s.prepareRequest(req, c)

	if err != nil {
		return
	}

	res, err = s.cl.Do(req)
	if err != nil {
		return
	}

	return
}

func (s *Api) doRequest(ctx context.Context, c *Claims, url string, method string, data []byte, v any) error {
	res, err := s.doRequestRaw(ctx, c, url, method, data)
	if err != nil {
		return err
	}

	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(res.Body)

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if res.StatusCode == http.StatusOK {
		err = json.Unmarshal(body, v)
		if err != nil {
			return err
		}
		return nil
	} else if res.StatusCode == http.StatusNotFound {
		return nil
	} else if res.StatusCode == http.StatusForbidden {
		return errors.Errorf("access is forbidden url=%v", url)
	} else {
		var e ra.ErrorResponse
		err = json.Unmarshal(body, &e)
		if err != nil {
			return errors.Wrapf(err, "failed to parse status=%v body=%v url=%v", res.StatusCode, body, url)
		}
		return errors.New(e.Error)
	}
}

func (s *Api) ExportResourceContent(ctx context.Context, c *Claims, infohash string, itemID string) (e *ra.ExportResponse, err error) {
	u := s.url + "/resource/" + infohash + "/export/" + itemID + "?use-premium-domain=false"
	e = &ra.ExportResponse{}
	err = s.doRequest(ctx, c, u, "GET", nil, e)
	// if e.Source.ID == nil
	// 	e = nil
	// }
	return
}

func (s *Api) Download(ctx context.Context, u string) (io.ReadCloser, error) {
	return s.DownloadWithRange(ctx, u, 0, -1)
}

// makeTorrentHTTPProxyRequest rewrites u to hit torrent-http-proxy directly
// when useInternalTorrentHTTPProxy is set, instead of the external host
// (typically an edge nginx) baked into the export URL rest-api handed out.
//
// An edge that fronts multiple services on one hostname (self-hosted's
// nginx does, since every service shares port 8080) commonly routes to
// torrent-http-proxy by path prefix rather than by subdomain, and strips
// that prefix before proxying. rest-api bakes the external-facing prefix
// into every export URL it builds (its own EXPORT_PATH_PREFIX), because it
// has no way to know the request will end up going straight to
// torrent-http-proxy instead of through that edge. torrent-http-proxy's own
// router has no notion of the prefix -- it expects the infohash as the
// first path segment -- so going internal must also strip it here, or
// every internal request 404s/500s on a path torrent-http-proxy cannot
// parse while looking, from the outside, exactly like a working torrent
// fetch (see FetchTorrent's comment for the failure this produced before
// path stripping existed).
func (s *Api) makeTorrentHTTPProxyRequest(ctx context.Context, u string) (*http.Request, error) {
	if s.useInternalTorrentHTTPProxy {
		internal := fmt.Sprintf("%v:%v", s.torrentHTTPProxyHost, s.torrentHTTPProxyPort)
		ur, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		ur.Host = internal
		ur.Scheme = "http"
		if s.torrentHTTPProxyPathPrefix != "" {
			prefix := "/" + strings.Trim(s.torrentHTTPProxyPathPrefix, "/") + "/"
			if strings.HasPrefix(ur.Path, prefix) {
				ur.Path = "/" + strings.TrimPrefix(ur.Path, prefix)
			}
		}
		u = ur.String()
	}
	return http.NewRequestWithContext(ctx, "GET", u, nil)
}

// FetchTorrent retrieves the bencode .torrent for the given resource,
// hosted at the seeder behind the given export URL. It reuses the auth
// tokens already embedded in the export URL by replacing the whole path
// with "/{infohash}/source.torrent" — the seeder serves the torrent at that
// route via renderTorrent (see
// torrent-web-seeder/server/services/web_seeder.go).
//
// infohash is passed in explicitly rather than read off exportURL's first
// path segment: rest-api's EXPORT_PATH_PREFIX (self-hosted sets it to
// "/torrent-http-proxy/" so its single nginx can route exported URLs to
// torrent-http-proxy by path instead of by subdomain) shifts the infohash to
// a later segment, and parsing the URL blind produced "torrent-http-proxy"
// as the "infohash" and a 404/500 loop that left every pledge stuck at
// vaulted=false with no error surfaced to the caller.
func (s *Api) FetchTorrent(ctx context.Context, exportURL string, infohash string) ([]byte, error) {
	if infohash == "" {
		return nil, errors.New("empty infohash")
	}
	u, err := url.Parse(exportURL)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse export URL")
	}
	u.Path = "/" + infohash + "/source.torrent"

	req, err := s.makeTorrentHTTPProxyRequest(ctx, u.String())
	if err != nil {
		return nil, err
	}
	res, err := s.cl.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch torrent")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.Errorf("unexpected status %d fetching torrent from %s", res.StatusCode, u.String())
	}
	return io.ReadAll(res.Body)
}

func (s *Api) DownloadWithRange(ctx context.Context, u string, start int, end int) (io.ReadCloser, error) {
	req, err := s.makeTorrentHTTPProxyRequest(ctx, u)
	if err != nil {
		log.WithError(err).Error("failed to make new request")
		return nil, err
	}
	if start != 0 || end != -1 {
		startStr := strconv.Itoa(start)
		endStr := ""
		if end != -1 {
			endStr = strconv.Itoa(end)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%v-%v", startStr, endStr))
	}
	log.WithFields(log.Fields{
		"url":   req.URL.String(),
		"start": start,
		"end":   end,
	}).Info("downloading with range")
	res, err := s.cl.Do(req)
	if err != nil {
		log.WithError(err).Error("failed to do request")
		return nil, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		_ = res.Body.Close()
		return nil, errors.Errorf("unexpected status %d downloading url=%s", res.StatusCode, req.URL.String())
	}
	return res.Body, nil
}
