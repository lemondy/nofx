package store

import (
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ============================================================================
// us_stock (design 2026-10-09 §6 paper mode): the paper ledger. Paper trades
// run the full data/decision/risk path but place no orders, so they live in
// their own tables — they must NOT appear in trader_positions / trade_journal
// (those feed the live statistics, review and version-effect pages).
// ============================================================================

// DefaultStockPaperCash is the starting paper equity (USDT) when the trader
// has no explicit initial balance.
const DefaultStockPaperCash = 10000.0

const (
	StockPaperOpen   = "OPEN"
	StockPaperClosed = "CLOSED"
	StockPaperLong   = "LONG"
	StockPaperShort  = "SHORT"

	// stockPaperQtyEpsilon: quantities at or below this count as flat.
	stockPaperQtyEpsilon = 1e-9
)

// StockPaperAccount is the per-trader paper cash balance.
type StockPaperAccount struct {
	TraderID    string  `gorm:"column:trader_id;primaryKey" json:"trader_id"`
	Cash        float64 `gorm:"column:cash;not null;default:0" json:"cash"`
	InitialCash float64 `gorm:"column:initial_cash;not null;default:0" json:"initial_cash"`
	CreatedAt   int64   `gorm:"column:created_at" json:"created_at"` // Unix ms UTC
	UpdatedAt   int64   `gorm:"column:updated_at" json:"updated_at"`
}

func (StockPaperAccount) TableName() string { return "stock_paper_accounts" }

// StockPaperPosition is one simulated holding of a bStock pair. One OPEN
// row per (trader, symbol); a CLOSED row is history.
type StockPaperPosition struct {
	ID              int64   `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID        string  `gorm:"column:trader_id;not null;index:idx_stock_paper_trader_status" json:"trader_id"`
	Symbol          string  `gorm:"column:symbol;not null" json:"symbol"`
	Side            string  `gorm:"column:side;not null;default:LONG" json:"side"`
	Funding         float64 `gorm:"column:funding;not null;default:0" json:"funding"`
	LastFundingTime int64   `gorm:"column:last_funding_time;not null;default:0" json:"last_funding_time"`
	Quantity        float64 `gorm:"column:quantity;not null" json:"quantity"` // currently held
	EntryQty        float64 `gorm:"column:entry_quantity;default:0" json:"entry_quantity"`
	AvgPrice        float64 `gorm:"column:avg_price;not null" json:"avg_price"`
	InitialStop     float64 `gorm:"column:initial_stop;default:0" json:"initial_stop"` // write-once opening-risk stop
	Stop            float64 `gorm:"column:stop;default:0" json:"stop"`                 // live protective stop
	TakeProfit      float64 `gorm:"column:take_profit;default:0" json:"take_profit"`
	OpenedAt        int64   `gorm:"column:opened_at" json:"opened_at"`
	UpdatedAt       int64   `gorm:"column:updated_at" json:"updated_at"`
	Status          string  `gorm:"column:status;default:OPEN;index:idx_stock_paper_trader_status" json:"status"`
	// Exit legs accumulate: ExitPrice is the quantity-weighted average of
	// every sell leg (ExitedQty / ExitValue are its working sums).
	ExitedQty   float64 `gorm:"column:exited_qty;default:0" json:"exited_qty"`
	ExitValue   float64 `gorm:"column:exit_value;default:0" json:"exit_value"`
	ExitPrice   float64 `gorm:"column:exit_price;default:0" json:"exit_price"`
	RealizedPnL float64 `gorm:"column:realized_pnl;default:0" json:"realized_pnl"`
	ClosedAt    int64   `gorm:"column:closed_at;default:0" json:"closed_at"`
	CloseReason string  `gorm:"column:close_reason;default:''" json:"close_reason"`
	// Fee is the cumulative trading fee in USDT.
	Fee float64 `gorm:"column:fee;default:0" json:"fee"`
}

func (StockPaperPosition) TableName() string { return "stock_paper_positions" }

// EffectiveSide treats pre-migration rows as longs.
func (p *StockPaperPosition) EffectiveSide() string {
	if p.Side == StockPaperShort {
		return StockPaperShort
	}
	return StockPaperLong
}

func (p *StockPaperPosition) MarketValue(price float64) float64 {
	value := p.Quantity * price
	if p.EffectiveSide() == StockPaperShort {
		return -value
	}
	return value
}

func paperFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// ApplyShortFunding atomically advances the settlement watermark, position
// funding and cash. Replaying an event after a restart is a no-op.
func (s *StockPaperStore) ApplyShortFunding(id int64, settlement time.Time, rate, mark float64) error {
	if !paperFinite(rate) || !paperFinite(mark) || mark <= 0 {
		return fmt.Errorf("invalid funding settlement")
	}
	return retryOnceOnBusy(func() error {
		return s.db.Transaction(func(tx *gorm.DB) error {
			var p StockPaperPosition
			if err := tx.First(&p, id).Error; err != nil {
				return err
			}
			ms := settlement.UnixMilli()
			if p.Status != StockPaperOpen || p.EffectiveSide() != StockPaperShort || ms <= p.LastFundingTime || ms <= p.OpenedAt {
				return nil
			}
			amount := rate * p.Quantity * mark
			res := tx.Model(&StockPaperPosition{}).Where("id = ? AND last_funding_time = ?", id, p.LastFundingTime).Updates(map[string]interface{}{
				"funding": gorm.Expr("funding + ?", amount), "realized_pnl": gorm.Expr("realized_pnl + ?", amount), "last_funding_time": ms,
			})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return nil
			}
			return tx.Model(&StockPaperAccount{}).Where("trader_id = ?", p.TraderID).Update("cash", gorm.Expr("cash + ?", amount)).Error
		})
	})
}

// StockPendingEntry is a resting limit-entry order of a live us_stock trader,
// persisted so a restart can still finish the fill (protection + position row).
// Own table on purpose: trader_pending_entries is read by the futures
// account-level reservation code.
type StockPendingEntry struct {
	ID        int64   `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID  string  `gorm:"column:trader_id;not null;index:idx_stock_pending_trader_symbol,unique" json:"trader_id"`
	Symbol    string  `gorm:"column:symbol;not null;index:idx_stock_pending_trader_symbol,unique" json:"symbol"`
	OrderID   string  `gorm:"column:order_id;not null" json:"order_id"`
	Action    string  `gorm:"column:action" json:"action"` // open_long | add_long
	Quantity  float64 `gorm:"column:quantity" json:"quantity"`
	FilledQty float64 `gorm:"column:filled_qty;default:0" json:"filled_qty"` // already booked into the position row
	// Cumulative quote proceeds / fee (USDT) already booked with FilledQty, so
	// the next increment is priced from the DELTAS, not the order's running average.
	FilledQuote float64 `gorm:"column:filled_quote;default:0" json:"filled_quote"`
	FilledFee   float64 `gorm:"column:filled_fee;default:0" json:"filled_fee"`
	LimitPrice  float64 `gorm:"column:limit_price" json:"limit_price"`
	Stop        float64 `gorm:"column:stop" json:"stop"`
	TakeProfit  float64 `gorm:"column:take_profit" json:"take_profit"`
	PlacedAt    int64   `gorm:"column:placed_at" json:"placed_at"`
}

func (StockPendingEntry) TableName() string { return "stock_pending_entries" }

// StockPaperStore persists the paper ledger and live resting-entry state.
type StockPaperStore struct {
	db *gorm.DB
}

func NewStockPaperStore(db *gorm.DB) *StockPaperStore { return &StockPaperStore{db: db} }

func (s *StockPaperStore) initTables() error {
	return s.db.AutoMigrate(&StockPaperAccount{}, &StockPaperPosition{}, &StockPendingEntry{}, &StockProtectionOrder{}, &StockProtectionIntent{}, &StockWeekHalt{})
}

// EnsureAccount returns the trader's paper account, creating it with
// startCash (DefaultStockPaperCash when <= 0) on first use.
func (s *StockPaperStore) EnsureAccount(traderID string, startCash float64) (*StockPaperAccount, error) {
	if startCash <= 0 || math.IsNaN(startCash) || math.IsInf(startCash, 0) {
		startCash = DefaultStockPaperCash
	}
	now := time.Now().UTC().UnixMilli()
	acct := &StockPaperAccount{TraderID: traderID, Cash: startCash, InitialCash: startCash, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(acct).Error; err != nil {
		return nil, err
	}
	return s.GetAccount(traderID)
}

// GetAccount returns the paper account, or (nil, nil) when none exists yet.
func (s *StockPaperStore) GetAccount(traderID string) (*StockPaperAccount, error) {
	var acct StockPaperAccount
	err := s.db.Where("trader_id = ?", traderID).First(&acct).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &acct, nil
}

// OpenOrAdd books a paper buy of qty at price: cash decreases, and the symbol's
// OPEN row is created or merged (weighted average cost). stop / takeProfit
// replace the live protection; InitialStop is write-once. now stamps the row.
func (s *StockPaperStore) OpenOrAdd(traderID, symbol string, qty, price, stop, takeProfit float64, now time.Time) (*StockPaperPosition, error) {
	pos, _, err := s.OpenOrAddWithFee(traderID, symbol, qty, price, stop, takeProfit, 0, now)
	return pos, err
}

// OpenOrAddWithFee is OpenOrAdd that also charges a trading fee of feeBps basis
// points of the notional: cash decreases by cost+fee and the fee accumulates on
// the position row. It returns the fee charged. feeBps 0 behaves exactly like
// OpenOrAdd.
func (s *StockPaperStore) OpenOrAddWithFee(traderID, symbol string, qty, price, stop, takeProfit, feeBps float64, now time.Time) (*StockPaperPosition, float64, error) {
	return s.openOrAddSide(traderID, symbol, StockPaperLong, qty, price, stop, takeProfit, feeBps, now, false)
}

// OpenOrAddSideWithFee books cash and net realized fees for us_stock. Legacy
// wrappers retain factor_quant's gross-PnL convention.
func (s *StockPaperStore) OpenOrAddSideWithFee(traderID, symbol, side string, qty, price, stop, takeProfit, feeBps float64, now time.Time) (*StockPaperPosition, float64, error) {
	return s.openOrAddSide(traderID, symbol, side, qty, price, stop, takeProfit, feeBps, now, true)
}

func (s *StockPaperStore) openOrAddSide(traderID, symbol, side string, qty, price, stop, takeProfit, feeBps float64, now time.Time, net bool) (*StockPaperPosition, float64, error) {
	if side != StockPaperLong && side != StockPaperShort {
		return nil, 0, fmt.Errorf("invalid paper side %q", side)
	}
	if !paperFinite(qty) || !paperFinite(price) || !paperFinite(feeBps) {
		return nil, 0, fmt.Errorf("non-finite paper fill")
	}

	if qty <= 0 || price <= 0 {
		return nil, 0, fmt.Errorf("paper buy needs positive qty and price (qty=%v price=%v)", qty, price)
	}
	if feeBps < 0 {
		feeBps = 0
	}
	nowMs := now.UTC().UnixMilli()
	var out StockPaperPosition
	cost := qty * price
	fee := cost * feeBps / 10000
	cashDelta := -cost - fee
	if side == StockPaperShort {
		cashDelta = cost - fee
	}
	// A transient SQLITE_BUSY is retried once. Replay-safe: a busy error means
	// the transaction rolled back (with _txlock=immediate it fires at BEGIN),
	// and out is reset so the retry's First() is not keyed by a stale id.
	err := retryOnceOnBusy(func() error {
		out = StockPaperPosition{}
		return s.db.Transaction(func(tx *gorm.DB) error {
			res := tx.Model(&StockPaperAccount{}).Where("trader_id = ?", traderID).Updates(map[string]interface{}{
				"cash": gorm.Expr("cash + ?", cashDelta), "updated_at": nowMs})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return fmt.Errorf("paper account for %s does not exist", traderID)
			}
			err := tx.Where("trader_id = ? AND symbol = ? AND status = ?", traderID, symbol, StockPaperOpen).First(&out).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				out = StockPaperPosition{TraderID: traderID, Symbol: symbol, Side: side, LastFundingTime: nowMs, Quantity: qty, EntryQty: qty, AvgPrice: price,
					InitialStop: stop, Stop: stop, TakeProfit: takeProfit, OpenedAt: nowMs, UpdatedAt: nowMs, Status: StockPaperOpen, Fee: fee}
				if net {
					out.RealizedPnL = -fee
				}
				return tx.Create(&out).Error
			}
			if err != nil {
				return err
			}
			if out.EffectiveSide() != side {
				return fmt.Errorf("OPPOSITE_HELD: %s", symbol)
			}
			newQty := out.Quantity + qty
			out.AvgPrice = (out.AvgPrice*out.Quantity + price*qty) / newQty
			out.Quantity, out.EntryQty = newQty, out.EntryQty+qty
			if out.InitialStop <= 0 {
				out.InitialStop = stop
			}
			if stop > 0 {
				out.Stop = stop
			}
			out.TakeProfit = takeProfit
			out.UpdatedAt = nowMs
			out.Fee += fee
			if net {
				out.RealizedPnL -= fee
			}
			return tx.Save(&out).Error
		})
	})
	if err != nil {
		return nil, 0, err
	}
	return &out, fee, nil
}

