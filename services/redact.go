package services

import "regexp"

// The export URLs rest-api hands out carry credentials in the query: the
// platform API key (api-key) and a JWT (token). The worker downloads every
// file through such a URL, and it ended up whole in the "downloading with
// range" log line and in the error texts that handleError persists to
// resource.error and the worker log -- including the ones net/http builds
// itself: a *url.Error from http.Client.Do quotes the request URL with only
// a userinfo password stripped, never the query. Same rules as
// torrent-http-proxy's and web-ui's redactors (2026-09-30), so one Loki
// query finds what is left in all three.

// credentialParam is a credential parameter and its value: the name at the
// start of a query parameter, raw or percent-encoded once or twice (a URL
// passed on inside another), then `=` in any of those encodings. The value
// ends where the next parameter begins, encoded or not.
var credentialParam = regexp.MustCompile(`(?i)((?:^|[?&;]|%3F|%26|%253F|%2526)(?:token|api-key|api_key|apikey)(?:=|%3D|%253D))(?:[A-Za-z0-9._~-]|%2[Ee])+`)

// jwtLike is a JWT wherever it stands -- a path segment too.
var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)

const redacted = "<redacted>"

// redactURL is s as logs and stored errors may keep it: the values of its
// credential parameters, and any JWT, replaced by "<redacted>"; the names
// and everything else as is, so the line stays debuggable.
func redactURL(s string) string {
	s = credentialParam.ReplaceAllString(s, "${1}"+redacted)
	return jwtLike.ReplaceAllString(s, redacted)
}

// redactedError is an error whose message went through redactURL. The
// original stays reachable for errors.Is/As (Unwrap) and pkg/errors.Cause
// (Cause), so callers that tell a cancelled context from a network failure
// keep doing so. Wrapping the original with a redacted message would not be
// enough: the wrapped error's own message is what leaks.
type redactedError struct {
	err error
	msg string
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
func (e *redactedError) Cause() error  { return e.err }

// redactError is err with credentials stripped from its message, or err
// itself when there was nothing to strip.
func redactError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	r := redactURL(msg)
	if r == msg {
		return err
	}
	return &redactedError{err: err, msg: r}
}
