package binance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
	"nofx/logger"
)

const (
	futuresV3RecheckInterval   = 6 * time.Hour
	futuresV3TransientCooldown = 5 * time.Minute
	futuresV3SemanticCooldown  = 6 * time.Hour
	futuresSymbolConfigTTL     = 5 * time.Minute
)

// Decisions are process-local, shared across trader recreation for the SAME
// endpoint and credentials. A cooldown must not affect other accounts/testnet,
// nor be bypassed by a config reload. Never persist credentials
// or account data. Separate locks keep account and position reads independent.
var futuresReadStates = struct {
	sync.Mutex
	entries map[[32]byte]*futuresReadState
}{entries: make(map[[32]byte]*futuresReadState)}

type futuresReadState struct {
	positionMu            sync.Mutex
	usePositionV3         bool
	positionDisabledUntil time.Time
	positionCheckedAt     time.Time
	accountMu             sync.Mutex
	useAccountV3          bool
	accountDisabledUntil  time.Time
	accountCheckedAt      time.Time
	configMu              sync.Mutex
	configs               map[string]cachedFuturesConfig
}

// The SDK discards HTTP status when constructing common.APIError. Preserve it
// for this request without changing the shared client's transport or copying
// its atomic counters. The configured proxy, meter and timeout stay in use.
type futuresReadTransport struct {
	base   http.RoundTripper
	client *futures.Client
	status atomic.Int64
}

func (t *futuresReadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if resp != nil {
		t.status.Store(int64(resp.StatusCode))
		t.client.UsedWeight.UpdateByHeader(resp.Header)
		t.client.OrderCount.UpdateByHeader(resp.Header)
	}
	return resp, err
}

func (t *FuturesTrader) v3ReadClient() (*futures.Client, *futuresReadTransport) {
	hc := *t.client.HTTPClient
	transport := &futuresReadTransport{base: hc.Transport, client: t.client}
	if transport.base == nil {
		transport.base = http.DefaultTransport
	}
	hc.Transport = transport
	client := &futures.Client{
		APIKey: t.client.APIKey, SecretKey: t.client.SecretKey, KeyType: t.client.KeyType,
		BaseURL: t.client.BaseURL, UserAgent: t.client.UserAgent, HTTPClient: &hc,
		Debug: t.client.Debug, Logger: t.client.Logger, TimeOffset: t.client.TimeOffset,
	}
	return client, transport
}

// Only classify request errors here. Validation/reconciliation failures always
// use the semantic cooldown; any unavailable leverage uses the transient one.
func futuresV3ErrorCooldown(err error, status int) time.Duration {
	if IsAuthOrIPError(err) || (status >= 500 && status < 600) {
		return futuresV3TransientCooldown
	}
	// A missing endpoint is semantic even if its body happens to use -1000.
	if status == http.StatusNotFound {
		return futuresV3SemanticCooldown
	}
	var apiErr *common.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case -1000, -1001, -1003, -1007, -1008:
			return futuresV3TransientCooldown
		default:
			return futuresV3SemanticCooldown
		}
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return futuresV3SemanticCooldown
	}
	return futuresV3TransientCooldown // network, timeout, EOF or proxy failure
}

func logFuturesV3Cooldown(endpoint string, duration time.Duration, until time.Time, err error) {
	// Called only when entering a cooldown, under the endpoint's state lock.
	if duration == futuresV3TransientCooldown {
		logger.Warnf("futures %s v3 cooling down for %s until %s; falling back to v2: %v", endpoint, duration, until.Format(time.RFC3339), err)
	} else {
		logger.Errorf("futures %s v3 cooling down for %s until %s; falling back to v2: %v", endpoint, duration, until.Format(time.RFC3339), err)
	}
}

func (s *futuresReadState) coolPositionV3(now time.Time, duration time.Duration, err error) {
	s.positionDisabledUntil = now.Add(duration)
	s.usePositionV3 = false
	logFuturesV3Cooldown("positionRisk", duration, s.positionDisabledUntil, err)
}

