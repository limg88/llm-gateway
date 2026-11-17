package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// How an answer ended, in the words of the Chat Completions API. Every
// backend reports one of these with its last events; an answer that ends
// without one is incomplete.
const (
	finishStop      = "stop"
	finishLength    = "length" // the output budget, or the model's own limit, was reached
	finishToolCalls = "tool_calls"
	finishFilter    = "content_filter"
)

// failKind is why a backend could not answer, as far as a client can act
// on it.
type failKind int

const (
	failGeneric     failKind = iota
	failAuth                 // the backend's own login or key, not the client's
	failRateLimit            // a temporary limit: wait and retry
	failQuota                // an allowance is used up: retrying soon is useless
	failOverloaded           // the provider is busy
	failTimeout              // the provider gave up waiting
	failTooLarge             // the input does not fit the model
	failOutputLimit          // the answer does not fit the output budget
	failInvalid              // the backend refused the request as it is
	failIncomplete           // the answer stopped before its end
)

// failures gives every kind its HTTP status, error type, stable machine
// code and public message. The message is fixed: what a backend says may
// carry credentials or pieces of the conversation, and goes to the log only.
var failures = map[failKind]struct {
	status          int
	typ, code, text string
}{
	failGeneric:     {http.StatusBadGateway, "backend_error", "backend_error", "the backend failed to answer"},
	failAuth:        {http.StatusBadGateway, "backend_error", "backend_authentication_error", "the backend is not logged in or its credentials were refused"},
	failRateLimit:   {http.StatusTooManyRequests, "rate_limit_error", "backend_rate_limit", "the backend is rate limited, retry later"},
	failQuota:       {http.StatusTooManyRequests, "rate_limit_error", "backend_quota_exceeded", "the backend's usage quota is exhausted"},
	failOverloaded:  {http.StatusServiceUnavailable, "overloaded_error", "backend_overloaded", "the backend is overloaded, retry later"},
	failTimeout:     {http.StatusGatewayTimeout, "timeout_error", "backend_timeout", "the backend timed out"},
	failTooLarge:    {http.StatusRequestEntityTooLarge, "invalid_request_error", "context_length_exceeded", "the input is too large for the model"},
	failOutputLimit: {http.StatusUnprocessableEntity, "invalid_request_error", "output_limit_exceeded", "the answer exceeded the output token budget and was not returned: raise the budget or ask for less"},
	failInvalid:     {http.StatusBadRequest, "invalid_request_error", "backend_invalid_request", "the backend refused the request: check the model and the parameters"},
	failIncomplete:  {http.StatusBadGateway, "backend_error", "backend_incomplete_response", "the backend's answer stopped before its end"},
}

// BackendError is a classified failure of a backend.
type BackendError struct {
	Kind       failKind
	Backend    string
	RetryAfter time.Duration // zero when the backend did not say
	Detail     string        // what the backend said, redacted and cut: for the log
}

func (e *BackendError) Error() string {
	return e.Backend + ": " + failures[e.Kind].text
}

func newBackendError(backend string, kind failKind, detail string) *BackendError {
	return &BackendError{Kind: kind, Backend: backend, Detail: redact(detail)}
}

// textFailure classifies what a CLI or a provider wrote about its failure.
func textFailure(backend, text string) *BackendError {
	e := newBackendError(backend, classify(text), text)
	if e.Kind == failRateLimit || e.Kind == failQuota || e.Kind == failOverloaded {
		e.RetryAfter = retryIn(text)
	}
	return e
}

var errNoOutput = errors.New("the backend returned no output")

// canFallback tells whether another target may be tried after err. An
// answer over the output budget would be over it elsewhere too.
func canFallback(err error) bool {
	var be *BackendError
	return !errors.As(err, &be) || be.Kind != failOutputLimit
}

var failPatterns = []struct {
	kind failKind
	re   *regexp.Regexp
}{
	{failOutputLimit, regexp.MustCompile(`output token maximum|max_output_tokens`)},
	{failTooLarge, regexp.MustCompile(`prompt is too long|context.{0,20}(length|window|limit)|too many (input )?tokens|input is too (long|large)|request too large|maximum context`)},
	{failQuota, regexp.MustCompile(`quota|usage limit|credit balance|out of (credits|extra usage)|billing|insufficient.{0,10}(credits|funds)|usage_not_included`)},
	{failRateLimit, regexp.MustCompile(`rate.?limit|too many requests|\b429\b`)},
	{failQuota, regexp.MustCompile(`hit your .{0,20}limit|limit reached`)},
	{failAuth, regexp.MustCompile(`unauthori[sz]ed|authenticat|not logged in|log ?in|sign in|invalid.{0,20}(api.?key|token|credentials)|forbidden|oauth|expired|\b40[13]\b`)},
	{failOverloaded, regexp.MustCompile(`overloaded|server is busy|capacity|temporarily unavailable|\b(503|529)\b`)},
	{failTimeout, regexp.MustCompile(`timed? ?out|deadline exceeded`)},
}

// classify reads the kind of a failure from its text. It is used on error
// output only, never on an answer, which may well talk about limits.
func classify(text string) failKind {
	text = strings.ToLower(text)
	for _, p := range failPatterns {
		if p.re.MatchString(text) {
			return p.kind
		}
	}
	return failGeneric
}

