package store

import (
	"errors"
	"testing"

	"github.com/mattn/go-sqlite3"
)

// The realistic shapes lastError() produces: primary code plus an extended
// code for the busy variants that surface as "database is locked".
func TestIsSQLiteBusy(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"busy", sqlite3.Error{Code: sqlite3.ErrBusy}, true},
		{"busy snapshot", sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusySnapshot}, true},
		{"busy recovery", sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusyRecovery}, true},
		{"locked", sqlite3.Error{Code: sqlite3.ErrLocked}, true},
		{"constraint", sqlite3.Error{Code: sqlite3.ErrConstraint}, false},
		{"generic", errors.New("database is locked"), false},
	} {
		if got := isSQLiteBusy(c.err); got != c.want {
			t.Errorf("%s: isSQLiteBusy=%v want %v", c.name, got, c.want)
		}
	}
}

func TestRetryOnceOnBusy(t *testing.T) {
	calls := 0
	if err := retryOnceOnBusy(func() error {
		calls++
		if calls == 1 {
			return sqlite3.Error{Code: sqlite3.ErrBusy}
		}
		return nil
	}); err != nil || calls != 2 {
		t.Fatalf("transient busy must succeed on retry: err=%v calls=%d", err, calls)
	}

	calls = 0
	sentinel := errors.New("boom")
	if err := retryOnceOnBusy(func() error {
		calls++
		return sentinel
	}); !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("non-busy must not be retried: err=%v calls=%d", err, calls)
	}

	calls = 0
	busy := sqlite3.Error{Code: sqlite3.ErrBusy}
	if err := retryOnceOnBusy(func() error {
		calls++
		return busy
	}); !isSQLiteBusy(err) || calls != 2 {
		t.Fatalf("persistent busy must surface after exactly one retry: err=%v calls=%d", err, calls)
	}
}
