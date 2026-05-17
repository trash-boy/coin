package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"coin/internal/binance"
	"coin/internal/strategy"
)

// runLiveScan implements the `live-scan` subcommand.
//
// Behaviour:
//   1. enumerate all USD-M PERPETUAL contracts quoted in USDT via exchangeInfo
//      joined with /fapi/v1/ticker/24hr;
//   2. drop symbols with 24h quote volume below -min-quote-volume USDT;
//   3. on every candle close, evaluate strategy.LatestSignal for each symbol
//      concurrently (capped by -workers);
//   4. take all ENTER signals, sort by ATR/close descending, cap by
//      max-positions minus already-held positions, and place real orders;
//   5. EXIT signals on currently-held symbols always run.
//
// Per the user's explicit instruction, dry-run is REQUIRED to be disabled
// AND -i-understand-risk must be true. There is no first-cycle dry-run grace.
func runLiveScan(args []string) {
	fs := flag.NewFlagSet("live-scan", flag.ExitOnError)
	interval := fs.String("interval", "1h", "kline interval")
	envName := fs.String("env", "testnet", "prod or testnet")
	equityFlag := fs.Float64("equity", 0, "override sizing equity (USDT). 0 = fetch from balance")
	lookback := fs.Int("lookback", 5000, "klines fetched per cycle per symbol")
	pollEvery := fs.Duration("poll", 30*time.Second, "polling interval after candle close")
	dryRun := fs.Bool("dry-run", true, "if true, NEVER sends real orders")
	confirm := fs.Bool("i-understand-risk", false, "must be true to enable real orders")
	maxDailyLossPct := fs.Float64("max-daily-loss-pct", 5.0, "freeze entries for the rest of UTC day if equity DD this %% from day start")
	stateDir := fs.String("state-dir", ".", "directory for live state json files")
	configPath := fs.String("config", "", "flat YAML or JSON strategy config path")
	minQuoteVol := fs.Float64("min-quote-volume", 50_000_000, "minimum 24h USDT quote volume per symbol")
	maxPositions := fs.Int("max-positions", 3, "maximum concurrent positions across all symbols")
	workers := fs.Int("workers", 8, "concurrent symbol evaluators per cycle")
	verbose := fs.Bool("verbose", false, "print per-symbol reason for every NO_TRADE")
	cfg := configFlags(fs)
	_ = fs.Parse(args)
	if *configPath != "" {
		if err := loadConfigFile(*configPath, cfg); err != nil {
			log.Fatal(err)
		}
		_ = fs.Parse(args)
	}

	live := !*dryRun && *confirm
	if live && strings.EqualFold(*envName, "prod") {
		fmt.Println("==========================================================")
		fmt.Println("  WARNING: REAL ORDERS WILL BE PLACED ON BINANCE MAINNET.")
		fmt.Println("  Mode    : LIVE-SCAN (multi-symbol auto scanner)")
		fmt.Println("  Strategy: leverage_trend (low PF in backtest, see README)")
		fmt.Println("  Max concurrent positions:", *maxPositions)
		fmt.Println("  Min 24h quote volume    :", formatUSDT(*minQuoteVol))
		fmt.Println("  You assume full responsibility for fund loss.")
		fmt.Println("==========================================================")
	}
	if !live {
		fmt.Println("[mode] DRY-RUN")
	} else {
		fmt.Println("[mode] LIVE-SCAN")
	}
	fmt.Printf("[env ] %s    [interval] %s    [poll] %v    [maxPos] %d    [workers] %d\n",
		*envName, *interval, *pollEvery, *maxPositions, *workers)

	client := binance.NewFuturesClient(*envName)
	if client.APIKey == "" || client.Secret == "" {
		log.Fatal("BINANCE_API_KEY / BINANCE_API_SECRET are required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[exit] received signal, stopping loop...")
		cancel()
	}()

	if err := client.SyncTime(ctx); err != nil {
		log.Fatalf("sync time: %v", err)
	}

	dur, err := candleDuration(*interval)
	if err != nil {
		log.Fatal(err)
	}

	scannerStatePath := filepath.Join(*stateDir,
		fmt.Sprintf(".live_scan_state.%s.json", strings.ToLower(*envName)))
	sst := loadScannerState(scannerStatePath, *envName)

	for {
		if ctx.Err() != nil {
			return
		}
		next := nextCloseTime(time.Now().UTC(), dur).Add(5 * time.Second)
		sleep := time.Until(next)
		if sleep > 0 {
			fmt.Printf("[wait] next eval at %s UTC (in %v)\n", next.UTC().Format("15:04:05"), sleep.Round(time.Second))
			if !sleepWithCancel(ctx, sleep) {
				return
			}
		}
		if err := scanCycle(ctx, client, *interval, *lookback, *equityFlag,
			*maxDailyLossPct, live, cfg, *minQuoteVol, *maxPositions, *workers,
			*stateDir, *envName, scannerStatePath, &sst, *verbose); err != nil {
			fmt.Fprintf(os.Stderr, "[cycle err] %v\n", err)
		}
		if !sleepWithCancel(ctx, *pollEvery) {
			return
		}
	}
}

