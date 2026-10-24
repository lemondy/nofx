package types

import (
	"context"
	"errors"
	"nofx/fundsledger"
	"time"
)

var ErrIncomeHistoryUnavailable = errors.New("income history unavailable")

// IncomeHistoryReader is optional; the shared Trader interface is unchanged.
// Implementations call history GET endpoints only. On partial failure, records
// already read may accompany an error; callers must surface incomplete coverage.
type IncomeHistoryReader interface {
	ReadIncomeHistory(context.Context, time.Time, time.Time) ([]fundsledger.Income, error)
}
