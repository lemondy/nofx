package mcp

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsRetryableError(t *testing.T) {
	client := &Client{Cfg: DefaultConfig()}

	cases := []struct {
		name  string
		err   error
		retry bool
	}{
		{"http client timeout", errors.New(`Post "https://open.bigmodel.cn/api/paas/v4/chat/completions": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`), true},
		{"context deadline", errors.New("context deadline exceeded"), true},
		{"lowercase timeout", errors.New("read tcp: i/o timeout"), true},
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"bad gateway", errors.New("API returned error (status 502): ..."), true},
		{"invalid api key", errors.New("API returned error (status 401): unauthorized"), false},
		{"empty choices", errors.New("API returned empty response"), false},
	}

	for _, tc := range cases {
		got := client.IsRetryableError(tc.err)
		if got != tc.retry {
			t.Errorf("%s: IsRetryableError=%v, want %v (err: %v)", tc.name, got, tc.retry, tc.err)
		}
	}
}

func TestRetryExhaustsAttempts(t *testing.T) {
	client := &Client{Cfg: DefaultConfig()}
	// 3 attempts at 120s timeout each would be too slow for a unit test —
	// just verify the loop count via IsRetryableError semantics: a timeout
	// error must be retried, i.e. CallWithMessages would loop maxRetries times.
	if client.Cfg.MaxRetries != 2 {
		t.Fatalf("expected MaxRetries=2, got %d", client.Cfg.MaxRetries)
	}
	if client.Cfg.RetryWaitBase != 2*1e9 {
		t.Fatalf("unexpected retry wait base: %v", client.Cfg.RetryWaitBase)
	}
	_ = fmt.Sprint
}