// Reduce books a paper sell of qty at price (qty >= held, or <= 0, sells all).
// Returns the leg's realized PnL and whether the position is now closed.
func (s *StockPaperStore) Reduce(traderID, symbol string, qty, price float64, now time.Time, reason string) (float64, bool, error) {
	pnl, closed, _, err := s.ReduceWithFee(traderID, symbol, qty, price, 0, now, reason)
	return pnl, closed, err
}

// ReduceWithFee is Reduce that also charges a trading fee of feeBps basis
// points of the sale proceeds: cash increases by proceeds-fee and the fee
// accumulates on the position row. The leg's realized PnL stays gross (like
// the crypto paper ledger, fees are tracked separately). It returns the fee
// charged. feeBps 0 behaves exactly like Reduce.
func (s *StockPaperStore) ReduceWithFee(traderID, symbol string, qty, price, feeBps float64, now time.Time, reason string) (float64, bool, float64, error) {
	return s.reduceSide(traderID, symbol, StockPaperLong, qty, price, feeBps, now, reason, false)
}

// ReduceSideWithFee books a long sale or short cover, net of the exit fee.
// Funding and opening fees already accumulated in RealizedPnL remain there.
func (s *StockPaperStore) ReduceSideWithFee(traderID, symbol, side string, qty, price, feeBps float64, now time.Time, reason string) (float64, bool, float64, error) {
	return s.reduceSide(traderID, symbol, side, qty, price, feeBps, now, reason, true)
}

