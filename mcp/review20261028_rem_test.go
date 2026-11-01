package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func remSSEChunk(content, reasoning, finish string) string {
	chunk := map[string]any{"choices": []any{map[string]any{
		"delta":         map[string]string{"content": content, "reasoning_content": reasoning},
		"finish_reason": finish,
	}}}
	data, _ := json.Marshal(chunk)
	return "data: " + string(data) + "\n\n"
}

func TestReview20261028RemStreamTruncationWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		chars            int
		truncated        bool
	}{
		{"separate finish", remSSEChunk("决策", "", "") + remSSEChunk("", "", "length") + "data: [DONE]\n", "决策", 2, true},
		{"same chunk finish", remSSEChunk("决策[", "", "length"), "决策[", 3, true},
		{"reasoning fallback", remSSEChunk("", "分析中", "length"), "分析中", 3, true},
		{"reasoning and content", remSSEChunk("", "分析", "") + remSSEChunk("[]", "", "length"), "[]", 4, true},
		{"same chunk reasoning and content", remSSEChunk("[]", "分析", "length"), "[]", 4, true},
		{"empty truncated", remSSEChunk("", "", "length"), "", 0, true},
		{"duplicate finish", remSSEChunk("ok", "", "length") + remSSEChunk("", "", "length"), "ok", 2, true},
		{"normal stop", remSSEChunk("正常", "", "stop"), "正常", 2, false},
		{"no finish metadata", remSSEChunk("ok", "", ""), "ok", 2, false},
	} {
		for _, requestStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/request=%v", tc.name, requestStream), func(t *testing.T) {
				transport := NewMockHTTPClient()
				transport.Response = tc.body
				log := NewMockLogger()
				c := NewClient(WithProvider(ProviderCustom), WithModel("default-model"),
					WithAPIKey("test-key"), WithHTTPClient(transport.ToHTTPClient()), WithLogger(log)).(*Client)
				model := c.Model
				var got string
				var err error
				var chunks []string
				if requestStream {
					model = "request-model"
					got, err = c.CallWithRequestStream(&Request{Model: model}, func(chunk string) { chunks = append(chunks, chunk) })
				} else {
					got, err = c.callStreamSingle("system", "user", time.Second)
				}
				calls := len(transport.GetRequests())
				if err != nil || got != tc.want || calls != 1 {
					t.Fatalf("return/request semantics changed: got=%q err=%v calls=%d", got, err, calls)
				}
				if requestStream && tc.want != "" && !strings.Contains(tc.name, "reasoning fallback") && len(chunks) != 1 {
					t.Fatalf("content callbacks changed: %v", chunks)
				}
				warnings := log.GetLogsByLevel("WARN")
				if !tc.truncated {
					if len(warnings) != 0 {
						t.Fatalf("normal completion warned: %+v", warnings)
					}
					return
				}
				if len(warnings) != 1 {
					t.Fatalf("want one truncation warning, got %+v", warnings)
				}
				for _, want := range []string{model, "Response TRUNCATED by max_tokens", "finish_reason=length", fmt.Sprintf("%d chars received", tc.chars)} {
					if !strings.Contains(warnings[0].Message, want) {
						t.Errorf("warning missing %q: %s", want, warnings[0].Message)
					}
				}
			})
		}
	}
}

type remStreamErrorReader struct{}

func (remStreamErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReview20261028RemTruncatedStreamStillReturnsReadError(t *testing.T) {
	body := "data: {\"model\":\"stream-model\"}\n\n" + remSSEChunk("部", "", "") + remSSEChunk("分", "", "length")
	var chunks, warnings []string
	lines := 0
	got, err := parseSSEStream(io.MultiReader(strings.NewReader(body), remStreamErrorReader{}),
		func(s string) { chunks = append(chunks, s) }, func() { lines++ },
		func(model string, chars int) { warnings = append(warnings, fmt.Sprintf("%s:%d", model, chars)) })
	if got != "部分" || !errors.Is(err, io.ErrUnexpectedEOF) || lines != 6 ||
		!reflect.DeepEqual(chunks, []string{"部", "部分"}) || !reflect.DeepEqual(warnings, []string{"stream-model:2"}) {
		t.Fatalf("got=%q err=%v lines=%d chunks=%v warnings=%v", got, err, lines, chunks, warnings)
	}
}
