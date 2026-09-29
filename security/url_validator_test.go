package security

import (
	"net/http"
	"testing"
	"time"
)

// The schemeless proxy form ("127.0.0.1:7890") must land on the exemption
// list — http.ProxyFromEnvironment accepts it and dials the proxy, so a
// mismatch here made the SSRF guard block the operator's own proxy as a
// private IP (2026-09-30 candidate-pool collapse).
func TestEnvProxyHostsSchemeless(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "127.0.0.1:7890")
	t.Setenv("HTTP_PROXY", "127.0.0.1:7890")
	t.Setenv("ALL_PROXY", "")

	hosts := envProxyHosts()
	if !hosts["127.0.0.1"] {
		t.Fatalf("schemeless proxy missing from exemption list: %v", hosts)
	}
}

// The canonical scheme'd form keeps working, and a bogus value must not
// poison the whole list.
func TestEnvProxyHostsSchemedAndBogus(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("HTTP_PROXY", "not a url :::")
	hosts := envProxyHosts()
	if !hosts["127.0.0.1"] {
		t.Fatalf("schemed proxy missing from exemption list: %v", hosts)
	}
}

// End-to-end shape: with the schemeless env set, the client the fetchers use
// must be built against an exemption list that actually contains the proxy
// host (the same source DialContext reads).
func TestSafeHTTPClientExemptsSchemelessProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "127.0.0.1:7890")
	client := SafeHTTPClient(5 * time.Second)
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("unexpected transport")
	}
	if hosts := envProxyHosts(); !hosts["127.0.0.1"] {
		t.Fatalf("DialContext exemption source missing the proxy: %v", hosts)
	}
}