func (s *StockPaperStore) reduceSide(traderID, symbol, side string, qty, price, feeBps float64, now time.Time, reason string, net bool) (float64, bool, float64, error) {
	if !paperFinite(qty) || !paperFinite(price) || !paperFinite(feeBps) {
		return 0, false, 0, fmt.Errorf("non-finite paper fill")
	}

	if price <= 0 {
		return 0, false, 0, fmt.Errorf("paper sell needs a positive price")
	}
	if feeBps < 0 {
		feeBps = 0
	}
	nowMs := now.UTC().UnixMilli()
	var pnl, fee float64
	closed := false
	// Retried once on SQLITE_BUSY (the transaction rolled back); per-attempt
	// state is reset and the requested qty is never mutated across attempts.
	err := retryOnceOnBusy(func() error {
		pnl, fee, closed = 0, 0, false
		return s.db.Transaction(func(tx *gorm.DB) error {
			var pos StockPaperPosition
			if err := tx.Where("trader_id = ? AND symbol = ? AND status = ?", traderID, symbol, StockPaperOpen).First(&pos).Error; err != nil {
				return err
			}
			if pos.EffectiveSide() != side {
				return fmt.Errorf("OPPOSITE_HELD: %s", symbol)
			}
			qty := qty
			if qty <= 0 || qty >= pos.Quantity-stockPaperQtyEpsilon {
				qty = pos.Quantity
			}
			pnl = (price - pos.AvgPrice) * qty
			if side == StockPaperShort {
				pnl = -pnl
			}
			fee = qty * price * feeBps / 10000
			if net {
				pnl -= fee
			}
			cashDelta := qty*price - fee
			if side == StockPaperShort {
				cashDelta = -qty*price - fee
			}
			pos.Fee += fee
			pos.Quantity -= qty
			pos.ExitedQty += qty
			pos.ExitValue += qty * price
			pos.ExitPrice = pos.ExitValue / pos.ExitedQty
			pos.RealizedPnL += pnl
			pos.UpdatedAt = nowMs
			if pos.Quantity <= stockPaperQtyEpsilon {
				pos.Quantity, pos.Status, pos.ClosedAt, pos.CloseReason = 0, StockPaperClosed, nowMs, reason
				closed = true
			}
			if err := tx.Save(&pos).Error; err != nil {
				return err
			}
			return tx.Model(&StockPaperAccount{}).Where("trader_id = ?", traderID).Updates(map[string]interface{}{
				"cash": gorm.Expr("cash + ?", cashDelta), "updated_at": nowMs}).Error
		})
	})
	return pnl, closed, fee, err
}

