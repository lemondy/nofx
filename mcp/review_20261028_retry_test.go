package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reviewRetryClient(server *httptest.Server, stream bool) *Client {
	c := NewClient(
		WithProvider(ProviderCustom), WithAPIKey("shared-test-key"), WithBaseURL(server.URL),
		WithHTTPClient(server.Client()), WithLogger(NewNoopLogger()),
		WithStreamDecisions(stream), WithMaxRetries(3), WithRetryWaitBase(time.Millisecond),
	).(*Client)
	c.retryWait = func(context.Context, time.Duration) error { return nil }
	return c
}

func reviewWriteSuccess(w http.ResponseWriter, stream bool) {
	if stream {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	} else {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}
}

func TestReview20261028StreamingRetriesWithoutFallback(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504, 520, 524, 529, 599} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var streams, nonStreams atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Stream bool }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if !body.Stream {
					nonStreams.Add(1)
					reviewWriteSuccess(w, false)
					return
				}
				if streams.Add(1) == 1 {
					w.WriteHeader(status)
					fmt.Fprint(w, "try later")
					return
				}
				reviewWriteSuccess(w, true)
			}))
			defer server.Close()
			c := reviewRetryClient(server, true)
			waits := 0
			c.retryWait = func(context.Context, time.Duration) error { waits++; return nil }
			result, err := c.CallWithMessages("system", "user")
			if err != nil || result != "ok" || streams.Load() != 2 || nonStreams.Load() != 0 || waits != 1 {
				t.Fatalf("result=%q err=%v streams=%d fallback=%d waits=%d", result, err, streams.Load(), nonStreams.Load(), waits)
			}
		})
	}
}

func TestReview20261028NonStreamingStatusRetries(t *testing.T) {
	for _, tc := range []struct {
		status, failures, attempts int
		wantErr                    bool
	}{
		{429, 2, 3, false}, {500, 1, 2, false}, {529, 1, 2, false},
		{400, 1, 1, true}, {401, 1, 1, true}, {429, 4, 4, true}, // MaxRetries(3) + status bonus
	} {
		t.Run(fmt.Sprintf("%d_failures_%d", tc.status, tc.failures), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if int(calls.Add(1)) <= tc.failures {
					w.WriteHeader(tc.status)
					// A non-retryable typed status must take precedence over body text.
					fmt.Fprint(w, "timeout status 429")
					return
				}
				reviewWriteSuccess(w, false)
			}))
			defer server.Close()
			result, err := reviewRetryClient(server, false).CallWithMessages("system", "user")
			if (err != nil) != tc.wantErr || int(calls.Load()) != tc.attempts {
				t.Fatalf("err=%v calls=%d, want error=%v calls=%d", err, calls.Load(), tc.wantErr, tc.attempts)
			}
			if !tc.wantErr && result != "ok" {
				t.Fatalf("result=%q", result)
			}
			if tc.wantErr {
				var statusErr *APIStatusError
				if !errors.As(err, &statusErr) || statusErr.StatusCode != tc.status {
					t.Fatalf("lost typed status: %v", err)
				}
			}
		})
	}
}

type reviewCallMode struct {
	name   string
	stream bool
	call   func(*Client) (string, error)
}

func reviewCallModes() []reviewCallMode {
	return []reviewCallMode{
		{"messages", false, func(c *Client) (string, error) { return c.CallWithMessages("system", "user") }},
		{"messages_stream", true, func(c *Client) (string, error) { return c.CallWithMessages("system", "user") }},
		{"request", false, func(c *Client) (string, error) { return c.CallWithRequest(&Request{}) }},
		{"full", false, func(c *Client) (string, error) {
			r, err := c.CallWithRequestFull(&Request{})
			if err != nil {
				return "", err
			}
			return r.Content, nil
		}},
	}
}

func TestReview20261028RetryAfterAcrossRetryLoops(t *testing.T) {
	for _, mode := range reviewCallModes() {
		t.Run(mode.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "2")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				reviewWriteSuccess(w, mode.stream)
			}))
			defer server.Close()
			c := reviewRetryClient(server, mode.stream)
			log := NewMockLogger()
			c.Log = log
			var waits []time.Duration
			c.retryWait = func(ctx context.Context, d time.Duration) error {
				waits = append(waits, d)
				return ctx.Err()
			}
			result, err := mode.call(c)
			if err != nil || result != "ok" || calls.Load() != 2 || len(waits) != 1 || waits[0] < 2*time.Second {
				t.Fatalf("result=%q err=%v calls=%d waits=%v", result, err, calls.Load(), waits)
			}
			logs := log.GetLogsByLevel("WARN")
			if len(logs) != 1 || !strings.Contains(logs[0].Message, "status 429") || !strings.Contains(logs[0].Message, "retrying in 2s") {
				t.Fatalf("retry log missing status/wait: %v", logs)
			}
		})
	}
}

