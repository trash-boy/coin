package binance

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"coin/internal/strategy"
)

const (
	ProdFuturesBaseURL    = "https://fapi.binance.com"
	TestnetFuturesBaseURL = "https://demo-fapi.binance.com"
	ProdSpotBaseURL       = "https://api.binance.com"
	TestnetSpotBaseURL    = "https://testnet.binance.vision"
)

type FuturesClient struct {
	BaseURL      string
	SpotBaseURL  string
	APIKey       string
	Secret       string
	TimeOffsetMS int64
	HTTP         *http.Client
}

func NewFuturesClient(env string) *FuturesClient {
	base := ProdFuturesBaseURL
	spotBase := ProdSpotBaseURL
	if strings.EqualFold(env, "testnet") || strings.EqualFold(env, "demo") {
		base = TestnetFuturesBaseURL
		spotBase = TestnetSpotBaseURL
	}
	return &FuturesClient{
		BaseURL:     base,
		SpotBaseURL: spotBase,
		APIKey:      os.Getenv("BINANCE_API_KEY"),
		Secret:      os.Getenv("BINANCE_API_SECRET"),
		HTTP: &http.Client{
			Timeout:   15 * time.Second,
			Transport: defaultTransport(),
		},
	}
}

// defaultTransport 返回 http.Transport;保留 Go 默认读取 HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY 的行为。若能从 SSL_CERT_FILE 或项目根/工作目录的 cacert.pem 读到
// PEM,则把 RootCAs 显式注入,绕开 macOS 26 上 crypto/x509 调
// SecTrustEvaluateWithError 返回 OSStatus -26276 的兼容性问题。
func defaultTransport() *http.Transport {
	t := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if pool := loadRootCAsFromFile(); pool != nil {
		t.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return t
}

func loadRootCAsFromFile() *x509.CertPool {
	candidates := []string{os.Getenv("SSL_CERT_FILE")}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "cacert.pem"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "cacert.pem"))
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		pem, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(pem) {
			return pool
		}
	}
	return nil
}

type SymbolRules struct {
	Symbol      string
	StepSize    float64
	TickSize    float64
	MinQty      float64
	MinNotional float64
}

type PremiumIndex struct {
	Symbol          string
	MarkPrice       float64
	IndexPrice      float64
	LastFundingRate float64
	NextFundingTime time.Time
	Time            time.Time
}

type BookTicker struct {
	Symbol   string
	BidPrice float64
	BidQty   float64
	AskPrice float64
	AskQty   float64
	Time     time.Time
}

type OrderResponse struct {
	ClientOrderID string
	OrderID       int64
	Symbol        string
	Status        string
	Type          string
	Side          string
	AveragePrice  string
	ExecutedQty   string
}

type ProtectedOrderResult struct {
	Entry OrderResponse
	Stop  OrderResponse
}

func (r SymbolRules) RoundQuantity(qty float64) float64 {
	if r.StepSize <= 0 {
		return qty
	}
	return math.Floor(qty/r.StepSize) * r.StepSize
}

func (r SymbolRules) RoundPrice(price float64) float64 {
	if r.TickSize <= 0 {
		return price
	}
	return math.Round(price/r.TickSize) * r.TickSize
}

func (r SymbolRules) ValidNotional(qty, price float64) bool {
	if r.MinNotional <= 0 {
		return true
	}
	return qty*price >= r.MinNotional
}

