package security

import (
	"strings"
	"testing"
)

func TestReadResponseBodyBound(t *testing.T) {
	if _, err := ReadResponseBody(strings.NewReader(strings.Repeat("a", MaxResponseBytes+1))); err == nil {
		t.Fatal("oversized response accepted")
	}
	got, err := ReadResponseBody(strings.NewReader("ok"))
	if err != nil || string(got) != "ok" {
		t.Fatalf("%s %v", got, err)
	}
}
