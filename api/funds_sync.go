package api

import (
	"context"
	"errors"
	"github.com/adshao/go-binance/v2/common"
	"nofx/fundsledger"
	"nofx/logger"
	"nofx/trader/types"
	"sync"
	"time"
)

type fundsSyncState struct {
	mu        sync.Mutex
	last      time.Time
	warnings  map[string][]string
	retryFrom map[string]time.Time
}

type fundsSyncAccount struct {
	exchangeID string
	traders    map[string]string
}

func (s *Server) fundsSyncEntry(key string) *fundsSyncState {
	s.fundsSyncMu.Lock()
	defer s.fundsSyncMu.Unlock()
	if s.fundsSync == nil {
		s.fundsSync = map[string]*fundsSyncState{}
	}
	e := s.fundsSync[key]
	if e == nil {
		e = &fundsSyncState{warnings: map[string][]string{}, retryFrom: map[string]time.Time{}}
		s.fundsSync[key] = e
	}
	return e
}

// One mutex/throttle per owned exchange account, including both its spot and
// futures wallets. Only existing manager instances are used: loading traders
// here could set position mode or start unrelated exchange operations.
func (s *Server) syncFunds(ctx context.Context, userID string, account fundsSyncAccount, now time.Time) map[string][]string {
	e := s.fundsSyncEntry(userID + "/" + account.exchangeID)
	return runFundsSync(e, account, now, func(kind, traderID string) []string {
		return s.syncFundsKind(ctx, account.exchangeID, kind, traderID, now, e)
	})
}

func runFundsSync(e *fundsSyncState, account fundsSyncAccount, now time.Time, read func(string, string) []string) map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.last.IsZero() && now.Sub(e.last) < 15*time.Minute {
		return copyFundsWarnings(e.warnings)
	}
	e.last = now
	for kind, traderID := range account.traders {
		e.warnings[kind] = read(kind, traderID)
	}
	// Return a copy; the next request can update the cached map concurrently.
	return copyFundsWarnings(e.warnings)
}

func (s *Server) syncFundsKind(ctx context.Context, exchangeID, kind, traderID string, now time.Time, state *fundsSyncState) []string {
	if s.traderManager == nil {
		return []string{"交易员实例未加载，无法读取交易所流水；转账回退到残差推断"}
	}
	at, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		return []string{"交易员实例未加载，无法读取交易所流水；转账回退到残差推断"}
	}
	reader, ok := at.GetUnderlyingTrader().(types.IncomeHistoryReader)
	if !ok {
		return []string{"交易所不支持读取收支/充提/划转历史；转账回退到残差推断"}
	}
	return s.readFundsIncome(ctx, exchangeID, kind, now, state, reader)
}

func (s *Server) readFundsIncome(ctx context.Context, exchangeID, kind string, now time.Time, state *fundsSyncState, reader types.IncomeHistoryReader) []string {
	latest, err := s.store.AccountIncome().LatestTimeForKind(exchangeID, kind)
	if err != nil {
		return []string{"收支流水同步游标读取失败，转账回退到残差推断"}
	}
	floor := now.AddDate(0, 0, -90)
	start := floor
	if latest > 0 {
		start = time.UnixMilli(latest).Add(-time.Hour)
		if start.Before(floor) {
			start = floor
		}
	}
	if retry := state.retryFrom[kind]; !retry.IsZero() && retry.Before(start) {
		start = retry
		if start.Before(floor) {
			start = floor
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	records, readErr := reader.ReadIncomeHistory(readCtx, start, now)
	saveErr := s.store.AccountIncome().SaveForKind(exchangeID, kind, records)
	warnings := []string{}
	if kind == fundsledger.LiveFutures {
		warnings = append(warnings, "币安合约流水仅覆盖最近约三个月；更早历史按残差推断")
	}
	if err := errors.Join(readErr, saveErr); err != nil {
		logger.Warnf("Funds income sync incomplete (%s): %v", kind, err)
		state.retryFrom[kind] = start
		warning := "交易所收支历史同步失败或不完整，未覆盖部分按残差推断"
		if kind == fundsledger.LiveSpot {
			warning = "现货充值/提现/划转历史不可用或部分缺失（接口/权限/数据限制），未覆盖部分按残差推断"
		}
		var apiErr *common.APIError
		if errors.As(readErr, &apiErr) && (apiErr.Code == -2015 || apiErr.Code == -2014) {
			warning += "；币安拒绝 API Key、IP 白名单或历史读取权限"
		}
		if errors.Is(readErr, context.DeadlineExceeded) {
			warning += "；读取超时"
		}
		warnings = append(warnings, warning)
	} else {
		delete(state.retryFrom, kind)
	}
	return warnings
}

func copyFundsWarnings(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range in {
		out[k] = append([]string{}, v...)
	}
	return out
}