// SetProtection updates the live stop / take-profit of an OPEN paper position.
// takeProfit <= 0 clears the take-profit.
func (s *StockPaperStore) SetProtection(traderID, symbol string, stop, takeProfit float64, now time.Time) error {
	return s.db.Model(&StockPaperPosition{}).
		Where("trader_id = ? AND symbol = ? AND status = ?", traderID, symbol, StockPaperOpen).
		Updates(map[string]interface{}{"stop": stop, "take_profit": takeProfit, "updated_at": now.UTC().UnixMilli()}).Error
}

// ListOpen returns the trader's OPEN paper positions (oldest first).
func (s *StockPaperStore) ListOpen(traderID string) ([]*StockPaperPosition, error) {
	var rows []*StockPaperPosition
	err := s.db.Where("trader_id = ? AND status = ?", traderID, StockPaperOpen).Order("opened_at ASC").Find(&rows).Error
	return rows, err
}

// ListClosed returns the trader's most recent CLOSED paper positions.
func (s *StockPaperStore) ListClosed(traderID string, limit int) ([]*StockPaperPosition, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []*StockPaperPosition
	err := s.db.Where("trader_id = ? AND status = ?", traderID, StockPaperClosed).Order("closed_at DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// UpsertPending stores/refreshes the trader's resting limit entry for a symbol.
func (s *StockPaperStore) UpsertPending(p *StockPendingEntry) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "trader_id"}, {Name: "symbol"}},
		UpdateAll: true,
	}).Create(p).Error
}

