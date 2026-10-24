package fundsledger

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Aggregate computes cumulative account amounts from all available history.
// Days limits the daily/flow presentation, never the lifetime transfer deduction:
// truncating transfers while retaining configured initial_balance invents returns.
// Callers supply Now and Location so this function is deterministic and IO-free.
func Aggregate(inputs []AccountInput, o Options) Overview {
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.Days <= 0 {
		o.Days = 90
	}
	today := midnight(o.Now, o.Location)
	cutoff := today.AddDate(0, 0, 1-o.Days).Format("2006-01-02")
	out := Overview{GeneratedAt: o.Now.UTC(), TZ: o.Location.String(), Days: o.Days,
		Accounts: []Account{}, Daily: []Daily{}, DailyTotal: []Daily{}, Flows: []Flow{}}
	totals := map[string]*Daily{}
	capital := 0.0
	for _, in := range inputs {
		if in.Kind == Paper && !o.IncludePaper {
			continue
		}
		a, days, flows, denominator := reconcile(in, o)
		out.Accounts = append(out.Accounts, a)
		if in.Kind != Paper && a.FirstDate != "" {
			addAmounts(&out.Totals, a.Amounts)
			capital += denominator
		}
		for _, d := range days {
			if d.Date < cutoff {
				continue
			}
			out.Daily = append(out.Daily, d)
			if in.Kind == Paper {
				continue
			}
			sum := totals[d.Date]
			if sum == nil {
				sum = &Daily{Date: d.Date}
				totals[d.Date] = sum
			}
			addDay(&sum.DayAmounts, d.DayAmounts)
		}
		for _, f := range flows {
			if f.Date >= cutoff {
				out.Flows = append(out.Flows, f)
			}
		}
	}
	finishReturns(&out.Totals, capital)
	for _, d := range totals {
		out.DailyTotal = append(out.DailyTotal, *d)
	}
	sort.Slice(out.Accounts, func(i, j int) bool {
		a, b := out.Accounts[i], out.Accounts[j]
		if a.Name == b.Name {
			return a.TraderID < b.TraderID
		}
		return a.Name < b.Name
	})
	sort.Slice(out.Daily, func(i, j int) bool {
		a, b := out.Daily[i], out.Daily[j]
		if a.Date != b.Date {
			return a.Date > b.Date
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.TraderID < b.TraderID
	})
	sort.Slice(out.DailyTotal, func(i, j int) bool { return out.DailyTotal[i].Date > out.DailyTotal[j].Date })
	sort.Slice(out.Flows, func(i, j int) bool {
		a, b := out.Flows[i], out.Flows[j]
		if a.Time.Equal(b.Time) {
			return a.ID < b.ID
		}
		return a.Time.After(b.Time)
	})
	return out
}

func midnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
func unique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}
func source(a, b string) string {
	if a == "" || a == "none" {
		if b == "" {
			return "none"
		}
		return b
	}
	if b == "" || b == "none" || a == b {
		return a
	}
	return "mixed"
}

type bucket struct {
	DayAmounts
	tradingIncome, feeIncome bool
	flows                    []Flow
}

