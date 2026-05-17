package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"coin/internal/binance"
)

// loadRootCAs 优先按 SSL_CERT_FILE,然后按项目根的 cacert.pem 加载,绕开 macOS 26 上
// crypto/x509 调 SecTrust 返回 OSStatus -26276 的兼容性 bug。
func loadRootCAs() *x509.CertPool {
	candidates := []string{os.Getenv("SSL_CERT_FILE")}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "cacert.pem"))
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
			fmt.Printf("(using RootCAs from %s)\n", p)
			return pool
		}
	}
	return nil
}

func main() {
	symbol := "BTCUSDT"
	if len(os.Args) > 1 {
		symbol = os.Args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := binance.NewFuturesClient("prod")
	if pool := loadRootCAs(); pool != nil {
		c.HTTP = &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		}
	}
	fmt.Printf("== Binance USDⓈ-M connectivity probe ==\nBaseURL : %s\nSymbol  : %s\n\n", c.BaseURL, symbol)

	t0 := time.Now()
	if err := c.SyncTime(ctx); err != nil {
		fmt.Println("[FAIL] SyncTime:", err)
		os.Exit(1)
	}
	fmt.Printf("[OK] SyncTime          rtt=%v offset=%dms\n\n", time.Since(t0).Round(time.Millisecond), c.TimeOffsetMS)

	t0 = time.Now()
	candles, err := c.Klines(ctx, symbol, "1h", time.Time{}, time.Time{}, 5)
	if err != nil {
		fmt.Println("[FAIL] Klines:", err)
		os.Exit(1)
	}
	fmt.Printf("[OK] Klines 1h x%-3d   rtt=%v\n", len(candles), time.Since(t0).Round(time.Millisecond))
	for _, k := range candles {
		fmt.Printf("    %s  O=%-9.2f H=%-9.2f L=%-9.2f C=%-9.2f V=%-10.3f\n",
			k.OpenTime.UTC().Format("2006-01-02 15:04Z"), k.Open, k.High, k.Low, k.Close, k.Volume)
	}
	fmt.Println()

	t0 = time.Now()
	bt, err := c.FuturesBookTicker(ctx, symbol)
	if err != nil {
		fmt.Println("[FAIL] FuturesBookTicker:", err)
		os.Exit(1)
	}
	mid := (bt.BidPrice + bt.AskPrice) / 2
	spreadBps := (bt.AskPrice - bt.BidPrice) / mid * 1e4
	fmt.Printf("[OK] FuturesBookTicker rtt=%v\n    Bid=%.2f (qty=%.3f)  Ask=%.2f (qty=%.3f)  Mid=%.2f  Spread=%.2fbps\n\n",
		time.Since(t0).Round(time.Millisecond), bt.BidPrice, bt.BidQty, bt.AskPrice, bt.AskQty, mid, spreadBps)

	t0 = time.Now()
	pi, err := c.PremiumIndex(ctx, symbol)
	if err != nil {
		fmt.Println("[FAIL] PremiumIndex:", err)
		os.Exit(1)
	}
	fmt.Printf("[OK] PremiumIndex      rtt=%v\n    MarkPrice=%.2f  IndexPrice=%.2f  LastFundingRate=%+.6f%%  NextFunding=%s\n\n",
		time.Since(t0).Round(time.Millisecond), pi.MarkPrice, pi.IndexPrice, pi.LastFundingRate*100, pi.NextFundingTime.UTC().Format("2006-01-02 15:04Z"))

	t0 = time.Now()
	since := time.Now().Add(-72 * time.Hour)
	frs, err := c.FundingRates(ctx, symbol, since, time.Time{})
	if err != nil {
		fmt.Println("[FAIL] FundingRates:", err)
		os.Exit(1)
	}
	fmt.Printf("[OK] FundingRates last72h rtt=%v  rows=%d\n", time.Since(t0).Round(time.Millisecond), len(frs))
	tail := frs
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	for _, f := range tail {
		fmt.Printf("    %s  rate=%+.6f%%  mark=%.2f\n", f.Time.UTC().Format("2006-01-02 15:04Z"), f.Rate*100, f.MarkPrice)
	}

	fmt.Println("\n== ALL OK ==")
}
