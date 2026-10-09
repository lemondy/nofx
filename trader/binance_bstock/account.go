package binance_bstock

import (
	"context"
	"fmt"
	"sort"

	binance "github.com/adshao/go-binance/v2"
)

func (t *BStockTrader) GetMarketPrice(symbol string) (float64, error) {
	if _, err := t.rule(symbol); err != nil {
		return 0, err
	}
	quotes, err := t.client.NewListPricesService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return 0, wrap("ticker/price", err)
	}
	for _, p := range quotes {
		if p.Symbol == symbol && finite(number(p.Price)) && number(p.Price) > 0 {
			return number(p.Price), nil
		}
	}
	return 0, fmt.Errorf("ticker/price: no valid price for %s", symbol)
}

func (t *BStockTrader) account() (*binance.Account, error) {
	a, err := t.client.NewGetAccountService().Do(context.Background())
	return a, wrap("spot account", err)
}
func holding(a *binance.Account, asset string) (free, total float64) {
	for _, b := range a.Balances {
		if b.Asset == asset {
			free = number(b.Free)
			return free, free + number(b.Locked)
		}
	}
	return 0, 0
}
func clone(m map[string]interface{}) map[string]interface{} {
	c := make(map[string]interface{}, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
func clonePositions(p []map[string]interface{}) []map[string]interface{} {
	c := make([]map[string]interface{}, len(p))
	for i, m := range p {
		c[i] = clone(m)
	}
	return c
}
func sortedSymbols(r map[string]binance.Symbol) []string {
	s := make([]string, 0, len(r))
	for symbol := range r {
		s = append(s, symbol)
	}
	sort.Strings(s)
	return s
}

func (t *BStockTrader) GetBalance() (map[string]interface{}, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.balance != nil && t.now().Sub(t.balanceTime) < cacheTTL {
		return clone(t.balance), nil
	}
	rules, err := t.symbols()
	if err != nil {
		return nil, err
	}
	a, err := t.account()
	if err != nil {
		return nil, err
	}
	available, equity := holding(a, "USDT")
	unrealized := 0.0
	for _, symbol := range sortedSymbols(rules) {
		_, qty := holding(a, rules[symbol].BaseAsset)
		if qty <= 0 {
			continue
		}
		mark, e := t.GetMarketPrice(symbol)
		if e != nil {
			return nil, e
		}
		entry, known, e := t.CostBasis(symbol)
		if e != nil {
			return nil, e
		}
		equity += qty * mark
		// Deposits/transfers without buy history have unknown cost. Do not
		// manufacture unrealized gains on those units.
		unrealized += min(qty, known) * (mark - entry)
	}
	t.balance = map[string]interface{}{"totalEquity": equity, "totalWalletBalance": equity - unrealized, "availableBalance": available, "totalUnrealizedProfit": unrealized}
	t.balanceTime = t.now()
	return clone(t.balance), nil
}

func (t *BStockTrader) GetPositions() ([]map[string]interface{}, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.positions != nil && t.now().Sub(t.positionsTime) < cacheTTL {
		return clonePositions(t.positions), nil
	}
	rules, err := t.symbols()
	if err != nil {
		return nil, err
	}
	a, err := t.account()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0)
	for _, symbol := range sortedSymbols(rules) {
		s := rules[symbol]
		_, qty := holding(a, s.BaseAsset)
		if qty <= field(filter(s, "LOT_SIZE"), "stepSize") {
			continue
		}
		entry, known, e := t.CostBasis(symbol)
		if e != nil {
			return nil, e
		}
		mark, e := t.GetMarketPrice(symbol)
		if e != nil {
			return nil, e
		}
		out = append(out, map[string]interface{}{"symbol": symbol, "side": "long", "positionAmt": qty, "entryPrice": entry, "markPrice": mark, "unRealizedProfit": min(qty, known) * (mark - entry), "leverage": 1.0, "liquidationPrice": 0.0})
	}
	t.positions, t.positionsTime = out, t.now()
	return clonePositions(out), nil
}