func (s *futuresReadState) coolAccountV3(now time.Time, duration time.Duration, err error) {
	s.accountDisabledUntil = now.Add(duration)
	s.useAccountV3 = false
	logFuturesV3Cooldown("account", duration, s.accountDisabledUntil, err)
}

// A successful v2 fallback means the account is readable, so ANY v3 error
// (including auth/IP — the proxy's exit IP can differ between two concurrent
// requests) is suppressed and the valid v2 data is returned: surfacing an auth
// error here would discard good data and falsely trip the trader's authBlocked.
// Only when v2 ALSO failed are both errors returned (auth/IP stays detectable
// by IsAuthOrIPError so the PR #26 recovery probe can act).
func futuresV3FallbackError(source string, v3err, v2err error) error {
	if v2err != nil {
		return errors.Join(WrapAuthError(source+" v3", v3err), WrapAuthError(source+" v2 fallback", v2err))
	}
	return nil
}

type cachedFuturesConfig struct {
	config futures.SymbolConfig
	at     time.Time
}

func (t *FuturesTrader) readState() *futuresReadState {
	key := sha256.Sum256([]byte(t.client.BaseURL + "\x00" + t.client.APIKey + "\x00" + t.client.SecretKey))
	futuresReadStates.Lock()
	defer futuresReadStates.Unlock()
	s := futuresReadStates.entries[key]
	if s == nil {
		s = &futuresReadState{configs: make(map[string]cachedFuturesConfig)}
		futuresReadStates.entries[key] = s
	}
	return s
}

func (t *FuturesTrader) symbolConfig(symbol string) (futures.SymbolConfig, error) {
	s := t.readState()
	s.configMu.Lock()
	defer s.configMu.Unlock()
	now := t.now()
	if c, ok := s.configs[symbol]; ok && now.Sub(c.at) >= 0 && now.Sub(c.at) < futuresSymbolConfigTTL {
		return c.config, nil
	}
	configs, err := t.client.NewGetSymbolConfigService().Symbol(symbol).Do(context.Background())
	if err != nil {
		return futures.SymbolConfig{}, WrapAuthError("GetPositions symbolConfig "+symbol, err)
	}
	var found *futures.SymbolConfig
	for _, c := range configs {
		if c != nil && c.Symbol == symbol {
			if found != nil || c.Leverage <= 0 || (!strings.EqualFold(c.MarginType, "CROSSED") && !strings.EqualFold(c.MarginType, "ISOLATED")) {
				return futures.SymbolConfig{}, fmt.Errorf("invalid/duplicate symbolConfig for %s: %+v", symbol, c)
			}
			found = c
		}
	}
	if found == nil {
		return futures.SymbolConfig{}, fmt.Errorf("symbolConfig missing leverage for %s", symbol)
	}
	s.configs[symbol] = cachedFuturesConfig{config: *found, at: now}
	return *found, nil
}

// Serialize with in-flight config reads so a pre-change fetch cannot republish
// stale leverage after the successful setter has returned.
func (t *FuturesTrader) invalidateSymbolConfig(symbol string) {
	s := t.readState()
	s.configMu.Lock()
	delete(s.configs, symbol)
	s.configMu.Unlock()
	// Also discard any output snapshot containing the old leverage. Do not hold
	// configMu here: GetPositions acquires positionsCacheMutex before configMu.
	t.positionsCacheMutex.Lock()
	t.cachedPositions = nil
	t.positionsCacheMutex.Unlock()
}

func finiteFuturesNumber(field, value string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid %s=%q", field, value)
	}
	return n, nil
}

