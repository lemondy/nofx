package mcp

import (
	"strings"
	"testing"
)

func TestParseMCPResponseFullFallsBackToReasoningContent(t *testing.T) {
	client := &Client{Log: NewNoopLogger()}

	body := []byte(`{
		"choices": [{
			"message": {
				"content": "",
				"reasoning_content": "The market is bearish. I decide to wait. [ {\"symbol\":\"BTCUSDT\",\"action\":\"wait\"} ]"
			},
			"finish_reason": "stop"
		}]
	}`)

	resp, err := client.ParseMCPResponseFull(body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if resp.Content == "" {
		t.Fatal("expected reasoning_content fallback to be returned as content")
	}
	if want := "The market is bearish"; len(resp.Content) < len(want) || resp.Content[:len(want)] != want {
		t.Fatalf("unexpected content: %q", resp.Content)
	}
}

func TestParseMCPResponseFullPrefersContent(t *testing.T) {
	client := &Client{Log: NewNoopLogger()}

	body := []byte(`{
		"choices": [{
			"message": {
				"content": "[{\"symbol\":\"BTCUSDT\",\"action\":\"wait\"}]",
				"reasoning_content": "thinking..."
			},
			"finish_reason": "stop"
		}]
	}`)

	resp, err := client.ParseMCPResponseFull(body)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if resp.Content != `[{"symbol":"BTCUSDT","action":"wait"}]` {
		t.Fatalf("content should take precedence, got %q", resp.Content)
	}
}

func TestParseSSEStreamFallsBackToReasoningContent(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking part 1 \"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"part 2\"}}]}\n" +
		"data: [DONE]\n"

	got, err := ParseSSEStream(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatalf("stream parse failed: %v", err)
	}
	if got != "thinking part 1 part 2" {
		t.Fatalf("unexpected reasoning fallback: %q", got)
	}
}

func TestParseSSEStreamPrefersContent(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hmm\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"final answer\"}}]}\n" +
		"data: [DONE]\n"

	got, err := ParseSSEStream(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatalf("stream parse failed: %v", err)
	}
	if got != "final answer" {
		t.Fatalf("content should take precedence, got %q", got)
	}
}