// BookEntryFill books a live entry fill into the program-owned position row and,
// in the SAME transaction, advances the resting entry's fill watermark
// (pending, nil for an immediately complete fill) and records the "protection
// still to be established" intent (when intent). A crash or write failure can
// therefore neither lose the watermark nor book the same fill twice. fill
// carries the quantity/price/fee increment and the row template used when no
// owned row exists yet. booked=false means the watermark already covered this
// fill (idempotent replay): nothing was changed. A transient SQLITE_BUSY from
// an aligned crypto-cycle write burst is retried once.
func (s *StockPaperStore) BookEntryFill(fill *TraderPosition, pending *StockPendingEntry, intent bool) (booked bool, err error) {
	err = retryOnceOnBusy(func() error {
		var e error
		booked, e = s.bookEntryFillOnce(fill, pending, intent)
		return e
	})
	return booked, err
}

func (s *StockPaperStore) bookEntryFillOnce(fill *TraderPosition, pending *StockPendingEntry, intent bool) (booked bool, err error) {
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if pending != nil {
			var cur StockPendingEntry
			e := tx.Where("trader_id = ? AND symbol = ?", pending.TraderID, pending.Symbol).First(&cur).Error
			if e == nil && cur.OrderID == pending.OrderID && cur.FilledQty >= pending.FilledQty-1e-12 {
				return nil // already booked
			}
			if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
		}
		positions := NewPositionStore(tx)
		rows, e := positions.StockOwnedRows(fill.TraderID, fill.Symbol)
		if e != nil {
			return e
		}
		if len(rows) == 0 {
			row := *fill
			if e := positions.CreateOpenPosition(&row); e != nil {
				return e
			}
		} else if e := positions.UpdatePositionQuantityAndPrice(rows[0].ID, fill.Quantity, fill.EntryPrice, fill.Fee); e != nil {
			return e
		}
		if pending != nil {
			if e := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "trader_id"}, {Name: "symbol"}}, UpdateAll: true}).Create(pending).Error; e != nil {
				return e
			}
		}
		if intent {
			row := StockProtectionIntent{TraderID: fill.TraderID, Symbol: fill.Symbol, CreatedAt: time.Now().UTC().UnixMilli()}
			if e := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; e != nil {
				return e
			}
		}
		booked = true
		return nil
	})
	return booked, err
}

// DeletePending removes the resting-entry row (filled / cancelled).
func (s *StockPaperStore) DeletePending(traderID, symbol string) error {
	return s.db.Where("trader_id = ? AND symbol = ?", traderID, symbol).Delete(&StockPendingEntry{}).Error
}

// ListPending returns every resting entry of the trader.
func (s *StockPaperStore) ListPending(traderID string) ([]*StockPendingEntry, error) {
	var rows []*StockPendingEntry
	err := s.db.Where("trader_id = ?", traderID).Find(&rows).Error
	return rows, err
}
