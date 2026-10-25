package store

import (
	"fmt"
	"nofx/fundsledger"
	"time"
)

// FundsAccount loads only an owned trader and its owned exchange/strategy.
// It performs no exchange IO and does not create paper accounts or snapshots.
func (s *Store) FundsAccount(userID, traderID string, now time.Time) (fundsledger.AccountInput, string, error) {
	in := fundsledger.AccountInput{Kind: fundsledger.LiveFutures, Warnings: []string{}, Sources: []string{}}
	t, err := s.Trader().GetForUser(userID, traderID)
	if err != nil {
		return in, "", err
	}
	in.TraderID = t.ID
	in.Name = t.Name
	in.InitialBalance = t.InitialBalance
	var e Exchange
	ownedExchangeID := ""
	err = s.gdb.Select("id", "exchange_type", "name").Where("id = ? AND user_id = ?", t.ExchangeID, userID).First(&e).Error
	if err != nil {
		in.Warnings = append(in.Warnings, "交易所账户配置不可用，无法同步确认转账")
	} else {
		ownedExchangeID = e.ID
		in.Exchange = e.Name
		if in.Exchange == "" {
			in.Exchange = e.ExchangeType
		}
		if e.ExchangeType == "binance" {
			in.Exchange = "Binance"
		}
		if e.ExchangeType == "paper" {
			in.Kind = fundsledger.Paper
		}
	}
	stock := false
	if t.StrategyID != "" {
		strategy, e := s.Strategy().Get(userID, t.StrategyID)
		if e != nil {
			in.Warnings = append(in.Warnings, "策略配置不可用，无法确认是否为美股模拟盘")
			in.Kind = fundsledger.Paper
		} else {
			cfg, e := strategy.ParseConfig()
			if e != nil {
				in.Warnings = append(in.Warnings, "策略配置无法解析，账户按模拟盘排除实盘合计")
				in.Kind = fundsledger.Paper
			} else if cfg.StrategyType == StrategyTypeUSStock {
				stock = true
				if cfg.StockConfig.IsPaper() {
					in.Kind = fundsledger.Paper
				} else if in.Kind != fundsledger.Paper {
					in.Kind = fundsledger.LiveSpot
				}
			}
		}
	}
	snaps, err := s.Equity().GetByTimeRange(t.ID, time.Unix(0, 0), now)
	if err != nil {
		in.Warnings = append(in.Warnings, "权益快照读取失败")
	} else {
		for _, r := range snaps {
			in.Snapshots = append(in.Snapshots, fundsledger.Snapshot{Time: r.Timestamp, Equity: r.TotalEquity, Unrealized: r.UnrealizedPnL})
		}
	}
	if stock && in.Kind == fundsledger.Paper {
		var rows []StockPaperPosition
		err = s.gdb.Where("trader_id = ?", t.ID).Find(&rows).Error
		if err == nil {
			for _, r := range rows {
				at := r.ClosedAt
				if r.Status != StockPaperClosed {
					if r.RealizedPnL == 0 {
						continue
					}
					at = r.UpdatedAt
					in.Warnings = append(in.Warnings, "模拟美股部分卖出盈亏按持仓更新时间归日")
				}
				in.Trades = append(in.Trades, fundsledger.Trade{Time: time.UnixMilli(at).UTC(), PnL: r.RealizedPnL})
			}
			in.Sources = append(in.Sources, "stock_paper_positions")
		}
	} else {
		var rows []TraderPosition
		q := s.gdb.Select("status", "exit_time", "entry_time", "updated_at", "realized_pnl", "fee").Where("trader_id = ?", t.ID)
		if stock {
			q = q.Where("status = ? OR (status = ? AND (realized_pnl <> 0 OR fee <> 0))", "CLOSED", "OPEN")
		} else {
			q = q.Where("status = ?", "CLOSED")
		}
		err = q.Find(&rows).Error
		if err == nil {
			for _, r := range rows {
				at := r.ExitTime
				if r.Status == "OPEN" {
					at = r.UpdatedAt
					in.Warnings = append(in.Warnings, "美股未平仓行含累计部分卖出盈亏/手续费，按持仓更新时间归日（缺少逐笔时间）")
				}
				if at > 0 {
					in.Trades = append(in.Trades, fundsledger.Trade{Time: time.UnixMilli(at).UTC(), PnL: r.RealizedPnL, Fees: r.Fee})
				}
			}
		}
	}
	if err != nil {
		in.Warnings = append(in.Warnings, "交易盈亏记录读取失败")
	}
	if in.Kind != fundsledger.Paper && ownedExchangeID != "" {
		in.Income, err = s.AccountIncome().ListForKind(ownedExchangeID, in.Kind, 0, now.UnixMilli())
		if err != nil {
			in.Warnings = append(in.Warnings, "收支流水读取失败，转账回退到残差推断")
		}
	}
	return in, ownedExchangeID, nil
}

// FundsKind allows synchronization to skip simulation without loading secrets.
func (s *Store) FundsKind(userID, traderID string) (string, error) {
	t, err := s.Trader().GetForUser(userID, traderID)
	if err != nil {
		return "", err
	}
	var e Exchange
	if err = s.gdb.Select("exchange_type").Where("id = ? AND user_id = ?", t.ExchangeID, userID).First(&e).Error; err != nil {
		return "", err
	}
	if e.ExchangeType == "paper" {
		return fundsledger.Paper, nil
	}
	if t.StrategyID != "" {
		st, err := s.Strategy().Get(userID, t.StrategyID)
		if err != nil {
			return "", err
		}
		cfg, err := st.ParseConfig()
		if err != nil {
			return "", fmt.Errorf("parse strategy: %w", err)
		}
		if cfg.StrategyType == StrategyTypeUSStock {
			if cfg.StockConfig.IsPaper() {
				return fundsledger.Paper, nil
			}
			return fundsledger.LiveSpot, nil
		}
	}
	return fundsledger.LiveFutures, nil
}
