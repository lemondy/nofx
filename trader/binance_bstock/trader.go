package binance_bstock

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/common"
	"github.com/google/uuid"
	"nofx/trader/types"
)

const cacheTTL = 15 * time.Second
const clientPrefix = "nxbs_"

// ErrAuthOrPermission allows callers to pause execution until credentials,
// IP restrictions or spot trading permissions are fixed. APIError is retained
// in the same error chain so errors.As still exposes Binance's code/message.
var ErrAuthOrPermission = errors.New("spot bStock auth/permission error")

type BStockTrader struct {
	client *binance.Client
	now    func() time.Time
	// Serialize order/protection mutations, including cancel-and-replace sequences.
	execMu        sync.Mutex
	mu            sync.Mutex
	rulesMu       sync.Mutex
	rules         map[string]binance.Symbol
	rulesTime     time.Time
	balance       map[string]interface{}
	balanceTime   time.Time
	positions     []map[string]interface{}
	positionsTime time.Time
	historyMu     sync.Mutex
	history       map[string]*fifoHistory
}

var _ types.SpotStockTrader = (*BStockTrader)(nil)

func NewBStockTrader(apiKey, secretKey string) *BStockTrader {
	c := binance.NewClient(apiKey, secretKey)
	c.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	return &BStockTrader{client: c, now: time.Now, history: make(map[string]*fifoHistory)}
}

func clientID() string                { return clientPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:24] }
func number(s string) float64         { f, _ := strconv.ParseFloat(s, 64); return f }
func id(n int64) string               { return strconv.FormatInt(n, 10) }
func value(f float64) string          { return strconv.FormatFloat(f, 'f', -1, 64) }
func unsupported(action string) error { return fmt.Errorf("%s not supported on spot bStock", action) }

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	category := "Binance request failed"
	var api *common.APIError
	if errors.As(err, &api) {
		msg := strings.ToLower(api.Message)
		switch {
		case api.Code == -2015 || api.Code == -2014 || strings.Contains(msg, "not permitted"):
			return fmt.Errorf("%s: %w (check API key, IP restrictions and spot trading permission): %w", op, ErrAuthOrPermission, err)
		case api.Code == -1121 || strings.Contains(msg, "not tradable") || strings.Contains(msg, "market is closed"):
			category = "symbol not tradable"
		case api.Code == -1013 || strings.Contains(msg, "filter"):
			category = "spot filter failure"
		}
	}
	return fmt.Errorf("%s: %s: %w", op, category, err)
}

// signed is used only where the pinned SDK lacks the current OCO endpoint.
// It shares the SDK transport, credentials, BaseURL and server time offset.
func (t *BStockTrader) signed(method, path string, p url.Values, out interface{}) error {
	p.Set("timestamp", id(t.now().UnixMilli()-t.client.TimeOffset))
	p.Set("recvWindow", "5000")
	query := p.Encode()
	mac := hmac.New(sha256.New, []byte(t.client.SecretKey))
	_, _ = mac.Write([]byte(query))
	query += "&signature=" + hex.EncodeToString(mac.Sum(nil))
	req, err := http.NewRequestWithContext(context.Background(), method, t.client.BaseURL+path+"?"+query, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("X-MBX-APIKEY", t.client.APIKey)
	req.Header.Set("User-Agent", t.client.UserAgent)
	resp, err := t.client.HTTPClient.Do(req)
	if err != nil {
		return wrap(path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return wrap(path, err)
	}
	if resp.StatusCode >= 400 {
		api := &common.APIError{Response: body}
		_ = json.Unmarshal(body, api)
		return wrap(path, api)
	}
	if out == nil {
		return nil
	}
	return wrap(path, json.Unmarshal(body, out))
}

// Caller holds execMu. Cache fills hold mu until published, so invalidation
// cannot be undone by a concurrent, older account fetch.
func (t *BStockTrader) invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.balance, t.positions = nil, nil
	t.historyMu.Lock()
	defer t.historyMu.Unlock()
	for _, h := range t.history {
		h.at = time.Time{}
	}
}

func (t *BStockTrader) OpenShort(string, float64, int) (map[string]interface{}, error) {
	return nil, unsupported("OpenShort")
}
func (t *BStockTrader) CloseShort(string, float64) (map[string]interface{}, error) {
	return nil, unsupported("CloseShort")
}

// SetLeverage(1) is a no-op for shared cash execution paths; all other values fail.
func (t *BStockTrader) SetLeverage(_ string, leverage int) error {
	if leverage == 1 {
		return nil
	}
	return unsupported("SetLeverage")
}

// Margin mode is explicitly rejected: this executor never switches wallet modes.
func (t *BStockTrader) SetMarginMode(string, bool) error { return unsupported("SetMarginMode") }

func orderMap(o *types.SpotOrderResult) map[string]interface{} {
	return map[string]interface{}{"orderId": o.OrderID, "symbol": o.Symbol, "status": o.Status, "side": o.Side, "type": o.Type, "executedQty": o.ExecutedQty, "avgPrice": o.AvgPrice, "commission": o.Commission, "clientOrderId": o.ClientOrderID}
}

func (t *BStockTrader) CloseLong(symbol string, qty float64) (map[string]interface{}, error) {
	o, err := t.SellMarket(symbol, qty)
	if err != nil {
		return nil, err
	}
	return orderMap(o), nil
}