func reconcile(in AccountInput, o Options) (Account, []Daily, []Flow, float64) {
	a := Account{TraderID: in.TraderID, Name: in.Name, Kind: in.Kind, Exchange: in.Exchange,
		Amounts: Amounts{InitialBalance: in.InitialBalance}, Sources: append([]string{}, in.Sources...), Warnings: append([]string{}, in.Warnings...)}
	a.Warnings = unique(a.Warnings, "金额按 USDT 计，USD/USDC 等稳定币按 1:1；非稳定币流水保留原币数量但不参与金额对账")
	snaps := make([]Snapshot, 0, len(in.Snapshots))
	for _, s := range in.Snapshots {
		if !s.Time.After(o.Now) {
			snaps = append(snaps, s)
		}
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.Before(snaps[j].Time) })
	if len(snaps) == 0 {
		a.Warnings = unique(a.Warnings, "没有权益快照，无法计算收益率；该账户暂不计入合计")
		return a, nil, nil, 0
	}
	a.Sources = unique(a.Sources, "equity_snapshots")
	first, last := snaps[0], snaps[len(snaps)-1]
	a.FirstDate = first.Time.In(o.Location).Format("2006-01-02")
	a.LastDate = last.Time.In(o.Location).Format("2006-01-02")
	a.CurrentEquity = last.Equity
	a.UnrealizedPnL = last.Unrealized
	if math.Abs(first.Equity-in.InitialBalance) > 0.01 {
		a.Warnings = unique(a.Warnings, "首条快照与初始资金存在差额；首条快照之前的变动无法对账")
	}
	if midnight(last.Time, o.Location).Before(midnight(o.Now, o.Location)) {
		a.Warnings = unique(a.Warnings, "最新权益快照不是今天，当前权益与收益截至最后快照")
	}
	byDate := map[string]Snapshot{}
	for _, s := range snaps {
		byDate[s.Time.In(o.Location).Format("2006-01-02")] = s
	}
	buckets := map[string]*bucket{}
	get := func(t time.Time) *bucket {
		key := t.In(o.Location).Format("2006-01-02")
		b := buckets[key]
		if b == nil {
			b = &bucket{}
			buckets[key] = b
		}
		return b
	}
	within := func(t time.Time) bool { return !t.Before(first.Time) && !t.After(last.Time) }
	seen := map[string]bool{}
	for _, r := range in.Income {
		t := time.UnixMilli(r.TimeMS).UTC()
		if !within(t) {
			continue
		}
		key := fmt.Sprintf("%s/%s/%d", r.TranID, r.IncomeType, r.TimeMS)
		if seen[key] {
			continue
		}
		seen[key] = true
		a.Sources = unique(a.Sources, "exchange_income")
		if !StableAsset(r.Asset) {
			a.Warnings = unique(a.Warnings, "跳过非稳定币流水："+r.Asset)
			continue
		}
		b := get(t)
		category := ClassifyIncome(r.IncomeType)
		switch category {
		case "transfer":
			if r.Amount == 0 {
				continue
			}
			typ := "transfer_in"
			if r.Amount < 0 {
				typ = "transfer_out"
			}
			switch strings.ToUpper(r.IncomeType) {
			case "DEPOSIT":
				typ = "deposit"
			case "WITHDRAW":
				typ = "withdraw"
			}
			b.NetTransfer += r.Amount
			b.TransferSource = source(b.TransferSource, "exchange")
			b.flows = append(b.flows, Flow{ID: in.TraderID + "/exchange/" + key, Time: t, Date: t.In(o.Location).Format("2006-01-02"), TraderID: in.TraderID, Name: in.Name, Amount: r.Amount, Type: typ, Source: "exchange", Confirmed: true, Note: r.Info})
		case "trading":
			b.TradingPnL += r.Amount
			b.tradingIncome = true
		case "fee":
			b.Fees -= r.Amount
			b.feeIncome = true
		case "funding":
			b.Funding += r.Amount
		default:
			b.OtherIncome += r.Amount
		}
	}
	for _, t := range in.Trades {
		if !within(t.Time) {
			continue
		}
		b := get(t.Time)
		if in.Kind != LiveFutures || !b.tradingIncome {
			b.TradingPnL += t.PnL
		}
		if in.Kind != LiveFutures || !b.feeIncome {
			b.Fees += t.Fees
		}
		if !contains(a.Sources, "stock_paper_positions") {
			a.Sources = unique(a.Sources, "trader_positions")
		}
	}
	days := []Daily{}
	flows := []Flow{}
	previous := first
	// Known cash/earnings during a snapshot gap must not be inferred a second
	// time when snapshots resume. Unexplained offsets keep each daily equation
	// and the cumulative equation intact while carried closes remain unchanged.
	gapExplained := 0.0
	gapDays := 0
	missing := false
	for day := midnight(first.Time, o.Location); !day.After(midnight(o.Now, o.Location)); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		b := buckets[date]
		if b == nil {
			b = &bucket{}
		}
		close, observed := byDate[date]
		if !observed {
			close = previous
			gapDays++
			missing = true
		}
		d := Daily{Date: date, TraderID: in.TraderID, Name: in.Name, DayAmounts: b.DayAmounts}
		d.OpenEquity = previous.Equity
		d.CloseEquity = close.Equity
		d.DeltaEquity = d.CloseEquity - d.OpenEquity
		d.UnrealizedChange = close.Unrealized - previous.Unrealized
		explained := d.TradingPnL - d.Fees + d.Funding + d.OtherIncome + d.UnrealizedChange
		residual := d.DeltaEquity - explained - d.NetTransfer
		d.Unexplained = residual
		threshold := math.Max(2, 0.02*d.OpenEquity)
		if observed {
			remaining := residual - gapExplained
			if math.Abs(remaining) > threshold {
				typ := "inferred_in"
				if remaining < 0 {
					typ = "inferred_out"
				}
				f := Flow{ID: in.TraderID + "/inferred/" + date, Time: close.Time.UTC(), Date: date, TraderID: in.TraderID, Name: in.Name, Amount: remaining, Type: typ, Source: "inferred", Note: fmt.Sprintf("对账残差 %.8f（阈值 %.8f）；时间取当日最后快照", remaining, threshold)}
				if gapDays > 0 {
					f.Note += "；跨缺快照日累计残差，实际发生日期不确定"
				}
				b.flows = append(b.flows, f)
				d.NetTransfer += remaining
				d.TransferSource = source(d.TransferSource, "inferred")
				d.Unexplained -= remaining
			}
			gapExplained = 0
			gapDays = 0
		} else {
			gapExplained += explained + d.NetTransfer
		}
		if d.TransferSource == "" {
			d.TransferSource = "none"
		}
		a.NetTransfers += d.NetTransfer
		a.TradingPnL += d.TradingPnL
		a.Fees += d.Fees
		a.Funding += d.Funding
		a.OtherIncome += d.OtherIncome
		a.Unexplained += d.Unexplained
		days = append(days, d)
		flows = append(flows, b.flows...)
		previous = close
	}
	if missing {
		a.Warnings = unique(a.Warnings, "部分日期缺少快照：沿用上一收盘权益，仅在快照恢复后推断剩余转账")
	}
	capital := in.InitialBalance
	duration := last.Time.Sub(first.Time).Seconds()
	for _, f := range flows {
		if duration > 0 {
			capital += f.Amount * last.Time.Sub(f.Time).Seconds() / duration
		}
	}
	inferred := false
	for _, f := range flows {
		if !f.Confirmed {
			inferred = true
		}
	}
	if inferred {
		a.Warnings = unique(a.Warnings, "推断转账仅按每日残差识别；缺失成交或估值误差也可能形成残差")
	}
	if capital <= 0 {
		a.Warnings = unique(a.Warnings, "Modified Dietz 加权本金非正，收益率返回 0")
	}
	finishReturns(&a.Amounts, capital)
	return a, days, flows, capital
}
func finishReturns(a *Amounts, capital float64) {
	a.AdjustedPnL = a.CurrentEquity - a.InitialBalance - a.NetTransfers
	if a.InitialBalance > 0 {
		a.RawReturnPct = (a.CurrentEquity/a.InitialBalance - 1) * 100
	}
	if capital > 0 {
		a.AdjustedReturnPct = a.AdjustedPnL / capital * 100
	}
}
func addAmounts(a *Amounts, b Amounts) {
	a.InitialBalance += b.InitialBalance
	a.CurrentEquity += b.CurrentEquity
	a.UnrealizedPnL += b.UnrealizedPnL
	a.NetTransfers += b.NetTransfers
	a.TradingPnL += b.TradingPnL
	a.Fees += b.Fees
	a.Funding += b.Funding
	a.OtherIncome += b.OtherIncome
	a.Unexplained += b.Unexplained
}
func addDay(a *DayAmounts, b DayAmounts) {
	a.OpenEquity += b.OpenEquity
	a.CloseEquity += b.CloseEquity
	a.DeltaEquity += b.DeltaEquity
	a.TradingPnL += b.TradingPnL
	a.Fees += b.Fees
	a.Funding += b.Funding
	a.OtherIncome += b.OtherIncome
	a.UnrealizedChange += b.UnrealizedChange
	a.NetTransfer += b.NetTransfer
	a.Unexplained += b.Unexplained
	a.TransferSource = source(a.TransferSource, b.TransferSource)
}

func contains(xs []string, value string) bool {
	for _, x := range xs {
		if x == value {
			return true
		}
	}
	return false
}
