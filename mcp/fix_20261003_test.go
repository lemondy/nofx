package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fixRoundTrip func(*http.Request) (*http.Response, error)

func (f fixRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStreamingPreservesRedirectPolicy(t *testing.T) {
	blocked := errors.New("redirect blocked")
	redirects, requests := 0, 0
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return blocked }, Transport: fixRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	c := NewClient(WithAPIKey("test"), WithHTTPClient(hc)).(*Client)
	c.BaseURL = "https://public.example"
	c.Cfg.MaxRetries = 1
	if _, err := c.callStreamSingle("system", "user", time.Second); !errors.Is(err, blocked) {
		t.Fatalf("redirect policy lost: %v", err)
	}
	if requests != 1 || redirects != 1 {
		t.Fatalf("requests=%d redirects=%d", requests, redirects)
	}
}
func TestTraderRequestCancellationInterruptsNormalAndStreamingCalls(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "streaming"}[stream], func(t *testing.T) {
			entered := make(chan struct{})
			hc := &http.Client{Transport: fixRoundTrip(func(r *http.Request) (*http.Response, error) {
				close(entered)
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			c := NewClient(WithAPIKey("test"), WithHTTPClient(hc), WithStreamDecisions(stream)).(*Client)
			c.Cfg.MaxRetries = 2
			ctx, cancel := context.WithCancel(context.Background())
			c.SetRequestContext(ctx)
			done := make(chan error, 1)
			go func() { _, err := c.CallWithMessages("system", "user"); done <- err }()
			<-entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("AI request did not cancel")
			}
		})
	}
}