var (
	retryAnchor = regexp.MustCompile(`(?i)(?:retry|try again)[ -]?(?:after|in)[: ]+`)
	retryPart   = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*(ms|milliseconds?|s|secs?|seconds?|m|mins?|minutes?|h|hrs?|hours?|d|days?)?\b[ ,]*(?:and )?`)
	retryUnits  = map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}
)

// retryIn finds a wait such as "try again in 1 hour 20 minutes" or
// "retry after 30" (seconds) in an error text.
func retryIn(text string) time.Duration {
	loc := retryAnchor.FindStringIndex(text)
	if loc == nil {
		return 0
	}
	rest := text[loc[1]:]
	var total time.Duration
	for {
		m := retryPart.FindStringSubmatch(rest)
		if m == nil {
			return total
		}
		n, _ := strconv.ParseFloat(m[1], 64)
		unit := time.Second
		switch {
		case strings.HasPrefix(m[2], "ms"), strings.HasPrefix(m[2], "milli"):
			unit = time.Millisecond
		case m[2] != "":
			unit = retryUnits[m[2][0]]
		}
		total += time.Duration(n * float64(unit))
		rest = rest[len(m[0]):]
	}
}

// retryAfterHeader reads a Retry-After header: seconds or an HTTP date.
func retryAfterHeader(v string) time.Duration {
	v = strings.TrimSpace(v)
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

var (
	secretValue = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[^\s"',;]+`)
	secretField = regexp.MustCompile(`(?i)(api[-_]?key|token|secret|password|authorization|cookie)("?\s*[:=]\s*"?)[^\s"',;]+`)
	secretKey   = regexp.MustCompile(`\b(sk|pk|gsk|xai|ghp|gho|key)[-_][A-Za-z0-9_\-]{12,}`)
)

const maxDetail = 300

// redact prepares a backend's own words for the log: on one line, without
// what looks like a credential, and short enough not to carry a prompt or
// an answer along.
func redact(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = secretValue.ReplaceAllString(s, "$1 [redacted]")
	s = secretField.ReplaceAllString(s, "$1$2[redacted]")
	s = secretKey.ReplaceAllString(s, "[redacted]")
	if r := []rune(s); len(r) > maxDetail {
		s = string(r[:maxDetail]) + "…"
	}
	return s
}

// apiErr is an error as the client sees it.
type apiErr struct {
	status     int
	typ        string
	code       string
	msg        string
	retryAfter time.Duration
}

func badRequest(format string, args ...any) *apiErr {
	return &apiErr{status: http.StatusBadRequest, typ: "invalid_request_error", code: "invalid_request_error", msg: fmt.Sprintf(format, args...)}
}

func busy(msg string) *apiErr {
	return &apiErr{status: http.StatusServiceUnavailable, typ: "overloaded_error", code: "gateway_busy", msg: msg, retryAfter: time.Second}
}

// statusClientClosed is logged, never read: the client is gone.
const statusClientClosed = 499

// publicError turns the failure of a job into what the client is told.
// Nothing of err's own text is passed on.
func publicError(err error) *apiErr {
	var be *BackendError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		f := failures[failTimeout]
		return &apiErr{status: f.status, typ: f.typ, code: f.code, msg: f.text}
	case errors.Is(err, context.Canceled):
		return &apiErr{status: statusClientClosed, typ: "invalid_request_error", code: "request_cancelled", msg: "the request was cancelled"}
	case errors.As(err, &be):
		f := failures[be.Kind]
		return &apiErr{status: f.status, typ: f.typ, code: f.code, msg: be.Error(), retryAfter: be.RetryAfter}
	case errors.Is(err, errNoOutput):
		return &apiErr{status: http.StatusBadGateway, typ: "backend_error", code: "backend_error", msg: errNoOutput.Error()}
	}
	f := failures[failGeneric]
	return &apiErr{status: f.status, typ: f.typ, code: f.code, msg: f.text}
}

// logFailure describes a failure for the log: always by its code, and at
// the debug level also by what the backend said, redacted and cut. Those
// words can quote the conversation, so they stay out of an ordinary log.
func logFailure(err error) []any {
	attrs := []any{"code", publicError(err).code}
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		detail := redact(err.Error())
		var be *BackendError
		if errors.As(err, &be) {
			detail = be.Detail
		}
		attrs = append(attrs, "detail", detail)
	}
	return attrs
}

// retrySeconds is the wait to tell the client, in whole seconds; zero for
// none.
func (e *apiErr) retrySeconds() int {
	return int(math.Ceil(e.retryAfter.Seconds()))
}

func (e *apiErr) header(w http.ResponseWriter) {
	if n := e.retrySeconds(); n > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(n))
	}
}

func (e *apiErr) body() APIErrorBody {
	return APIErrorBody{Message: e.msg, Type: e.typ, Code: e.code, RetryAfter: e.retrySeconds()}
}

// anthropicBody is the error in the shape of the Messages API, whose types
// differ a little; the code is the same.
func (e *apiErr) anthropicBody() map[string]any {
	typ := e.typ
	switch {
	case e.status == http.StatusRequestEntityTooLarge:
		typ = "request_too_large"
	case typ == "backend_error", typ == "server_error":
		typ = "api_error"
	}
	body := map[string]any{"type": typ, "code": e.code, "message": e.msg}
	if n := e.retrySeconds(); n > 0 {
		body["retry_after"] = n
	}
	return body
}

func writeAPIErr(w http.ResponseWriter, e *apiErr) {
	e.header(w)
	writeJSON(w, e.status, APIError{Error: e.body()})
}

func writeAnthropicAPIErr(w http.ResponseWriter, e *apiErr) {
	e.header(w)
	writeJSON(w, e.status, map[string]any{"type": "error", "error": e.anthropicBody()})
}

// outputBudget checks one output budget parameter of a request.
func outputBudget(name string, v *int) (int, *apiErr) {
	if v == nil {
		return 0, nil
	}
	if *v < 1 {
		return 0, badRequest("%s must be a positive integer", name)
	}
	return *v, nil
}