type scannerState struct {
	Env             string  `json:"env"`
	DailyDate       string  `json:"daily_date"`
	DailyStartEquit float64 `json:"daily_start_equity"`
	Frozen          bool    `json:"frozen"`
	FreezeReason    string  `json:"freeze_reason"`
}

func loadScannerState(path, env string) scannerState {
	st := scannerState{Env: env}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	st.Env = env
	return st
}

func saveScannerState(path string, st *scannerState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

type scanCandidate struct {
	Symbol   string
	Signal   strategy.Signal
	ATRRatio float64
}

func scanCycle(ctx context.Context, c *binance.FuturesClient, interval string,
	lookback int, equityOverride, maxDailyLossPct float64, live bool,
	cfg *strategy.Config, minQuoteVol float64, maxPositions, workers int,
	stateDir, envName, scannerStatePath string, sst *scannerState, verbose bool) error {

	dur, derr := candleDuration(interval)
	if derr != nil {
		return fmt.Errorf("candle duration: %w", derr)
	}

	balance, err := c.AccountBalance(ctx, "USDT")
	if err != nil {
		return fmt.Errorf("account balance: %w", err)
	}
	equity := balance.MarginBalance
	if equity == 0 {
		equity = balance.WalletBalance
	}
	if equityOverride > 0 {
		equity = equityOverride
	}
	today := time.Now().UTC().Format("2006-01-02")
	if sst.DailyDate != today || sst.DailyStartEquit == 0 {
		sst.DailyDate = today
		sst.DailyStartEquit = equity
		sst.Frozen = false
		sst.FreezeReason = ""
	}
	if sst.DailyStartEquit > 0 {
		drawdown := (sst.DailyStartEquit - equity) / sst.DailyStartEquit * 100
		if drawdown >= maxDailyLossPct {
			sst.Frozen = true
			sst.FreezeReason = fmt.Sprintf("daily DD %.2f%% >= %.2f%%", drawdown, maxDailyLossPct)
		}
	}
	saveScannerState(scannerStatePath, sst)

	fmt.Printf("\n[cycle %s] equity=%.4f USDT  available=%.4f  uPnL=%.4f  dailyStart=%.4f  frozen=%v\n",
		time.Now().UTC().Format("2006-01-02 15:04:05Z"),
		equity, balance.AvailableBalance, balance.CrossUnPnL, sst.DailyStartEquit, sst.Frozen)

	listed, err := c.ListUSDTPerpetuals(ctx, minQuoteVol)
	if err != nil {
		return fmt.Errorf("list symbols: %w", err)
	}
	fmt.Printf("[scan] universe=%d symbols (24h vol >= %s)\n", len(listed), formatUSDT(minQuoteVol))
	if len(listed) == 0 {
		return nil
	}

	positions, err := c.Positions(ctx, "")
	if err != nil {
		return fmt.Errorf("positions: %w", err)
	}
	open := map[string]binance.Position{}
	for _, p := range positions {
		if absFloat(p.PositionAmt) > 0 {
			open[strings.ToUpper(p.Symbol)] = p
		}
	}
	freeSlots := maxPositions - len(open)
	if freeSlots < 0 {
		freeSlots = 0
	}
	fmt.Printf("[pos ] open=%d  freeSlots=%d/%d\n", len(open), freeSlots, maxPositions)

	universe := map[string]struct{}{}
	for _, s := range listed {
		universe[s.Symbol] = struct{}{}
	}
	for sym := range open {
		universe[sym] = struct{}{}
	}

	type job struct{ symbol string }
	jobs := make(chan job, len(universe))
	for sym := range universe {
		jobs <- job{symbol: sym}
	}
	close(jobs)

	allocation := equity
	if maxPositions > 0 {
		allocation = equity / float64(maxPositions)
	}

	var (
		mu         sync.Mutex
		candidates []scanCandidate
		exits      []scanCandidate
	)
	wg := sync.WaitGroup{}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				sym := j.symbol
				symbolCfg := *cfg
				loadLeverageBrackets(ctx, c, sym, &symbolCfg)
				endT := time.Now().UTC()
				startT := endT.Add(-time.Duration(lookback) * dur)
				var candles []strategy.Candle
				cursor := startT
				for pg := 0; pg < 10; pg++ {
					part, perr := c.Klines(ctx, sym, interval, cursor, time.Time{}, 1500)
					if perr != nil {
						fmt.Fprintf(os.Stderr, "[%s] klines err: %v\n", sym, perr)
						break
					}
					if len(part) == 0 {
						break
					}
					candles = append(candles, part...)
					lastOpen := part[len(part)-1].OpenTime
					newCursor := lastOpen.Add(dur)
					if !newCursor.After(cursor) || !newCursor.Before(endT) || len(part) < 1500 {
						break
					}
					cursor = newCursor
					time.Sleep(120 * time.Millisecond)
				}
				if len(candles) == 0 {
					continue
				}
				fundingRate := 0.0
				if pi, perr := c.PremiumIndex(ctx, sym); perr == nil {
					fundingRate = pi.LastFundingRate
				}
				sig, serr := strategy.LatestSignal(candles,
					strategy.Context{Equity: allocation, FundingRate: fundingRate}, symbolCfg)
				if serr != nil {
					fmt.Fprintf(os.Stderr, "[%s] strategy err: %v\n", sym, serr)
					continue
				}
				atrRatio := computeATRRatio(candles, symbolCfg.ATRPeriod)
				if verbose {
					last := candles[len(candles)-1]
					fmt.Printf("[scan] %-15s close=%-12.6f ATR/close=%.4f funding=%.4f%% action=%-8s reason=%s\n",
						sym, last.Close, atrRatio, fundingRate*100, sig.Action, sig.Reason)
				}
				switch sig.Action {
				case strategy.ActionEnter:
					sig = roundSignal(ctx, c, sym, sig)
					mu.Lock()
					candidates = append(candidates, scanCandidate{Symbol: sym, Signal: sig, ATRRatio: atrRatio})
					mu.Unlock()
				case strategy.ActionExit:
					if _, held := open[sym]; held {
						mu.Lock()
						exits = append(exits, scanCandidate{Symbol: sym, Signal: sig, ATRRatio: atrRatio})
						mu.Unlock()
					}
				}
			}
		}()
	}
	wg.Wait()

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ATRRatio > candidates[j].ATRRatio
	})

	fmt.Printf("[sigs] ENTER=%d  EXIT=%d\n", len(candidates), len(exits))

	for _, ex := range exits {
		handleScanExit(ctx, c, ex.Symbol, ex.Signal, live, stateDir, envName, open[ex.Symbol])
	}

	if sst.Frozen {
		fmt.Println("[skip] kill-switch frozen; not opening new positions:", sst.FreezeReason)
		return nil
	}

	taken := 0
	for _, cand := range candidates {
		if taken >= freeSlots {
			fmt.Printf("[skip] %s ATR/close=%.4f -- no free slot (%d/%d)\n",
				cand.Symbol, cand.ATRRatio, len(open)+taken, maxPositions)
			continue
		}
		if _, alreadyOpen := open[cand.Symbol]; alreadyOpen {
			fmt.Printf("[skip] %s -- already in position\n", cand.Symbol)
			continue
		}
		if !live {
			fmt.Printf("[dry-run] %s WOULD ENTER %s qty=%.6f price~=%.2f stop=%.4f notional=%.2f lev=%.0f ATR/close=%.4f\n",
				cand.Symbol, cand.Signal.Side, cand.Signal.Quantity, cand.Signal.Price,
				cand.Signal.StopLoss, cand.Signal.Notional, cand.Signal.Leverage, cand.ATRRatio)
			taken++
			continue
		}
		fmt.Printf("[live] %s ENTERING %s qty=%.6f stop=%.4f ATR/close=%.4f\n",
			cand.Symbol, cand.Signal.Side, cand.Signal.Quantity, cand.Signal.StopLoss, cand.ATRRatio)
		res, oerr := c.OpenProtectedMarketPosition(ctx, cand.Symbol, cand.Signal)
		if oerr != nil {
			fmt.Fprintf(os.Stderr, "[%s] open err: %v\n", cand.Symbol, oerr)
			continue
		}
		fmt.Printf("[live] %s entry=%d stop=%d\n", cand.Symbol, res.Entry.OrderID, res.Stop.OrderID)
		taken++
	}
	if taken == 0 && len(candidates) == 0 {
		fmt.Println("[hold] no ENTER candidates this cycle")
	}
	return nil
}

