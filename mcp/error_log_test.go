package mcp

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCallWithMessages_LogsErrorsAtErrorLevel(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		mockLogger := NewMockLogger()
		client := NewClient(WithLogger(mockLogger))

		if _, err := client.CallWithMessages("system", "user"); err == nil {
			t.Fatal("expected error for missing API key")
		}

		errLogs := mockLogger.GetLogsByLevel("ERROR")
		if len(errLogs) != 1 || !strings.Contains(errLogs[0].Message, "AI API key not set") {
			t.Fatalf("expected 1 ERROR log for missing API key, got %+v", errLogs)
		}
	})

	t.Run("non-retryable 401 error", func(t *testing.T) {
		mockHTTP := NewMockHTTPClient()
		mockHTTP.SetErrorResponse(http.StatusUnauthorized, `{"error":"invalid key"}`)
		mockLogger := NewMockLogger()

		client := NewClient(
			WithHTTPClient(mockHTTP.ToHTTPClient()),
			WithLogger(mockLogger),
			WithAPIKey("sk-test-key"),
			WithStreamDecisions(false),
		)

		if _, err := client.CallWithMessages("system", "user"); err == nil {
			t.Fatal("expected 401 error")
		}

		errLogs := mockLogger.GetLogsByLevel("ERROR")
		if len(errLogs) != 1 || !strings.Contains(errLogs[0].Message, "non-retryable") || !strings.Contains(errLogs[0].Message, "401") {
			t.Fatalf("expected 1 ERROR log for non-retryable 401, got %+v", errLogs)
		}
	})

	t.Run("retryable error exhausted", func(t *testing.T) {
		mockHTTP := NewMockHTTPClient()
		mockHTTP.ResponseFunc = func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset")
		}
		mockLogger := NewMockLogger()

		client := NewClient(
			WithHTTPClient(mockHTTP.ToHTTPClient()),
			WithLogger(mockLogger),
			WithAPIKey("sk-test-key"),
			WithMaxRetries(2),
			WithRetryWaitBase(time.Nanosecond),
			WithStreamDecisions(false),
		)

		if _, err := client.CallWithMessages("system", "user"); err == nil {
			t.Fatal("expected error after retries exhausted")
		}

		errLogs := mockLogger.GetLogsByLevel("ERROR")
		if len(errLogs) != 1 || !strings.Contains(errLogs[0].Message, "still failed after 2 retries") {
			t.Fatalf("expected 1 ERROR log after retries exhausted, got %+v", errLogs)
		}
	})
}