func (c *FuturesClient) Klines(ctx context.Context, symbol, interval string, start, end time.Time, limit int) ([]strategy.Candle, error) {
	if limit <= 0 || limit > 1500 {
		limit = 1500
	}
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("interval", interval)
	params.Set("limit", strconv.Itoa(limit))
	if !start.IsZero() {
		params.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	}
	if !end.IsZero() {
		params.Set("endTime", strconv.FormatInt(end.UnixMilli(), 10))
	}

	var raw [][]interface{}
	if err := c.publicGET(ctx, "/fapi/v1/klines", params, &raw); err != nil {
		return nil, err
	}
	out := make([]strategy.Candle, 0, len(raw))
	for _, row := range raw {
		if len(row) < 11 {
			return nil, fmt.Errorf("unexpected kline row length %d", len(row))
		}
		openTime, err := numberToInt64(row[0])
		if err != nil {
			return nil, fmt.Errorf("open time: %w", err)
		}
		closeTime, err := numberToInt64(row[6])
		if err != nil {
			return nil, fmt.Errorf("close time: %w", err)
		}
		open, err := stringFloat(row[1])
		if err != nil {
			return nil, err
		}
		high, err := stringFloat(row[2])
		if err != nil {
			return nil, err
		}
		low, err := stringFloat(row[3])
		if err != nil {
			return nil, err
		}
		closePrice, err := stringFloat(row[4])
		if err != nil {
			return nil, err
		}
		volume, err := stringFloat(row[5])
		if err != nil {
			return nil, err
		}
		out = append(out, strategy.Candle{
			OpenTime:  time.UnixMilli(openTime).UTC(),
			CloseTime: time.UnixMilli(closeTime).UTC(),
			Open:      open,
			High:      high,
			Low:       low,
			Close:     closePrice,
			Volume:    volume,
		})
	}
	return out, nil
}