func TestReview20261028APIStatusErrorTextAndHeaders(t *testing.T) {
	const body = "provider failure"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(529)
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	modes := []reviewCallMode{
		{"call", false, func(c *Client) (string, error) { return c.Call("system", "user") }},
		{"request", false, func(c *Client) (string, error) { return c.callWithRequest(&Request{}) }},
		{"full", false, func(c *Client) (string, error) { _, err := c.callWithRequestFull(&Request{}); return "", err }},
		{"stream", true, func(c *Client) (string, error) { return c.callStreamSingle("system", "user", time.Second) }},
		{"request_stream", true, func(c *Client) (string, error) { return c.CallWithRequestStream(&Request{}, nil) }},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			_, err := mode.call(reviewRetryClient(server, mode.stream))
			prefix := "API returned error"
			if mode.stream {
				prefix = "API error"
			}
			want := fmt.Sprintf("%s (status 529): %s", prefix, body)
			var statusErr *APIStatusError
			if !errors.As(err, &statusErr) || err.Error() != want || statusErr.RetryAfter != 2*time.Second {
				t.Fatalf("error=%v, want %q with typed RetryAfter=2s", err, want)
			}
		})
	}
	if got := (&APIStatusError{StatusCode: 429, Body: body}).Error(); got != "API returned error (status 429): "+body {
		t.Fatalf("default error text=%q", got)
	}
}

func TestReview20261028ParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"2", 2 * time.Second}, {" 2 ", 2 * time.Second}, {"0", 0},
		{"60", time.Minute}, {"120", time.Minute}, {strings.Repeat("9", 100), time.Minute},
		{"-1", 0}, {"+2", 0}, {"1.5", 0}, {"garbage", 0}, {"", 0},
		{now.Add(2 * time.Second).Format(http.TimeFormat), 2 * time.Second},
		{now.Add(2 * time.Minute).Format(http.TimeFormat), time.Minute},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got := parseRetryAfter(tc.value, now); got != tc.want {
				t.Fatalf("parseRetryAfter=%v want %v", got, tc.want)
			}
		})
	}
}

func TestReview20261028Backoff(t *testing.T) {
	oldJitter := jitter
	t.Cleanup(func() { jitter = oldJitter })
	for _, tc := range []struct {
		base    time.Duration
		attempt int
		d       time.Duration
	}{
		{2 * time.Second, 1, 2 * time.Second}, {2 * time.Second, 2, 4 * time.Second},
		{2 * time.Second, 3, 8 * time.Second}, {2 * time.Second, 4, 16 * time.Second},
		{2 * time.Second, 5, 30 * time.Second}, {2 * time.Second, 1000, 30 * time.Second},
		{time.Duration(math.MaxInt64), 1000, 30 * time.Second}, {0, 1000, 0}, {-time.Second, 1, 0},
	} {
		for _, upper := range []bool{false, true} {
			jitter = func(n int64) int64 {
				if upper {
					return n - 1
				}
				return 0
			}
			want := tc.d / 2
			if upper && tc.d > 0 {
				want = tc.d - time.Nanosecond
			}
			if got := retryDelay(tc.base, tc.attempt, nil); got != want {
				t.Errorf("base=%v attempt=%d upper=%v: got %v want %v", tc.base, tc.attempt, upper, got, want)
			}
		}
	}
	jitter = func(int64) int64 { return 0 }
	for _, tc := range []struct{ hint, want time.Duration }{
		{-time.Second, time.Second}, {500 * time.Millisecond, time.Second},
		{2 * time.Second, 2 * time.Second}, {90 * time.Second, time.Minute},
	} {
		err := fmt.Errorf("wrapped: %w", &APIStatusError{StatusCode: 429, RetryAfter: tc.hint})
		if got := retryDelay(2*time.Second, 1, err); got != tc.want {
			t.Errorf("hint=%v: got %v want %v", tc.hint, got, tc.want)
		}
	}
}

