package store

import (
	"errors"
	"time"

	"github.com/mattn/go-sqlite3"
)

// isSQLiteBusy reports whether err is SQLITE_BUSY/SQLITE_LOCKED class. In WAL
// mode this fires when the busy_timeout budget ran out against the single
// writer, or on a stale-snapshot upgrade (BUSY_SNAPSHOT) that no amount of
// waiting can ever satisfy — both surface as "database is locked".
func isSQLiteBusy(err error) bool {
	var serr sqlite3.Error
	if errors.As(err, &serr) {
		return serr.Code == sqlite3.ErrBusy || serr.Code == sqlite3.ErrLocked
	}
	return false
}

// retryOnceOnBusy reruns fn once after a short pause when the first attempt
// failed with SQLITE_BUSY. Crypto cycles are aligned to 5-minute boundaries,
// so their ~150KB decision-row commits arrive in bursts against the same
// single-writer SQLite; a transient busy on the us-stock booking path must not
// surface as a protection-reconciliation ALERT. fn must be replay-safe: every
// caller books cumulative deltas against committed watermarks, so a rerun
// after a lost response books nothing twice.
func retryOnceOnBusy(fn func() error) error {
	err := fn()
	if err == nil || !isSQLiteBusy(err) {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	return fn()
}
