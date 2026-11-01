package mcp

import (
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// APIStatusError preserves the API's status and retry hint without changing
// the error text consumed by existing callers.
type APIStatusError struct {
	StatusCode int
	RetryAfter time.Duration
	Body       string
	prefix     string
}

func (e *APIStatusError) Error() string {
	prefix := e.prefix
	if prefix == "" {
		prefix = "API returned error"
	}
	return fmt.Sprintf("%s (status %d): %s", prefix, e.StatusCode, e.Body)
}

func newAPIStatusError(resp *http.Response, body, prefix string) *APIStatusError {
	return &APIStatusError{
		StatusCode: resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		Body:       body,
		prefix:     prefix,
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	// Delta-seconds permits only decimal digits, without a sign or fraction.
	if value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds >= 60 {
			return 60 * time.Second
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(date.Sub(now), 0), 60*time.Second)
	}
	return 0
}

// statusErrorBonusAttempts is the extra CallWithMessages attempt granted to
// fast-failing HTTP status errors (with the default budget: 3 attempts on 429).
const statusErrorBonusAttempts = 1

var reStatusCode = regexp.MustCompile(`status \d{3}`)

// isStatusError reports an HTTP status response, typed or as legacy provider
// error text ("... (status 429): ...").
func isStatusError(err error) bool {
	var statusErr *APIStatusError
	return errors.As(err, &statusErr) || reStatusCode.MatchString(err.Error())
}

func isRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status < 600)
}

// jitter is replaceable for deterministic tests. rand.Int63n is concurrency safe.
var jitter = rand.Int63n

// retryDelay uses equal jitter on a capped exponential delay. Retry-After is a
// lower bound, subject to the separate 60 second server-hint cap.
func retryDelay(base time.Duration, attempt int, err error) time.Duration {
	d := min(max(base, 0), 30*time.Second)
	for n := 1; n < attempt && d > 0 && d < 30*time.Second; n++ {
		d = min(d*2, 30*time.Second)
	}
	wait := d / 2
	if half := d / 2; half > 0 {
		wait += time.Duration(jitter(int64(half)))
	}
	var statusErr *APIStatusError
	if errors.As(err, &statusErr) && statusErr.RetryAfter > 0 {
		wait = max(wait, statusErr.RetryAfter)
	}
	return min(wait, 60*time.Second)
}

func (client *Client) retryAfterError(err error, attempt, maxAttempts int) error {
	wait := retryDelay(client.Cfg.RetryWaitBase, attempt, err)
	// The error retains the status text for both typed errors and legacy
	// provider errors, alongside the chosen wait and failed attempt number.
	client.Log.Warnf("⚠️  AI API call failed (attempt %d/%d): %v — retrying in %v", attempt, maxAttempts, err, wait)
	return client.waitForRetry(wait)
}
