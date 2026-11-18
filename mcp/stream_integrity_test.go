package mcp

import (
	"strings"
	"testing"
)

func TestStreamIntegrity(t *testing.T) {
	for _, s := range []string{"", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n", "data: {\"error\":{\"message\":\"overloaded\",\"code\":503}}\n", "data:{\"error\":\"429 rate limit\"}\n"} {
		_, err := parseSSEStream(strings.NewReader(s), nil, nil, nil)
		if err == nil {
			t.Errorf("accepted incomplete/error stream %q", s)
		}
	}
	s, err := parseSSEStream(strings.NewReader("data:{\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\ndata:[DONE]\n"), nil, nil, nil)
	if err != nil || s != "ok" {
		t.Fatalf("standard data: got %q %v", s, err)
	}
}

func TestStreamErrorsAreRetryable(t *testing.T) {
	c := NewClient().(*Client)
	for _, payload := range []string{
		`{"error":{"message":"busy","code":507}}`,
		`{"error":{"message":"busy","type":"server_error"}}`,
		`{"error":"429 rate limit"}`,
		`{"error":{"message":"overloaded"}}`,
	} {
		_, err := parseSSEStream(strings.NewReader("data:"+payload+"\ndata:[DONE]\n"), nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "upstream stream error:") || !c.IsRetryableError(err) {
			t.Errorf("unrecognized upstream error: %v", err)
		}
	}
	_, err := parseSSEStream(strings.NewReader(""), nil, nil, nil)
	if err == nil || !c.IsRetryableError(err) {
		t.Fatalf("EOF not retryable: %v", err)
	}
}