func TestReview20261028TypedAndLegacyRetryPolicy(t *testing.T) {
	c := NewClient().(*Client)
	for _, status := range []int{408, 429, 500, 502, 503, 504, 520, 524, 529} {
		for _, err := range []error{
			fmt.Errorf("wrapped: %w", &APIStatusError{StatusCode: status}),
			fmt.Errorf("legacy API returned error (status %d)", status),
		} {
			if !c.IsRetryableError(err) {
				t.Errorf("not retryable: %v", err)
			}
		}
	}
	for _, status := range []int{400, 401} {
		if c.IsRetryableError(fmt.Errorf("wrapped: %w", &APIStatusError{StatusCode: status, Body: "timeout"})) {
			t.Errorf("status %d should not retry", status)
		}
	}
	if c.IsRetryableError(nil) {
		t.Error("nil error should not retry")
	}
	// Default budget stays 2; status errors get statusErrorBonusAttempts on
	// top (see TestReview20261028StatusErrorsGetBonusAttempt).
	if c.Cfg.MaxRetries != 2 {
		t.Fatalf("default attempts=%d, want 2", c.Cfg.MaxRetries)
	}
}

func reviewSetLimiter(t *testing.T, value string) {
	t.Helper()
	t.Setenv("NOFX_LLM_MAX_INFLIGHT", value)
	if value == "" {
		if err := os.Unsetenv("NOFX_LLM_MAX_INFLIGHT"); err != nil {
			t.Fatal(err)
		}
	}
	oldLimit, oldLimiter := llmMaxInflight, llmInflightLimiter
	llmMaxInflight = sync.OnceValue(func() int { return getEnvInt("NOFX_LLM_MAX_INFLIGHT", 0) })
	llmInflightLimiter = &inflightLimiter{}
	t.Cleanup(func() { llmMaxInflight, llmInflightLimiter = oldLimit, oldLimiter })
}

func reviewReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request")
		var zero T
		return zero
	}
}

func reviewWaitForLimiterRefs(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		llmInflightLimiter.mu.Lock()
		refs := 0
		for _, entry := range llmInflightLimiter.entries {
			refs += entry.refs
		}
		llmInflightLimiter.mu.Unlock()
		if refs == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("limiter did not reach %d references", want)
}

func TestReview20261028LimiterSerializesAndCancels(t *testing.T) {
	modes := append(reviewCallModes(), reviewCallMode{
		"request_stream", true, func(c *Client) (string, error) { return c.CallWithRequestStream(&Request{}, nil) },
	})
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			reviewSetLimiter(t, "1")
			entered := make(chan struct{}, 3)
			finish := make(chan struct{})
			var finishOnce sync.Once
			unblock := func() { finishOnce.Do(func() { close(finish) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any
				json.NewDecoder(r.Body).Decode(&body)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush() // slot must survive receipt of headers
				entered <- struct{}{}
				select {
				case <-r.Context().Done():
					return
				case <-finish:
					reviewWriteSuccess(w, mode.stream)
				}
			}))
			defer server.Close()
			defer unblock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := reviewRetryClient(server, mode.stream)
			first.SetRequestContext(ctx)
			log := NewMockLogger()
			first.Log = log
			firstDone := make(chan error, 1)
			go func() { _, err := mode.call(first); firstDone <- err }()
			reviewReceive(t, entered)

			// The environment is read once, even if it changes after the first call.
			t.Setenv("NOFX_LLM_MAX_INFLIGHT", "0")
			waitCtx, cancelWait := context.WithCancel(context.Background())
			defer cancelWait()
			waiting := reviewRetryClient(server, mode.stream)
			waiting.Log = log
			waiting.SetRequestContext(waitCtx)
			waitDone := make(chan error, 1)
			go func() { _, err := mode.call(waiting); waitDone <- err }()
			reviewWaitForLimiterRefs(t, 2)
			cancelWait()
			if err := reviewReceive(t, waitDone); !errors.Is(err, context.Canceled) {
				t.Fatalf("waiting cancel=%v", err)
			}
			reviewWaitForLimiterRefs(t, 1)

			second := reviewRetryClient(server, mode.stream)
			second.Log = log
			second.BaseURL += "/other-path" // paths on the same host share the slot
			secondDone := make(chan error, 1)
			go func() { _, err := mode.call(second); secondDone <- err }()
			reviewWaitForLimiterRefs(t, 2)
			select {
			case <-entered:
				t.Fatal("second HTTP attempt entered while the first still held its slot")
			default:
			}
			cancel()
			if err := reviewReceive(t, firstDone); !errors.Is(err, context.Canceled) {
				t.Fatalf("in-flight cancel=%v", err)
			}
			reviewReceive(t, entered)
			unblock()
			if err := reviewReceive(t, secondDone); err != nil {
				t.Fatalf("second call=%v", err)
			}
			reviewWaitForLimiterRefs(t, 0)
			enabledLogs := 0
			for _, entry := range log.GetLogs() {
				if strings.Contains(entry.Message, "limiter enabled") {
					enabledLogs++
				}
			}
			if enabledLogs != 1 {
				t.Fatalf("enable log count=%d", enabledLogs)
			}
		})
	}
}