func (c *FuturesClient) KlinesRange(ctx context.Context, symbol, interval string, start, end time.Time) ([]strategy.Candle, error) {
	step, err := intervalDuration(interval)
	if err != nil {
		return nil, err
	}
	var all []strategy.Candle
	next := start
	for {
		prevNext := next
		part, err := c.Klines(ctx, symbol, interval, next, end, 1500)
		if err != nil {
			return nil, err
		}
		if len(part) == 0 {
			break
		}
		all = append(all, part...)
		lastOpen := part[len(part)-1].OpenTime
		next = lastOpen.Add(step)
		if !next.After(prevNext) {
			break
		}
		if !end.IsZero() && !next.Before(end) {
			break
		}
		if len(part) < 1500 {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	return dedupeCandles(all), nil
}

func (c *FuturesClient) SymbolRules(ctx context.Context, symbol string) (SymbolRules, error) {
	var raw struct {
		Symbols []struct {
			Symbol  string `json:"symbol"`
			Filters []struct {
				FilterType  string `json:"filterType"`
				StepSize    string `json:"stepSize"`
				TickSize    string `json:"tickSize"`
				MinQty      string `json:"minQty"`
				Notional    string `json:"notional"`
				MinNotional string `json:"minNotional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := c.publicGET(ctx, "/fapi/v1/exchangeInfo", url.Values{}, &raw); err != nil {
		return SymbolRules{}, err
	}
	want := strings.ToUpper(symbol)
	for _, s := range raw.Symbols {
		if s.Symbol != want {
			continue
		}
		rules := SymbolRules{Symbol: s.Symbol}
		for _, f := range s.Filters {
			switch f.FilterType {
			case "LOT_SIZE":
				rules.StepSize, _ = strconv.ParseFloat(f.StepSize, 64)
				rules.MinQty, _ = strconv.ParseFloat(f.MinQty, 64)
			case "PRICE_FILTER":
				rules.TickSize, _ = strconv.ParseFloat(f.TickSize, 64)
			case "MIN_NOTIONAL":
				if f.Notional != "" {
					rules.MinNotional, _ = strconv.ParseFloat(f.Notional, 64)
				} else {
					rules.MinNotional, _ = strconv.ParseFloat(f.MinNotional, 64)
				}
			}
		}
		return rules, nil
	}
	return SymbolRules{}, fmt.Errorf("symbol %s not found in exchangeInfo", want)
}

func (c *FuturesClient) PremiumIndex(ctx context.Context, symbol string) (PremiumIndex, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	var raw struct {
		Symbol          string `json:"symbol"`
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
		Time            int64  `json:"time"`
	}
	if err := c.publicGET(ctx, "/fapi/v1/premiumIndex", params, &raw); err != nil {
		return PremiumIndex{}, err
	}
	mark, err := strconv.ParseFloat(raw.MarkPrice, 64)
	if err != nil {
		return PremiumIndex{}, err
	}
	index, err := strconv.ParseFloat(raw.IndexPrice, 64)
	if err != nil {
		return PremiumIndex{}, err
	}
	rate, err := strconv.ParseFloat(raw.LastFundingRate, 64)
	if err != nil {
		return PremiumIndex{}, err
	}
	return PremiumIndex{
		Symbol:          raw.Symbol,
		MarkPrice:       mark,
		IndexPrice:      index,
		LastFundingRate: rate,
		NextFundingTime: time.UnixMilli(raw.NextFundingTime).UTC(),
		Time:            time.UnixMilli(raw.Time).UTC(),
	}, nil
}

func (c *FuturesClient) FuturesBookTicker(ctx context.Context, symbol string) (BookTicker, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	var raw bookTickerRaw
	if err := c.publicGET(ctx, "/fapi/v1/ticker/bookTicker", params, &raw); err != nil {
		return BookTicker{}, err
	}
	return raw.bookTicker()
}

func (c *FuturesClient) SpotBookTicker(ctx context.Context, symbol string) (BookTicker, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	var raw bookTickerRaw
	if err := c.publicGETBase(ctx, c.spotBaseURL(), "/api/v3/ticker/bookTicker", params, &raw); err != nil {
		return BookTicker{}, err
	}
	return raw.bookTicker()
}

func (c *FuturesClient) FundingRates(ctx context.Context, symbol string, start, end time.Time) ([]strategy.FundingRate, error) {
	var all []strategy.FundingRate
	next := start
	for {
		prevNext := next
		params := url.Values{}
		params.Set("symbol", strings.ToUpper(symbol))
		params.Set("limit", "1000")
		if !next.IsZero() {
			params.Set("startTime", strconv.FormatInt(next.UnixMilli(), 10))
		}
		if !end.IsZero() {
			params.Set("endTime", strconv.FormatInt(end.UnixMilli(), 10))
		}
		var raw []struct {
			FundingRate string `json:"fundingRate"`
			FundingTime int64  `json:"fundingTime"`
			MarkPrice   string `json:"markPrice"`
		}
		if err := c.publicGET(ctx, "/fapi/v1/fundingRate", params, &raw); err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}
		for _, row := range raw {
			if strings.TrimSpace(row.FundingRate) == "" {
				continue
			}
			rate, err := strconv.ParseFloat(row.FundingRate, 64)
			if err != nil {
				return nil, err
			}
			var mark float64
			if strings.TrimSpace(row.MarkPrice) != "" {
				if m, mErr := strconv.ParseFloat(row.MarkPrice, 64); mErr == nil {
					mark = m
				}
			}
			all = append(all, strategy.FundingRate{
				Time:      time.UnixMilli(row.FundingTime).UTC(),
				Rate:      rate,
				MarkPrice: mark,
			})
		}
		last := time.UnixMilli(raw[len(raw)-1].FundingTime).UTC()
		next = last.Add(time.Millisecond)
		if !next.After(prevNext) {
			break
		}
		if !end.IsZero() && !next.Before(end) {
			break
		}
		if len(raw) < 1000 {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	return all, nil
}

func (c *FuturesClient) LeverageBrackets(ctx context.Context, symbol string) ([]strategy.MaintenanceBracket, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	// Binance /fapi/v1/leverageBracket always returns a JSON array,
	// even when ?symbol=... narrows it to a single element. Decode as a slice.
	var raw []struct {
		Symbol   string `json:"symbol"`
		Brackets []struct {
			NotionalFloor    float64 `json:"notionalFloor"`
			NotionalCap      float64 `json:"notionalCap"`
			MaintMarginRatio float64 `json:"maintMarginRatio"`
			Cum              float64 `json:"cum"`
			InitialLeverage  int     `json:"initialLeverage"`
		} `json:"brackets"`
	}
	if err := c.signedRequest(ctx, http.MethodGet, "/fapi/v1/leverageBracket", params, &raw); err != nil {
		return nil, err
	}
	upper := strings.ToUpper(symbol)
	var entry *struct {
		Symbol   string `json:"symbol"`
		Brackets []struct {
			NotionalFloor    float64 `json:"notionalFloor"`
			NotionalCap      float64 `json:"notionalCap"`
			MaintMarginRatio float64 `json:"maintMarginRatio"`
			Cum              float64 `json:"cum"`
			InitialLeverage  int     `json:"initialLeverage"`
		} `json:"brackets"`
	}
	for i := range raw {
		if strings.EqualFold(raw[i].Symbol, upper) {
			entry = &raw[i]
			break
		}
	}
	if entry == nil {
		if len(raw) == 0 {
			return nil, fmt.Errorf("leverage brackets: empty response for %s", upper)
		}
		entry = &raw[0]
	}
	out := make([]strategy.MaintenanceBracket, 0, len(entry.Brackets))
	for _, bracket := range entry.Brackets {
		out = append(out, strategy.MaintenanceBracket{
			NotionalFloor:    bracket.NotionalFloor,
			NotionalCap:      bracket.NotionalCap,
			MaintMarginRatio: bracket.MaintMarginRatio,
			Cum:              bracket.Cum,
			InitialLeverage:  bracket.InitialLeverage,
		})
	}
	return out, nil
}

func (c *FuturesClient) SyncTime(ctx context.Context) error {
	var raw struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := c.publicGET(ctx, "/fapi/v1/time", url.Values{}, &raw); err != nil {
		return err
	}
	c.TimeOffsetMS = raw.ServerTime - time.Now().UnixMilli()
	return nil
}

func (c *FuturesClient) ChangeLeverage(ctx context.Context, symbol string, leverage int) error {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("leverage", strconv.Itoa(leverage))
	var raw map[string]interface{}
	return c.signedRequest(ctx, http.MethodPost, "/fapi/v1/leverage", params, &raw)
}

func ClampLeverageByBrackets(leverage int, notional float64, brackets []strategy.MaintenanceBracket) int {
	if leverage < 1 {
		leverage = 1
	}
	for _, bracket := range brackets {
		if notional >= bracket.NotionalFloor && (bracket.NotionalCap <= 0 || notional < bracket.NotionalCap) {
			if bracket.InitialLeverage > 0 && leverage > bracket.InitialLeverage {
				return bracket.InitialLeverage
			}
			return leverage
		}
	}
	return leverage
}

func (c *FuturesClient) QueryOrderByClientID(ctx context.Context, symbol string, clientOrderID string) (OrderResponse, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("origClientOrderId", clientOrderID)
	var raw orderResponseRaw
	if err := c.signedRequest(ctx, http.MethodGet, "/fapi/v1/order", params, &raw); err != nil {
		return OrderResponse{}, err
	}
	return raw.orderResponse(), nil
}

func (c *FuturesClient) PlaceMarketOrder(ctx context.Context, symbol string, side string, qty float64, reduceOnly bool) (OrderResponse, error) {
	return c.PlaceMarketOrderWithID(ctx, symbol, side, qty, reduceOnly, newClientOrderID("coin_mkt"))
}

func (c *FuturesClient) PlaceMarketOrderWithID(ctx context.Context, symbol string, side string, qty float64, reduceOnly bool, clientOrderID string) (OrderResponse, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("side", strings.ToUpper(side))
	params.Set("type", "MARKET")
	params.Set("quantity", formatFloat(qty))
	params.Set("newClientOrderId", clientOrderID)
	if reduceOnly {
		params.Set("reduceOnly", "true")
	}
	var raw orderResponseRaw
	if err := c.signedRequest(ctx, http.MethodPost, "/fapi/v1/order", params, &raw); err != nil {
		if isDuplicateClientOrderErr(err) && clientOrderID != "" {
			return c.QueryOrderByClientID(ctx, symbol, clientOrderID)
		}
		return OrderResponse{}, err
	}
	return raw.orderResponse(), nil
}

func (c *FuturesClient) PlaceStopMarketOrder(ctx context.Context, symbol string, side string, qty float64, stopPrice float64, reduceOnly bool, clientOrderID string) (OrderResponse, error) {
	return c.placeConditionalMarketOrder(ctx, symbol, side, "STOP_MARKET", qty, stopPrice, reduceOnly, clientOrderID)
}

func (c *FuturesClient) PlaceTakeProfitMarketOrder(ctx context.Context, symbol string, side string, qty float64, stopPrice float64, reduceOnly bool, clientOrderID string) (OrderResponse, error) {
	return c.placeConditionalMarketOrder(ctx, symbol, side, "TAKE_PROFIT_MARKET", qty, stopPrice, reduceOnly, clientOrderID)
}

func (c *FuturesClient) placeConditionalMarketOrder(ctx context.Context, symbol string, side string, orderType string, qty float64, stopPrice float64, reduceOnly bool, clientOrderID string) (OrderResponse, error) {
	if clientOrderID == "" {
		clientOrderID = newClientOrderID("coin_cond")
	}
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("side", strings.ToUpper(side))
	params.Set("type", orderType)
	params.Set("quantity", formatFloat(qty))
	params.Set("stopPrice", formatFloat(stopPrice))
	params.Set("workingType", "MARK_PRICE")
	params.Set("newClientOrderId", clientOrderID)
	if reduceOnly {
		params.Set("reduceOnly", "true")
	}
	var raw orderResponseRaw
	if err := c.signedRequest(ctx, http.MethodPost, "/fapi/v1/order", params, &raw); err != nil {
		if isDuplicateClientOrderErr(err) && clientOrderID != "" {
			return c.QueryOrderByClientID(ctx, symbol, clientOrderID)
		}
		return OrderResponse{}, err
	}
	return raw.orderResponse(), nil
}

func (c *FuturesClient) OpenProtectedMarketPosition(ctx context.Context, symbol string, sig strategy.Signal) (ProtectedOrderResult, error) {
	if sig.Action != strategy.ActionEnter || sig.Quantity <= 0 || sig.StopLoss <= 0 {
		return ProtectedOrderResult{}, fmt.Errorf("signal is not a valid entry")
	}
	leverage := int(math.Ceil(sig.Leverage))
	if leverage < 1 {
		leverage = 1
	}
	if leverage > 125 {
		leverage = 125
	}
	brackets, bracketErr := c.LeverageBrackets(ctx, symbol)
	if bracketErr == nil {
		leverage = ClampLeverageByBrackets(leverage, sig.Notional, brackets)
	}
	if err := c.ChangeLeverage(ctx, symbol, leverage); err != nil {
		return ProtectedOrderResult{}, fmt.Errorf("change leverage: %w", err)
	}

	entrySide, stopSide := orderSides(sig.Side)
	if entrySide == "" {
		return ProtectedOrderResult{}, fmt.Errorf("unsupported signal side %s", sig.Side)
	}
	entry, err := c.PlaceMarketOrderWithID(ctx, symbol, entrySide, sig.Quantity, false, newClientOrderID("coin_entry"))
	if err != nil {
		return ProtectedOrderResult{}, fmt.Errorf("entry order: %w", err)
	}
	stop, err := c.PlaceStopMarketOrder(ctx, symbol, stopSide, sig.Quantity, sig.StopLoss, true, newClientOrderID("coin_stop"))
	if err != nil {
		_, reduceErr := c.PlaceMarketOrderWithID(ctx, symbol, stopSide, sig.Quantity, true, newClientOrderID("coin_rollback"))
		if reduceErr != nil {
			return ProtectedOrderResult{Entry: entry}, fmt.Errorf("stop order failed: %w; rollback failed: %v", err, reduceErr)
		}
		return ProtectedOrderResult{Entry: entry}, fmt.Errorf("stop order failed and position was reduced: %w", err)
	}
	return ProtectedOrderResult{Entry: entry, Stop: stop}, nil
}

func (c *FuturesClient) StartUserDataStream(ctx context.Context) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("BINANCE_API_KEY is required for user data stream")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/fapi/v1/listenKey", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-MBX-APIKEY", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var raw struct {
		ListenKey string `json:"listenKey"`
	}
	if err := decodeResponse(resp, &raw); err != nil {
		return "", err
	}
	return raw.ListenKey, nil
}

func (c *FuturesClient) KeepaliveUserDataStream(ctx context.Context) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("BINANCE_API_KEY is required for user data stream")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/fapi/v1/listenKey", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-MBX-APIKEY", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var raw struct {
		ListenKey string `json:"listenKey"`
	}
	if err := decodeResponse(resp, &raw); err != nil {
		return "", err
	}
	return raw.ListenKey, nil
}

func (c *FuturesClient) StartUserDataStreamWithKeepalive(ctx context.Context, interval time.Duration) (string, func(), error) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	listenKey, err := c.StartUserDataStream(ctx)
	if err != nil {
		return "", nil, err
	}
	keepaliveCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = c.KeepaliveUserDataStream(keepaliveCtx)
			case <-keepaliveCtx.Done():
				return
			}
		}
	}()
	return listenKey, cancel, nil
}

func (c *FuturesClient) publicGET(ctx context.Context, path string, params url.Values, out interface{}) error {
	return c.publicGETBase(ctx, c.BaseURL, path, params, out)
}

func (c *FuturesClient) publicGETBase(ctx context.Context, baseURL, path string, params url.Values, out interface{}) error {
	u := strings.TrimRight(baseURL, "/") + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
			continue
		}
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("binance returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
			time.Sleep(time.Duration(200*(attempt+1)) * time.Millisecond)
			continue
		}
		defer resp.Body.Close()
		return decodeResponse(resp, out)
	}
	return lastErr
}

func (c *FuturesClient) spotBaseURL() string {
	if c.SpotBaseURL != "" {
		return c.SpotBaseURL
	}
	return ProdSpotBaseURL
}

func (c *FuturesClient) signedRequest(ctx context.Context, method, path string, params url.Values, out interface{}) error {
	if c.APIKey == "" || c.Secret == "" {
		return fmt.Errorf("BINANCE_API_KEY and BINANCE_API_SECRET are required for signed requests")
	}
	if c.TimeOffsetMS == 0 {
		_ = c.SyncTime(ctx)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		signed := cloneValues(params)
		signed.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli()+c.TimeOffsetMS, 10))
		signed.Set("recvWindow", "10000")
		payload := signed.Encode()
		mac := hmac.New(sha256.New, []byte(c.Secret))
		_, _ = mac.Write([]byte(payload))
		signed.Set("signature", hex.EncodeToString(mac.Sum(nil)))

		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path+"?"+signed.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-MBX-APIKEY", c.APIKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			sleepBackoff(attempt)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			sleepBackoff(attempt)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("decode binance response: %w", err)
			}
			return nil
		}
		lastErr = fmt.Errorf("binance returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
		if strings.Contains(string(body), "-1021") {
			if err := c.SyncTime(ctx); err != nil {
				return fmt.Errorf("%w; sync time failed: %v", lastErr, err)
			}
			sleepBackoff(attempt)
			continue
		}
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			sleepBackoff(attempt)
			continue
		}
		return lastErr
	}
	return lastErr
}

func decodeResponse(resp *http.Response, out interface{}) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("binance returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode binance response: %w", err)
	}
	return nil
}

func stringFloat(v interface{}) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("expected string float, got %T", v)
	}
	return strconv.ParseFloat(s, 64)
}

func numberToInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case json.Number:
		return n.Int64()
	default:
		return 0, fmt.Errorf("expected numeric timestamp, got %T", v)
	}
}

func intervalDuration(interval string) (time.Duration, error) {
	if len(interval) < 2 {
		return 0, fmt.Errorf("invalid interval %q", interval)
	}
	n, err := strconv.Atoi(interval[:len(interval)-1])
	if err != nil {
		return 0, err
	}
	switch interval[len(interval)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported interval %q", interval)
	}
}

func dedupeCandles(in []strategy.Candle) []strategy.Candle {
	out := make([]strategy.Candle, 0, len(in))
	seen := make(map[int64]bool, len(in))
	for _, c := range in {
		key := c.OpenTime.UnixMilli()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

type orderResponseRaw struct {
	ClientOrderID string `json:"clientOrderId"`
	OrderID       int64  `json:"orderId"`
	Symbol        string `json:"symbol"`
	Status        string `json:"status"`
	Type          string `json:"type"`
	Side          string `json:"side"`
	AveragePrice  string `json:"avgPrice"`
	ExecutedQty   string `json:"executedQty"`
}

type bookTickerRaw struct {
	Symbol   string `json:"symbol"`
	BidPrice string `json:"bidPrice"`
	BidQty   string `json:"bidQty"`
	AskPrice string `json:"askPrice"`
	AskQty   string `json:"askQty"`
	Time     int64  `json:"time"`
}

func (r bookTickerRaw) bookTicker() (BookTicker, error) {
	bidPrice, err := strconv.ParseFloat(r.BidPrice, 64)
	if err != nil {
		return BookTicker{}, err
	}
	bidQty, err := strconv.ParseFloat(r.BidQty, 64)
	if err != nil {
		return BookTicker{}, err
	}
	askPrice, err := strconv.ParseFloat(r.AskPrice, 64)
	if err != nil {
		return BookTicker{}, err
	}
	askQty, err := strconv.ParseFloat(r.AskQty, 64)
	if err != nil {
		return BookTicker{}, err
	}
	t := time.Time{}
	if r.Time > 0 {
		t = time.UnixMilli(r.Time).UTC()
	}
	return BookTicker{
		Symbol:   r.Symbol,
		BidPrice: bidPrice,
		BidQty:   bidQty,
		AskPrice: askPrice,
		AskQty:   askQty,
		Time:     t,
	}, nil
}

func (r orderResponseRaw) orderResponse() OrderResponse {
	return OrderResponse{
		ClientOrderID: r.ClientOrderID,
		OrderID:       r.OrderID,
		Symbol:        r.Symbol,
		Status:        r.Status,
		Type:          r.Type,
		Side:          r.Side,
		AveragePrice:  r.AveragePrice,
		ExecutedQty:   r.ExecutedQty,
	}
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for key, vals := range values {
		out[key] = append([]string(nil), vals...)
	}
	return out
}

func sleepBackoff(attempt int) {
	time.Sleep(time.Duration(200*(1<<attempt)) * time.Millisecond)
}

func newClientOrderID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b[:]))
}

func isDuplicateClientOrderErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "client order id") &&
		(strings.Contains(msg, "used") || strings.Contains(msg, "duplicate") || strings.Contains(msg, "-2010"))
}

func orderSides(side strategy.Side) (entry string, stop string) {
	switch side {
	case strategy.Long:
		return "BUY", "SELL"
	case strategy.Short:
		return "SELL", "BUY"
	default:
		return "", ""
	}
}

// AccountBalance 返回 USDT (或指定 asset) 的可用余额与权益(按 wallet/availableBalance/equity)。
type AccountAssetBalance struct {
	Asset            string  `json:"asset"`
	WalletBalance    float64 // 钱包余额
	AvailableBalance float64 // 可下单余额
	CrossUnPnL       float64 // 全仓未实现盈亏
	MarginBalance    float64 // 保证金余额(权益)
}

func (c *FuturesClient) AccountBalance(ctx context.Context, asset string) (AccountAssetBalance, error) {
	params := url.Values{}
	var raw []struct {
		Asset            string `json:"asset"`
		Balance          string `json:"balance"`
		CrossUnPnl       string `json:"crossUnPnl"`
		AvailableBalance string `json:"availableBalance"`
		MarginBalance    string `json:"marginBalance"`
	}
	if err := c.signedRequest(ctx, http.MethodGet, "/fapi/v2/balance", params, &raw); err != nil {
		return AccountAssetBalance{}, err
	}
	want := strings.ToUpper(asset)
	if want == "" {
		want = "USDT"
	}
	for _, r := range raw {
		if strings.ToUpper(r.Asset) != want {
			continue
		}
		out := AccountAssetBalance{Asset: r.Asset}
		if r.Balance != "" {
			out.WalletBalance, _ = strconv.ParseFloat(r.Balance, 64)
		}
		if r.CrossUnPnl != "" {
			out.CrossUnPnL, _ = strconv.ParseFloat(r.CrossUnPnl, 64)
		}
		if r.AvailableBalance != "" {
			out.AvailableBalance, _ = strconv.ParseFloat(r.AvailableBalance, 64)
		}
		if r.MarginBalance != "" {
			out.MarginBalance, _ = strconv.ParseFloat(r.MarginBalance, 64)
		}
		return out, nil
	}
	return AccountAssetBalance{Asset: want}, nil
}

// Position 单个标的的合约持仓快照(USDⓈ-M)。
type Position struct {
	Symbol           string
	PositionAmt      float64 // 正多负空,0 = 无仓
	EntryPrice       float64
	MarkPrice        float64
	Leverage         int
	UnrealizedProfit float64
}

func (c *FuturesClient) Positions(ctx context.Context, symbol string) ([]Position, error) {
	params := url.Values{}
	if symbol != "" {
		params.Set("symbol", strings.ToUpper(symbol))
	}
	var raw []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		EntryPrice       string `json:"entryPrice"`
		MarkPrice        string `json:"markPrice"`
		Leverage         string `json:"leverage"`
		UnrealizedProfit string `json:"unRealizedProfit"`
	}
	if err := c.signedRequest(ctx, http.MethodGet, "/fapi/v2/positionRisk", params, &raw); err != nil {
		return nil, err
	}
	out := make([]Position, 0, len(raw))
	for _, r := range raw {
		p := Position{Symbol: r.Symbol}
		if r.PositionAmt != "" {
			p.PositionAmt, _ = strconv.ParseFloat(r.PositionAmt, 64)
		}
		if r.EntryPrice != "" {
			p.EntryPrice, _ = strconv.ParseFloat(r.EntryPrice, 64)
		}
		if r.MarkPrice != "" {
			p.MarkPrice, _ = strconv.ParseFloat(r.MarkPrice, 64)
		}
		if r.Leverage != "" {
			if lv, err := strconv.Atoi(r.Leverage); err == nil {
				p.Leverage = lv
			}
		}
		if r.UnrealizedProfit != "" {
			p.UnrealizedProfit, _ = strconv.ParseFloat(r.UnrealizedProfit, 64)
		}
		out = append(out, p)
	}
	return out, nil
}

// CancelAllOpenOrders 撤掉指定 symbol 的所有挂单(止损/止盈/限价等)。
func (c *FuturesClient) CancelAllOpenOrders(ctx context.Context, symbol string) error {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	var raw map[string]interface{}
	return c.signedRequest(ctx, http.MethodDelete, "/fapi/v1/allOpenOrders", params, &raw)
}

// ListedSymbol describes one tradable USD-M perpetual contract with rough
// 24h liquidity, used by the live-scan command to short-list candidates.
type ListedSymbol struct {
	Symbol           string  // e.g. BTCUSDT
	BaseAsset        string  // e.g. BTC
	QuoteVolumeUSDT  float64 // 24h quote volume in USDT
	LastPrice        float64
	PriceChangePct   float64 // 24h percent change
}

// ListUSDTPerpetuals returns the set of USD-M PERPETUAL contracts in TRADING
// status with USDT quote, joined with /fapi/v1/ticker/24hr for liquidity.
// Symbols whose 24h quote volume is below minQuoteVolume USDT are dropped.
func (c *FuturesClient) ListUSDTPerpetuals(ctx context.Context, minQuoteVolume float64) ([]ListedSymbol, error) {
	var infoRaw struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			Status       string `json:"status"`
			ContractType string `json:"contractType"`
			BaseAsset    string `json:"baseAsset"`
			QuoteAsset   string `json:"quoteAsset"`
		} `json:"symbols"`
	}
	if err := c.publicGET(ctx, "/fapi/v1/exchangeInfo", url.Values{}, &infoRaw); err != nil {
		return nil, fmt.Errorf("exchangeInfo: %w", err)
	}
	allowed := make(map[string]struct {
		base string
	}, len(infoRaw.Symbols))
	for _, s := range infoRaw.Symbols {
		if !strings.EqualFold(s.Status, "TRADING") {
			continue
		}
		if !strings.EqualFold(s.ContractType, "PERPETUAL") {
			continue
		}
		if !strings.EqualFold(s.QuoteAsset, "USDT") {
			continue
		}
		allowed[strings.ToUpper(s.Symbol)] = struct{ base string }{base: s.BaseAsset}
	}

	var tickerRaw []struct {
		Symbol             string `json:"symbol"`
		LastPrice          string `json:"lastPrice"`
		PriceChangePercent string `json:"priceChangePercent"`
		QuoteVolume        string `json:"quoteVolume"`
	}
	if err := c.publicGET(ctx, "/fapi/v1/ticker/24hr", url.Values{}, &tickerRaw); err != nil {
		return nil, fmt.Errorf("ticker/24hr: %w", err)
	}
	out := make([]ListedSymbol, 0, len(tickerRaw))
	for _, t := range tickerRaw {
		meta, ok := allowed[strings.ToUpper(t.Symbol)]
		if !ok {
			continue
		}
		qv, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		if qv < minQuoteVolume {
			continue
		}
		lp, _ := strconv.ParseFloat(t.LastPrice, 64)
		pcp, _ := strconv.ParseFloat(t.PriceChangePercent, 64)
		out = append(out, ListedSymbol{
			Symbol:          strings.ToUpper(t.Symbol),
			BaseAsset:       meta.base,
			QuoteVolumeUSDT: qv,
			LastPrice:       lp,
			PriceChangePct:  pcp,
		})
	}
	return out, nil
}
