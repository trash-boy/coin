package binance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"coin/internal/strategy"
)

func TestKlinesAndSymbolRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/klines":
			fmt.Fprint(w, `[[1735689600000,"100.0","110.0","90.0","105.0","123.4",1735693199999,"0","0","0","0","0"]]`)
		case "/fapi/v1/exchangeInfo":
			fmt.Fprint(w, `{"symbols":[{"symbol":"BTCUSDT","filters":[{"filterType":"LOT_SIZE","minQty":"0.001","stepSize":"0.001"},{"filterType":"PRICE_FILTER","tickSize":"0.10"},{"filterType":"MIN_NOTIONAL","notional":"100"}]}]}`)
		case "/fapi/v1/premiumIndex":
			fmt.Fprint(w, `{"symbol":"BTCUSDT","markPrice":"105.0","indexPrice":"104.9","lastFundingRate":"0.00010000","nextFundingTime":1735718400000,"time":1735689600000}`)
		case "/fapi/v1/fundingRate":
			fmt.Fprint(w, `[{"symbol":"BTCUSDT","fundingRate":"0.00010000","fundingTime":1735689600000,"markPrice":"105.0"}]`)
		case "/fapi/v1/ticker/bookTicker":
			fmt.Fprint(w, `{"symbol":"BTCUSDT","bidPrice":"104.90","bidQty":"1.5","askPrice":"105.10","askQty":"2.5","time":1735689600000}`)
		case "/api/v3/ticker/bookTicker":
			fmt.Fprint(w, `{"symbol":"BTCUSDT","bidPrice":"104.80","bidQty":"1.0","askPrice":"105.00","askQty":"2.0"}`)
		case "/fapi/v1/time":
			fmt.Fprint(w, `{"serverTime":1735689600000}`)
		case "/fapi/v1/leverageBracket":
			fmt.Fprint(w, `{"symbol":"BTCUSDT","brackets":[{"bracket":1,"initialLeverage":50,"notionalCap":50000,"notionalFloor":0,"maintMarginRatio":0.004,"cum":0}]}`)
		case "/fapi/v1/leverage":
			fmt.Fprint(w, `{"leverage":2,"maxNotionalValue":"50000","symbol":"BTCUSDT"}`)
		case "/fapi/v1/order":
			if r.URL.Query().Get("newClientOrderId") == "" {
				if r.URL.Query().Get("origClientOrderId") == "" {
					http.Error(w, "expected client order id", http.StatusBadRequest)
					return
				}
				fmt.Fprintf(w, `{"clientOrderId":%q,"orderId":123,"symbol":"BTCUSDT","status":"NEW","type":"MARKET","side":"BUY","avgPrice":"0","executedQty":"0"}`,
					r.URL.Query().Get("origClientOrderId"))
				return
			}
			fmt.Fprintf(w, `{"clientOrderId":%q,"orderId":123,"symbol":"BTCUSDT","status":"NEW","type":%q,"side":%q,"avgPrice":"0","executedQty":"0"}`,
				r.URL.Query().Get("newClientOrderId"), r.URL.Query().Get("type"), r.URL.Query().Get("side"))
		case "/fapi/v1/listenKey":
			fmt.Fprint(w, `{"listenKey":"abc"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &FuturesClient{BaseURL: server.URL, SpotBaseURL: server.URL, HTTP: server.Client()}
	candles, err := client.Klines(context.Background(), "BTCUSDT", "1h", time.Time{}, time.Time{}, 100)
	if err != nil {
		t.Fatalf("Klines returned error: %v", err)
	}
	if len(candles) != 1 {
		t.Fatalf("expected 1 candle, got %d", len(candles))
	}
	if candles[0].Close != 105.0 {
		t.Fatalf("unexpected close: %f", candles[0].Close)
	}

	rules, err := client.SymbolRules(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("SymbolRules returned error: %v", err)
	}
	if got := rules.RoundQuantity(0.0019); got != 0.001 {
		t.Fatalf("unexpected rounded quantity: %f", got)
	}
	if got := rules.RoundPrice(100.04); got != 100.0 {
		t.Fatalf("unexpected rounded price: %f", got)
	}
	if rules.ValidNotional(0.5, 100) {
		t.Fatal("expected notional below minimum to be invalid")
	}

	premium, err := client.PremiumIndex(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("PremiumIndex returned error: %v", err)
	}
	if premium.LastFundingRate != 0.0001 {
		t.Fatalf("unexpected funding rate: %f", premium.LastFundingRate)
	}

	funding, err := client.FundingRates(context.Background(), "BTCUSDT", time.UnixMilli(1735689600000), time.UnixMilli(1735718400000))
	if err != nil {
		t.Fatalf("FundingRates returned error: %v", err)
	}
	if len(funding) != 1 || funding[0].Rate != 0.0001 {
		t.Fatalf("unexpected funding history: %+v", funding)
	}
	futuresBook, err := client.FuturesBookTicker(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("FuturesBookTicker returned error: %v", err)
	}
	if futuresBook.BidPrice != 104.90 || futuresBook.AskQty != 2.5 {
		t.Fatalf("unexpected futures book ticker: %+v", futuresBook)
	}
	spotBook, err := client.SpotBookTicker(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("SpotBookTicker returned error: %v", err)
	}
	if spotBook.BidQty != 1.0 || spotBook.AskPrice != 105.00 {
		t.Fatalf("unexpected spot book ticker: %+v", spotBook)
	}
	client.APIKey = "key"
	client.Secret = "secret"
	brackets, err := client.LeverageBrackets(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("LeverageBrackets returned error: %v", err)
	}
	if len(brackets) != 1 || brackets[0].MaintMarginRatio != 0.004 {
		t.Fatalf("unexpected brackets: %+v", brackets)
	}
	order, err := client.PlaceMarketOrder(context.Background(), "BTCUSDT", "BUY", 0.01, false)
	if err != nil {
		t.Fatalf("PlaceMarketOrder returned error: %v", err)
	}
	if order.ClientOrderID == "" || order.Type != "MARKET" {
		t.Fatalf("unexpected order response: %+v", order)
	}
	stop, err := client.PlaceStopMarketOrder(context.Background(), "BTCUSDT", "SELL", 0.01, 90, true, "test_stop")
	if err != nil {
		t.Fatalf("PlaceStopMarketOrder returned error: %v", err)
	}
	if stop.ClientOrderID != "test_stop" || stop.Type != "STOP_MARKET" {
		t.Fatalf("unexpected stop response: %+v", stop)
	}
	listenKey, err := client.StartUserDataStream(context.Background())
	if err != nil {
		t.Fatalf("StartUserDataStream returned error: %v", err)
	}
	if listenKey != "abc" {
		t.Fatalf("unexpected listen key %q", listenKey)
	}
	keptAlive, err := client.KeepaliveUserDataStream(context.Background())
	if err != nil {
		t.Fatalf("KeepaliveUserDataStream returned error: %v", err)
	}
	if keptAlive != "abc" {
		t.Fatalf("unexpected keepalive listen key %q", keptAlive)
	}

	sig := strategy.Signal{
		Action:   strategy.ActionEnter,
		Side:     strategy.Long,
		Quantity: 0.01,
		Leverage: 2,
		StopLoss: 90,
	}
	protected, err := client.OpenProtectedMarketPosition(context.Background(), "BTCUSDT", sig)
	if err != nil {
		t.Fatalf("OpenProtectedMarketPosition returned error: %v", err)
	}
	if protected.Entry.Type != "MARKET" || protected.Stop.Type != "STOP_MARKET" {
		t.Fatalf("unexpected protected order result: %+v", protected)
	}
}

func TestDuplicateClientOrderIDQueriesExistingOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			fmt.Fprint(w, `{"serverTime":1735689600000}`)
		case "/fapi/v1/order":
			if r.Method == http.MethodPost {
				http.Error(w, `{"code":-2010,"msg":"Duplicate client order ID"}`, http.StatusBadRequest)
				return
			}
			if r.Method == http.MethodGet && r.URL.Query().Get("origClientOrderId") == "dup_order" {
				fmt.Fprint(w, `{"clientOrderId":"dup_order","orderId":456,"symbol":"BTCUSDT","status":"NEW","type":"MARKET","side":"BUY","avgPrice":"0","executedQty":"0"}`)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &FuturesClient{BaseURL: server.URL, HTTP: server.Client(), APIKey: "key", Secret: "secret"}
	order, err := client.PlaceMarketOrderWithID(context.Background(), "BTCUSDT", "BUY", 0.01, false, "dup_order")
	if err != nil {
		t.Fatalf("PlaceMarketOrderWithID returned error: %v", err)
	}
	if order.OrderID != 456 || order.ClientOrderID != "dup_order" {
		t.Fatalf("expected queried existing order, got %+v", order)
	}
}

func TestClampLeverageByBrackets(t *testing.T) {
	brackets := []strategy.MaintenanceBracket{{
		NotionalFloor:   0,
		NotionalCap:     1000,
		InitialLeverage: 5,
	}}
	if got := ClampLeverageByBrackets(10, 500, brackets); got != 5 {
		t.Fatalf("expected leverage clamped to 5, got %d", got)
	}
	if got := ClampLeverageByBrackets(3, 500, brackets); got != 3 {
		t.Fatalf("expected leverage unchanged at 3, got %d", got)
	}
}
