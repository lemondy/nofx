package security

import (
	"context"
	"errors"
	"net"
	"testing"
)

func stubDNS(t *testing.T, fn func(host string, call int) ([]net.IPAddr, error)) *int {
	t.Helper()
	calls := 0
	orig, origDelay := lookupIPAddr, dnsRetryDelay
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		calls++
		return fn(host, calls)
	}
	dnsRetryDelay = 0
	t.Cleanup(func() { lookupIPAddr, dnsRetryDelay = orig, origDelay })
	return &calls
}

// 2026-10-07 review N3: a built-in provider host whose local DNS lookup fails
// behind a proxy must not fail the AI cycle.
func TestValidateURLTrustedHostSurvivesDNSFailureBehindProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	stubDNS(t, func(string, int) ([]net.IPAddr, error) { return nil, errors.New("no such host") })
	if err := ValidateURL("https://open.bigmodel.cn/api/coding/paas/v4/chat/completions"); err != nil {
		t.Fatalf("trusted provider blocked: %v", err)
	}
}

// User-supplied hosts stay fail-closed.
func TestValidateURLUntrustedHostStaysBlockedBehindProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	stubDNS(t, func(string, int) ([]net.IPAddr, error) { return nil, errors.New("no such host") })
	if err := ValidateURL("https://internal-looking.example/api"); err == nil {
		t.Fatal("untrusted unresolvable host allowed behind proxy")
	}
	// suffix match must not accept lookalike domains
	if err := ValidateURL("https://evilbigmodel.cn/x"); err == nil {
		t.Fatal("lookalike domain treated as trusted")
	}
}

// A transient miss is retried, and the retried answer is still IP-checked.
func TestValidateURLRetriesTransientDNSMiss(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	calls := stubDNS(t, func(_ string, n int) ([]net.IPAddr, error) {
		if n == 1 {
			return nil, errors.New("temporary failure")
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
	})
	if err := ValidateURL("https://custom.example/v1"); err == nil {
		t.Fatal("retried lookup resolving to a private IP must be blocked")
	}
	if *calls != 2 {
		t.Fatalf("lookups=%d, want 2", *calls)
	}
}