func handleScanExit(ctx context.Context, c *binance.FuturesClient, symbol string,
	sig strategy.Signal, live bool, stateDir, envName string, pos binance.Position) {

	side := string(strategy.Long)
	if pos.PositionAmt < 0 {
		side = string(strategy.Short)
	}
	qty := absFloat(pos.PositionAmt)
	if qty <= 0 {
		return
	}
	if !live {
		fmt.Printf("[dry-run] %s WOULD EXIT %s qty=%.6f at market\n", symbol, side, qty)
		return
	}
	closeSide := "SELL"
	if side == string(strategy.Short) {
		closeSide = "BUY"
	}
	fmt.Printf("[live] %s EXITING %s qty=%.6f via %s market reduceOnly\n", symbol, side, qty, closeSide)
	if _, cerr := c.PlaceMarketOrderWithID(ctx, symbol, closeSide, qty, true,
		fmt.Sprintf("coin_exit_%d", time.Now().UnixMilli())); cerr != nil {
		fmt.Fprintf(os.Stderr, "[%s] close err: %v\n", symbol, cerr)
		return
	}
	_ = c.CancelAllOpenOrders(ctx, symbol)
}

func computeATRRatio(candles []strategy.Candle, period int) float64 {
	if len(candles) == 0 {
		return 0
	}
	if period < 2 {
		period = 14
	}
	atr := strategy.ATR(candles, period)
	if len(atr) == 0 {
		return 0
	}
	last := atr[len(atr)-1]
	cl := candles[len(candles)-1].Close
	if cl <= 0 {
		return 0
	}
	return last / cl
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func formatUSDT(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.2fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.2fK", v/1e3)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}