func compareFuturesNumber(field, v2, v3 string, absolute, relative float64) error {
	a, err := finiteFuturesNumber("v2 "+field, v2)
	if err != nil {
		return err
	}
	b, err := finiteFuturesNumber("v3 "+field, v3)
	if err != nil {
		return err
	}
	tolerance := math.Max(absolute, relative*math.Max(math.Abs(a), math.Abs(b)))
	if math.Abs(a-b) > tolerance {
		return fmt.Errorf("%s mismatch: v2=%s v3=%s tolerance=%g", field, v2, v3, tolerance)
	}
	return nil
}

// Validate EVERY response, not only the six-hour comparisons. Missing numeric
// fields, null rows, duplicates and NaN must never silently become zero/flat.
func validatePositionsV3(positions []*futures.PositionRiskV3) error {
	if positions == nil {
		return fmt.Errorf("null v3 positionRisk response")
	}
	seen := make(map[string]bool)
	for _, p := range positions {
		if p == nil {
			return fmt.Errorf("null v3 positionRisk row")
		}
		amt, err := finiteFuturesNumber("positionAmt", p.PositionAmt)
		if err != nil {
			return err
		}
		if amt == 0 {
			continue
		}
		key := p.Symbol + "/" + p.PositionSide
		if p.Symbol == "" || (p.PositionSide != "LONG" && p.PositionSide != "SHORT" && p.PositionSide != "BOTH") || seen[key] {
			return fmt.Errorf("invalid/duplicate v3 position %s", key)
		}
		seen[key] = true
		for _, f := range []struct{ name, value string }{
			{"entryPrice", p.EntryPrice}, {"markPrice", p.MarkPrice},
			{"liquidationPrice", p.LiquidationPrice}, {"unRealizedProfit", p.UnRealizedProfit},
		} {
			if _, err := finiteFuturesNumber(key+" "+f.name, f.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func reconcilePositions(v2 []*futures.PositionRisk, v3 []*futures.PositionRiskV3) error {
	if err := validatePositionsV3(v3); err != nil {
		return err
	}
	if v2 == nil {
		return fmt.Errorf("null v2 positionRisk baseline")
	}
	baseline := make(map[string]*futures.PositionRisk)
	for _, p := range v2 {
		if p == nil {
			return fmt.Errorf("null v2 positionRisk row")
		}
		amt, err := finiteFuturesNumber("v2 positionAmt", p.PositionAmt)
		if err != nil {
			return err
		}
		if amt == 0 {
			continue
		}
		key := p.Symbol + "/" + p.PositionSide
		if baseline[key] != nil {
			return fmt.Errorf("duplicate v2 position %s", key)
		}
		baseline[key] = p
	}
	for _, b := range v3 {
		amt, _ := strconv.ParseFloat(b.PositionAmt, 64)
		if amt == 0 {
			continue
		}
		key := b.Symbol + "/" + b.PositionSide
		a := baseline[key]
		if a == nil {
			return fmt.Errorf("nonzero position set mismatch: v3-only %s", key)
		}
		for _, f := range []struct {
			name, a, b string
			abs, rel   float64
		}{
			{"positionAmt", a.PositionAmt, b.PositionAmt, 1e-10, 1e-9},
			{"entryPrice", a.EntryPrice, b.EntryPrice, 1e-8, 1e-8},
			{"markPrice", a.MarkPrice, b.MarkPrice, 1e-8, 0.005},
			{"liquidationPrice", a.LiquidationPrice, b.LiquidationPrice, 1e-8, 0.002},
		} {
			if err := compareFuturesNumber(key+" "+f.name, f.a, f.b, f.abs, f.rel); err != nil {
				return err
			}
		}
		// Allow the PnL change implied by the ACTUAL observed mark movement,
		// plus one cent/1% for rounding. Do not use the full allowed 0.5% of
		// notional: that could hide a large PnL error when marks are identical.
		mark2, _ := strconv.ParseFloat(a.MarkPrice, 64)
		mark3, _ := strconv.ParseFloat(b.MarkPrice, 64)
		pnlTolerance := 0.01 + math.Abs(amt*(mark3-mark2))
		if err := compareFuturesNumber(key+" unRealizedProfit", a.UnRealizedProfit, b.UnRealizedProfit, pnlTolerance, 0.01); err != nil {
			return err
		}
		delete(baseline, key)
	}
	for key := range baseline {
		return fmt.Errorf("nonzero position set mismatch: v2-only %s", key)
	}
	return nil
}

// Adapt into the old SDK shape so GetPositions' map, types, sign-based side,
// zero filtering, append order and cache publication remain unchanged.
func (t *FuturesTrader) enrichPositionsV3(v3 []*futures.PositionRiskV3) ([]*futures.PositionRisk, error) {
	positions := make([]*futures.PositionRisk, 0, len(v3))
	for _, p := range v3 {
		amt, _ := strconv.ParseFloat(p.PositionAmt, 64)
		if amt == 0 {
			continue
		}
		config, err := t.symbolConfig(p.Symbol)
		if err != nil {
			return nil, fmt.Errorf("%s leverage unavailable: %w", p.Symbol, err)
		}
		positions = append(positions, &futures.PositionRisk{
			Symbol: p.Symbol, PositionSide: p.PositionSide, PositionAmt: p.PositionAmt,
			EntryPrice: p.EntryPrice, MarkPrice: p.MarkPrice, UnRealizedProfit: p.UnRealizedProfit,
			LiquidationPrice: p.LiquidationPrice, Leverage: strconv.Itoa(config.Leverage), MarginType: config.MarginType,
		})
	}
	return positions, nil
}

func (t *FuturesTrader) fetchPositions() ([]*futures.PositionRisk, error) {
	s := t.readState()
	s.positionMu.Lock()
	defer s.positionMu.Unlock()
	if t.now().Before(s.positionDisabledUntil) {
		return t.client.NewGetPositionRiskService().Do(context.Background())
	}
	check := !s.usePositionV3 || t.now().Sub(s.positionCheckedAt) >= futuresV3RecheckInterval
	var v2 []*futures.PositionRisk
	var v2err error
	var v3 []*futures.PositionRiskV3
	var err error
	client, transport := t.v3ReadClient()
	if check {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); v2, v2err = t.client.NewGetPositionRiskService().Do(context.Background()) }()
		go func() { defer wg.Done(); v3, err = client.NewGetPositionRiskV3Service().Do(context.Background()) }()
		wg.Wait()
	} else {
		v3, err = client.NewGetPositionRiskV3Service().Do(context.Background())
	}
	cooldown := futuresV3SemanticCooldown
	if err != nil {
		cooldown = futuresV3ErrorCooldown(err, int(transport.status.Load()))
	}
	if err == nil {
		err = validatePositionsV3(v3)
	}
	if err == nil && check && v2err == nil {
		err = reconcilePositions(v2, v3)
	}
	if err != nil {
		s.coolPositionV3(t.now(), cooldown, err)
		if !check {
			v2, v2err = t.client.NewGetPositionRiskService().Do(context.Background())
		}
		if v2err != nil {
			return nil, futuresV3FallbackError("GetPositions", err, v2err)
		}
		return v2, futuresV3FallbackError("GetPositions", err, nil)
	}
	if check && v2err != nil {
		logger.Errorf("futures positionRisk v3 reconciliation unavailable; v2 baseline failed: %v", v2err)
		return nil, v2err
	}
	positions, configErr := t.enrichPositionsV3(v3)
	if configErr != nil {
		// Never emit a partial snapshot or repeat the failed config lookup on
		// every cache miss. Revalidate both sources after the cooldown.
		s.coolPositionV3(t.now(), futuresV3TransientCooldown, configErr)
		if !check {
			v2, v2err = t.client.NewGetPositionRiskService().Do(context.Background())
		}
		if v2err != nil {
			return nil, futuresV3FallbackError("GetPositions", configErr, v2err)
		}
		return v2, futuresV3FallbackError("GetPositions", configErr, nil)
	}
	if check {
		baseline := make(map[string]*futures.PositionRisk)
		for _, p := range v2 {
			baseline[p.Symbol+"/"+p.PositionSide] = p
		}
		for _, p := range positions {
			if err := compareFuturesNumber(p.Symbol+" leverage", baseline[p.Symbol+"/"+p.PositionSide].Leverage, p.Leverage, 0, 0); err != nil {
				s.coolPositionV3(t.now(), futuresV3SemanticCooldown, err)
				return v2, nil
			}
		}
		s.positionCheckedAt = t.now()
	}
	s.usePositionV3 = true
	s.positionDisabledUntil = time.Time{}
	return positions, nil
}

func validateAccountV3(a *futures.AccountV3) error {
	if a == nil {
		return fmt.Errorf("null v3 account response")
	}
	for _, f := range []struct{ name, value string }{
		{"totalWalletBalance", a.TotalWalletBalance}, {"availableBalance", a.AvailableBalance}, {"totalUnrealizedProfit", a.TotalUnrealizedProfit},
	} {
		if _, err := finiteFuturesNumber(f.name, f.value); err != nil {
			return err
		}
	}
	return nil
}

func reconcileAccount(a *futures.Account, b *futures.AccountV3) error {
	if a == nil {
		return fmt.Errorf("null v2 account baseline")
	}
	for _, f := range []struct {
		name, a, b string
		rel        float64
	}{
		{"totalWalletBalance", a.TotalWalletBalance, b.TotalWalletBalance, 1e-6},
		{"availableBalance", a.AvailableBalance, b.AvailableBalance, 0.005},
		{"totalUnrealizedProfit", a.TotalUnrealizedProfit, b.TotalUnrealizedProfit, 0.005},
	} {
		if err := compareFuturesNumber(f.name, f.a, f.b, 0.01, f.rel); err != nil {
			return err
		}
	}
	return nil
}

func (t *FuturesTrader) fetchAccount() (*futures.Account, error) {
	s := t.readState()
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if t.now().Before(s.accountDisabledUntil) {
		return t.client.NewGetAccountService().Do(context.Background())
	}
	check := !s.useAccountV3 || t.now().Sub(s.accountCheckedAt) >= futuresV3RecheckInterval
	var v2 *futures.Account
	var v2err error
	var v3 *futures.AccountV3
	var err error
	client, transport := t.v3ReadClient()
	if check {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); v2, v2err = t.client.NewGetAccountService().Do(context.Background()) }()
		go func() { defer wg.Done(); v3, err = client.NewGetAccountV3Service().Do(context.Background()) }()
		wg.Wait()
	} else {
		v3, err = client.NewGetAccountV3Service().Do(context.Background())
	}
	cooldown := futuresV3SemanticCooldown
	if err != nil {
		cooldown = futuresV3ErrorCooldown(err, int(transport.status.Load()))
	}
	if err == nil {
		err = validateAccountV3(v3)
	}
	if err == nil && check && v2err == nil {
		err = reconcileAccount(v2, v3)
	}
	if err != nil {
		s.coolAccountV3(t.now(), cooldown, err)
		if !check {
			v2, v2err = t.client.NewGetAccountService().Do(context.Background())
		}
		if v2err != nil {
			return nil, futuresV3FallbackError("GetBalance", err, v2err)
		}
		return v2, futuresV3FallbackError("GetBalance", err, nil)
	}
	if check && v2err != nil {
		logger.Errorf("futures account v3 reconciliation unavailable; v2 baseline failed: %v", v2err)
		return nil, v2err
	}
	s.useAccountV3 = true
	s.accountDisabledUntil = time.Time{}
	if check {
		s.accountCheckedAt = t.now()
	}
	return &futures.Account{TotalWalletBalance: v3.TotalWalletBalance, AvailableBalance: v3.AvailableBalance, TotalUnrealizedProfit: v3.TotalUnrealizedProfit}, nil
}
