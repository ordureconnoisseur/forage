// Package clienterr classifies external-client failures so callers can decide
// handling with errors.Is instead of parsing error strings. The three classes
// are the only decisions a caller actually makes about a failed client call:
//
//   - ErrTransient: retry (or skip this tick) — the call may succeed later.
//   - ErrNotFound:  act on absence now — the target definitively isn't there.
//   - ErrRejected:  give up — the request was understood and refused.
//
// The whole point: a caller (the poller especially) must never treat a transient
// failure as "the download/scene is gone" and advance a grab to a terminal
// state on it. See docs/error-handling.md.
package clienterr

import (
	"errors"
	"fmt"
	"net/http"
)

var (
	// ErrTransient marks a failure that may succeed on retry: a timeout, a
	// dropped/refused connection, a 5xx, a 408/429. Callers must NOT treat the
	// target as gone or fail a terminal state on it — retry, or skip and try
	// again next tick.
	ErrTransient = errors.New("transient client error")
	// ErrNotFound marks a target that definitively does not exist: an HTTP 404,
	// or a lookup that resolved to no row. Callers can act on absence at once.
	ErrNotFound = errors.New("not found")
	// ErrRejected marks a request the server understood and refused, where a
	// retry won't help: a 4xx other than 404, an auth failure, a malformed
	// request.
	ErrRejected = errors.New("rejected by client")
)

// Transport classifies an error from http.Client.Do. Once the request has been
// built, every Do error is connection-level (timeout, connection refused/reset,
// DNS failure, unexpected EOF) and therefore retryable, so it wraps
// ErrTransient — while preserving the original error so callers can still
// errors.Is for context.DeadlineExceeded or a net.Error. label gives context
// (e.g. "prowlarr search"). Returns nil for a nil err.
func Transport(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w (%w)", label, err, ErrTransient)
}

// Status classifies a non-2xx HTTP response, returning nil for 2xx. label and
// body build a human-readable message; the wrapped sentinel makes the class
// matchable with errors.Is. 404 -> ErrNotFound; 408/429/5xx -> ErrTransient;
// any other non-2xx -> ErrRejected. The original status code stays readable
// via Code, for callers whose reaction depends on WHICH transient failure it
// was (a 429 asks for a much longer back-off than a flaky 502).
func Status(label string, code int, body []byte) error {
	if code >= 200 && code < 300 {
		return nil
	}
	sentinel := ErrRejected
	switch {
	case code == http.StatusNotFound:
		sentinel = ErrNotFound
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500:
		sentinel = ErrTransient
	}
	return &statusErr{code: code, msg: fmt.Sprintf("%s %d: %s", label, code, truncate(body)), sentinel: sentinel}
}

// statusErr carries the HTTP status code alongside the classification
// sentinel. Error text and errors.Is behaviour are identical to the plain
// wrapped error Status used to return.
type statusErr struct {
	code     int
	msg      string
	sentinel error
}

func (e *statusErr) Error() string { return fmt.Sprintf("%s (%v)", e.msg, e.sentinel) }
func (e *statusErr) Unwrap() error { return e.sentinel }

// Code returns the HTTP status code carried by an error produced by Status,
// walking wrap chains; 0 when the error didn't come from an HTTP status
// (transport failures, semantic errors, nil).
func Code(err error) int {
	var se *statusErr
	if errors.As(err, &se) {
		return se.code
	}
	return 0
}

// truncate bounds an error body so a giant HTML/JSON error page can't bloat the
// wrapped message and the logs.
func truncate(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