func TestReview20261028LimiterUnlimitedAndKeyIsolation(t *testing.T) {
	for _, tc := range []struct{ name, limit, secondKey string }{
		{"unset", "", "shared-test-key"}, {"zero", "0", "shared-test-key"},
		{"invalid", "garbage", "shared-test-key"}, {"negative", "-1", "shared-test-key"},
		{"different_keys", "1", "different-test-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewSetLimiter(t, tc.limit)
			entered := make(chan struct{}, 2)
			finish := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				<-finish
				reviewWriteSuccess(w, false)
			}))
			defer server.Close()
			var finishOnce sync.Once
			unblock := func() { finishOnce.Do(func() { close(finish) }) }
			defer unblock()
			first, second := reviewRetryClient(server, false), reviewRetryClient(server, false)
			second.APIKey = tc.secondKey
			done := make(chan error, 2)
			for _, c := range []*Client{first, second} {
				go func() { _, err := c.CallWithMessages("system", "user"); done <- err }()
			}
			reviewReceive(t, entered)
			reviewReceive(t, entered)
			unblock()
			for range 2 {
				if err := reviewReceive(t, done); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReview20261028LimiterReleasesBeforeBackoffAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback_%v", fallback), func(t *testing.T) {
			reviewSetLimiter(t, "1")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					if fallback {
						w.WriteHeader(http.StatusBadRequest)
					} else {
						w.WriteHeader(http.StatusTooManyRequests)
					}
					return
				}
				reviewWriteSuccess(w, false)
			}))
			defer server.Close()
			c := reviewRetryClient(server, fallback)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c.SetRequestContext(ctx)
			c.retryWait = func(context.Context, time.Duration) error {
				_, err := c.Call("system", "during backoff")
				return err
			}
			result, err := c.CallWithMessages("system", "user")
			wantCalls := int32(3)
			if fallback {
				wantCalls = 2
			}
			if err != nil || result != "ok" || calls.Load() != wantCalls {
				t.Fatalf("result=%q err=%v calls=%d want=%d", result, err, calls.Load(), wantCalls)
			}
		})
	}
}

func TestReview20261028RetryWaitCancellation(t *testing.T) {
	c := NewClient().(*Client)
	ctx, cancel := context.WithCancel(context.Background())
	c.SetRequestContext(ctx)
	done := make(chan error, 1)
	go func() { done <- c.waitForRetry(time.Minute) }()
	cancel()
	if err := reviewReceive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry wait cancel=%v", err)
	}
}

func TestReview20261028StatusErrorsGetBonusAttempt(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{errors.New("context deadline exceeded"), 2},
		{&APIStatusError{StatusCode: 429, Body: "rate limited"}, 3},
		{errors.New("API returned error (status 503): busy"), 3},
	} {
		calls := 0
		c := NewClient(WithAPIKey("sk-test"), WithStreamDecisions(false), WithRetryWaitBase(time.Nanosecond)).(*Client)
		c.Hooks = &countingHooks{Client: c, err: tc.err, calls: &calls}
		if _, err := c.CallWithMessages("s", "u"); err == nil {
			t.Fatalf("%v: expected error", tc.err)
		}
		if calls != tc.want {
			t.Fatalf("%v: attempts=%d want %d", tc.err, calls, tc.want)
		}
	}
}

type countingHooks struct {
	*Client
	err   error
	calls *int
}

func (h *countingHooks) Call(string, string) (string, error) {
	*h.calls++
	return "", h.err
}
