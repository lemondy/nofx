package security

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestValidateURLLANPolicyReview(t *testing.T) {
	old := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = old })
	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		ip := net.ParseIP(host)
		if ip == nil {
			ip = net.ParseIP("192.168.50.165")
		}
		return []net.IPAddr{{IP: ip}}, nil
	}
	for _, tc := range []struct {
		url, allow string
		ok         bool
	}{
		{"http://192.168.50.165:3425/v1", "", true},
		{"http://model.example:3425/v1", "", false},
		{"http://model.example:3425/v1", "model.example:3425", true},
		{"http://model.example:3425/v1", "192.168.50.0/24", true},
		{"http://192.168.50.165:3425/v1", "10.0.0.0/8", false},
		{"http://192.168.50.165:3425/v1", "192.168.50.165", true},
		{"http://[fd00::123]:3425/v1", "", true},
	} {
		t.Run(tc.url+tc.allow, func(t *testing.T) {
			t.Setenv("NOFX_LAN_ALLOWLIST", tc.allow)
			if err := ValidateURL(tc.url); (err == nil) != tc.ok {
				t.Fatalf("err=%v want allowed=%v", err, tc.ok)
			}
		})
	}
}

func TestValidateURLDNSIPv6Review(t *testing.T) {
	old := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = old })
	lookupIPAddr = func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("fd12::1")}}, nil
	}
	for _, allow := range []string{"", "fd00::/8", "model.example:443", "model.example:80"} {
		t.Run(allow, func(t *testing.T) {
			t.Setenv("NOFX_LAN_ALLOWLIST", allow)
			err := ValidateURL("https://model.example:443")
			want := allow == "fd00::/8" || allow == "model.example:443"
			if (err == nil) != want {
				t.Fatalf("allow=%q err=%v", allow, err)
			}
		})
	}
}

func TestValidateURLRedirectLANReview(t *testing.T) {
	t.Setenv("NOFX_LAN_ALLOWLIST", "")
	client := SafeHTTPClient(time.Second)
	for _, origin := range []string{"https://8.8.8.8", "http://192.168.50.1"} {
		req, _ := http.NewRequest("GET", "http://192.168.50.165/v1", nil)
		first, _ := http.NewRequest("GET", origin, nil)
		err := client.CheckRedirect(req, []*http.Request{first})
		if (err == nil) != (origin == "http://192.168.50.1") {
			t.Fatalf("origin=%s err=%v", origin, err)
		}
	}
}
