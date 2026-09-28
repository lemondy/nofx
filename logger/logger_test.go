package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeLogMessageRedactsCredentials(t *testing.T) {
	input := `Post "https://api.telegram.org/bot123456:ABC_def/getUpdates?signature=deadbeef&limit=1"`
	got := sanitizeLogMessage(input)
	if strings.Contains(got, "123456:ABC_def") || strings.Contains(got, "deadbeef") {
		t.Fatalf("sensitive value remains in sanitized log: %s", got)
	}
	if !strings.Contains(got, "/bot<redacted>/") || !strings.Contains(got, "signature=<redacted>") {
		t.Fatalf("redaction markers missing: %s", got)
	}
}

func TestDailyFileWriterRotatesAtDateBoundary(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 27, 23, 59, 59, 0, time.Local)
	w := &dailyFileWriter{dir: dir, now: func() time.Time { return now }}
	if err := w.rotateLocked(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("day one\n")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if _, err := w.Write([]byte("day two\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	first, err := os.ReadFile(filepath.Join(dir, "nofx_2026-09-27.log"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "nofx_2026-09-28.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "day one\n" || string(second) != "day two\n" {
		t.Fatalf("unexpected rotated contents: first=%q second=%q", first, second)
	}
}
