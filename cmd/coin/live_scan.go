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
//  1. enumerate all USD-M PERPETUAL contracts quoted in USDT via exchangeInfo
//     joined with /fapi/v1/ticker/24hr;
//  2. drop symbols with 24h quote volume below -min-quote-volume USDT;
//  3. on every candle close, evaluate strategy.LatestSignal for each symbol
//     concurrently (capped by -workers);
//  4. take all ENTER signals, sort by ATR/close descending, cap by
//     max-positions minus already-held positions, and place real orders;
//  5. EXIT signals on currently-held symbols always run.
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
	runImmediately := fs.Bool("run-immediately", true, "run one scan immediately on startup before waiting for the next candle close")
	useKlineCache := fs.Bool("kline-cache", true, "cache klines locally and only fetch missing bars after the first full scan")
	wsKlineCache := fs.Bool("ws-kline-cache", false, "start an internal websocket kline cache updater in the same process")
	wsChunkSize := fs.Int("ws-chunk-size", 200, "websocket kline streams per connection when -ws-kline-cache is true")
	wsFlushEvery := fs.Duration("ws-flush", 5*time.Second, "websocket kline cache flush interval")
	dryRun := fs.Bool("dry-run", true, "if true, NEVER sends real orders")
	confirm := fs.Bool("i-understand-risk", false, "must be true to enable real orders")
	maxDailyLossPct := fs.Float64("max-daily-loss-pct", 5.0, "freeze entries for the rest of UTC day if equity DD this %% from day start")
	stateDir := fs.String("state-dir", ".", "directory for live state json files")
	configPath := fs.String("config", "", "flat YAML or JSON strategy config path")
	minQuoteVol := fs.Float64("min-quote-volume", 50_000_000, "minimum 24h USDT quote volume per symbol")
	maxPositions := fs.Int("max-positions", 3, "maximum concurrent positions across all symbols")
	workers := fs.Int("workers", 8, "concurrent symbol evaluators per cycle")
	verbose := fs.Bool("verbose", false, "print per-symbol reason for every NO_TRADE")
	marketFilter := fs.Bool("market-filter", true, "filter entries by BTC trend: longs only above BTC trend EMA, shorts only below")
	marketSymbol := fs.String("market-symbol", "BTCUSDT", "symbol used for market regime filter")
	maxSpreadBps := fs.Float64("max-spread-bps", 8.0, "skip live entries whose book spread exceeds this many bps; 0 disables")
	requestDelay := fs.Duration("request-delay", 0, "minimum delay between Binance API requests during scan; useful when scanning all contracts")
	replaceWhenFull := fs.Bool("replace-when-full", false, "when slots are full, scan for stronger entries and replace the weakest non-winning position")
	replaceScanTop := fs.Int("replace-scan-top", 40, "top N symbols by 24h base volume to scan when -replace-when-full is true; 0 scans all")
	replaceMinEdge := fs.Float64("replace-min-edge", 0.012, "minimum ATR/close edge required to replace a full-slot position")
	replaceMaxHeldPnL := fs.Float64("replace-max-held-pnl", 0.0, "only replace held positions whose unrealized PnL percent is <= this value")
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
	if *wsKlineCache {
		if !*useKlineCache {
			log.Fatal("-ws-kline-cache requires -kline-cache=true")
		}
		listed, err := client.ListUSDTPerpetuals(ctx, *minQuoteVol)
		if err != nil {
			log.Fatalf("ws kline symbols: %v", err)
		}
		sort.SliceStable(listed, func(i, j int) bool {
			return listed[i].Volume > listed[j].Volume
		})
		symbols := make([]string, 0, len(listed))
		for _, s := range listed {
			symbols = append(symbols, s.Symbol)
		}
		if len(symbols) == 0 {
			log.Fatal("ws kline symbols: empty universe")
		}
		cache := newKlineCache(*stateDir)
		store := newWSKlineStore(cache, *interval, *lookback, dur)
		store.load(symbols, *interval)
		startKlineWSCache(ctx, *envName, *interval, *wsChunkSize, *wsFlushEvery, symbols, store)
	}

	scannerStatePath := filepath.Join(*stateDir,
		fmt.Sprintf(".live_scan_state.%s.json", strings.ToLower(*envName)))
	sst := loadScannerState(scannerStatePath, *envName)

	firstCycle := true
	for {
		if ctx.Err() != nil {
			return
		}
		if firstCycle && *runImmediately {
			fmt.Println("[run ] immediate first eval; later cycles wait for candle close")
		} else {
			next := nextCloseTime(time.Now().UTC(), dur).Add(5 * time.Second)
			sleep := time.Until(next)
			if sleep > 0 {
				fmt.Printf("[wait] next eval at %s UTC (in %v)\n", next.UTC().Format("15:04:05"), sleep.Round(time.Second))
				if !sleepWithCancel(ctx, sleep) {
					return
				}
			}
		}
		if err := scanCycle(ctx, client, *interval, *lookback, *equityFlag,
			*maxDailyLossPct, live, cfg, *minQuoteVol, *maxPositions, *workers,
			*stateDir, *envName, scannerStatePath, &sst, *verbose, *marketFilter, *marketSymbol, *maxSpreadBps, *requestDelay,
			*replaceWhenFull, *replaceScanTop, *replaceMinEdge, *replaceMaxHeldPnL, *useKlineCache); err != nil {
			fmt.Fprintf(os.Stderr, "[cycle err] %v\n", err)
		}
		if !sleepWithCancel(ctx, *pollEvery) {
			return
		}
		firstCycle = false
	}
}

type scannerState struct {
	Env             string                     `json:"env"`
	DailyDate       string                     `json:"daily_date"`
	DailyStartEquit float64                    `json:"daily_start_equity"`
	PeakEquity      float64                    `json:"peak_equity"`
	Frozen          bool                       `json:"frozen"`
	FreezeReason    string                     `json:"freeze_reason"`
	Positions       map[string]managedPosition `json:"positions"`
}

func loadScannerState(path, env string) scannerState {
	st := scannerState{Env: env, Positions: map[string]managedPosition{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	st.Env = env
	if st.Positions == nil {
		st.Positions = map[string]managedPosition{}
	}
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

type heldScore struct {
	Symbol   string
	ATRRatio float64
	PnLPct   float64
}

type requestGate struct {
	mu    sync.Mutex
	delay time.Duration
	next  time.Time
}

func (g *requestGate) Wait(ctx context.Context) bool {
	if g == nil || g.delay <= 0 {
		return true
	}
	g.mu.Lock()
	now := time.Now()
	wait := g.next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	g.next = now.Add(wait).Add(g.delay)
	g.mu.Unlock()
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func scanCycle(ctx context.Context, c *binance.FuturesClient, interval string,
	lookback int, equityOverride, maxDailyLossPct float64, live bool,
	cfg *strategy.Config, minQuoteVol float64, maxPositions, workers int,
	stateDir, envName, scannerStatePath string, sst *scannerState, verbose bool,
	marketFilter bool, marketSymbol string, maxSpreadBps float64, requestDelay time.Duration,
	replaceWhenFull bool, replaceScanTop int, replaceMinEdge float64, replaceMaxHeldPnL float64, useKlineCache bool) error {
	if sst.Positions == nil {
		sst.Positions = map[string]managedPosition{}
	}

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
	if sst.PeakEquity <= 0 || equity > sst.PeakEquity {
		sst.PeakEquity = equity
	}
	if sst.DailyStartEquit > 0 {
		drawdown := (sst.DailyStartEquit - equity) / sst.DailyStartEquit * 100
		if drawdown >= maxDailyLossPct {
			sst.Frozen = true
			sst.FreezeReason = fmt.Sprintf("daily DD %.2f%% >= %.2f%%", drawdown, maxDailyLossPct)
		}
	}
	if sst.PeakEquity > 0 {
		drawdown := (sst.PeakEquity - equity) / sst.PeakEquity
		if drawdown >= cfg.MaxDrawdown {
			sst.Frozen = true
			sst.FreezeReason = fmt.Sprintf("max DD %.2f%% >= %.2f%%", drawdown*100, cfg.MaxDrawdown*100)
		}
	}
	saveScannerState(scannerStatePath, sst)

	fmt.Printf("\n[cycle %s] equity=%.4f USDT  available=%.4f  uPnL=%.4f  dailyStart=%.4f  frozen=%v\n",
		time.Now().UTC().Format("2006-01-02 15:04:05Z"),
		equity, balance.AvailableBalance, balance.CrossUnPnL, sst.DailyStartEquit, sst.Frozen)

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

	if sst.Frozen {
		if len(open) > 0 {
			fmt.Println("[risk] kill-switch frozen; closing open positions:", sst.FreezeReason)
			for sym, pos := range open {
				if closeExchangePosition(ctx, c, sym, pos, live, sst.FreezeReason) {
					delete(sst.Positions, sym)
				}
			}
			saveScannerState(scannerStatePath, sst)
		} else {
			fmt.Println("[skip] kill-switch frozen; no open positions:", sst.FreezeReason)
		}
		return nil
	}

	fullSlots := freeSlots == 0 && len(open) > 0
	var listed []binance.ListedSymbol
	if freeSlots > 0 || (fullSlots && replaceWhenFull) {
		listed, err = c.ListUSDTPerpetuals(ctx, minQuoteVol)
		if err != nil {
			return fmt.Errorf("list symbols: %w", err)
		}
		sort.SliceStable(listed, func(i, j int) bool {
			return listed[i].Volume > listed[j].Volume
		})
		if fullSlots && replaceWhenFull && replaceScanTop > 0 && len(listed) > replaceScanTop {
			listed = listed[:replaceScanTop]
		}
		fmt.Printf("[scan] universe=%d symbols (24h vol >= %s)\n", len(listed), formatUSDT(minQuoteVol))
		if len(listed) == 0 {
			return nil
		}
	}

	allowLong, allowShort := true, true
	if marketFilter {
		allowLong, allowShort = marketRegime(ctx, c, strings.ToUpper(marketSymbol), interval, lookback, dur, *cfg)
		fmt.Printf("[mkt ] %s filter allowLong=%v allowShort=%v\n", strings.ToUpper(marketSymbol), allowLong, allowShort)
	}

	universe := map[string]struct{}{}
	if fullSlots && !replaceWhenFull {
		fmt.Println("[scan] position slots full; managing open positions only")
		for sym := range open {
			universe[sym] = struct{}{}
		}
	} else {
		if fullSlots && replaceWhenFull {
			fmt.Printf("[scan] position slots full; scanning %d symbols for replacement candidates\n", len(listed))
		}
		for _, s := range listed {
			universe[s.Symbol] = struct{}{}
		}
		for sym := range open {
			universe[sym] = struct{}{}
		}
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
	gate := &requestGate{delay: requestDelay}
	fundingBySymbol := map[string]float64{}
	if len(universe) > 0 {
		if !gate.Wait(ctx) {
			return nil
		}
		premiums, perr := c.PremiumIndexes(ctx)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "warning: premium index map unavailable, funding filters use 0: %v\n", perr)
		} else {
			for sym, pi := range premiums {
				fundingBySymbol[sym] = pi.LastFundingRate
			}
		}
	}
	var klines *klineCache
	if useKlineCache {
		klines = newKlineCache(stateDir)
	}

	var (
		mu         sync.Mutex
		candidates []scanCandidate
		exits      []scanCandidate
		heldScores []heldScore
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
				var candles []strategy.Candle
				var perr error
				if klines != nil {
					candles, perr = klines.Load(ctx, c, sym, interval, lookback, dur, gate)
				} else {
					candles, perr = fetchKlinesLookback(ctx, c, sym, interval, lookback, dur, gate)
				}
				if perr != nil {
					fmt.Fprintf(os.Stderr, "[%s] klines err: %v\n", sym, perr)
					continue
				}
				if len(candles) == 0 {
					continue
				}
				warmup := strategy.WarmupBarsForCandles(candles, symbolCfg)
				if len(candles) < warmup {
					if _, held := open[sym]; held {
						fmt.Fprintf(os.Stderr, "[%s] insufficient history for position management: need %d candles, got %d\n",
							sym, warmup, len(candles))
					} else if verbose {
						last := candles[len(candles)-1]
						fmt.Printf("[scan] %-15s close=%-12.6f ATR/close=%.4f funding=%.4f%% action=%-8s reason=insufficient history: need %d candles, got %d\n",
							sym, last.Close, computeATRRatio(candles, symbolCfg.ATRPeriod), 0.0,
							strategy.ActionNone, warmup, len(candles))
					}
					continue
				}
				fundingRate := fundingBySymbol[strings.ToUpper(sym)]
				if heldPos, held := open[sym]; held {
					atrRatio := computeATRRatio(candles, symbolCfg.ATRPeriod)
					mu.Lock()
					mp := sst.Positions[sym]
					mu.Unlock()
					if manageOpenPosition(ctx, c, sym, heldPos, candles, fundingRate, symbolCfg, live, &mp) {
						mu.Lock()
						if mp.StopLoss <= 0 {
							delete(sst.Positions, sym)
						} else {
							sst.Positions[sym] = mp
						}
						mu.Unlock()
					}
					if mp.StopLoss <= 0 {
						continue
					}
					mu.Lock()
					heldScores = append(heldScores, heldScore{
						Symbol:   sym,
						ATRRatio: atrRatio,
						PnLPct:   positionPnLPct(heldPos),
					})
					mu.Unlock()
					continue
				}
				if err := symbolCfg.Validate(); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] strategy err: %v\n", sym, err)
					continue
				}
				ind := strategy.BuildIndicators(candles, symbolCfg)
				i := len(candles) - 1
				side, reason := strategy.EntrySide(candles, ind, i, symbolCfg, fundingRate)
				sig := strategy.Signal{
					Time:   candles[i].CloseTime,
					Action: strategy.ActionNone,
					Side:   strategy.Flat,
					Price:  candles[i].Close,
					Reason: reason,
				}
				if side != strategy.Flat {
					stop, ok, stopReason := strategy.StructureStop(candles, ind, i, side, symbolCfg)
					if !ok {
						sig.Reason = stopReason
					} else {
						if !gate.Wait(ctx) {
							return
						}
						loadLeverageBrackets(ctx, c, sym, &symbolCfg)
						sig = strategy.BuildEntrySignal(candles[i], side, allocation, ind.ATR[i], stop, 1, fundingRate, symbolCfg)
						if sig.Action == strategy.ActionEnter {
							sig.Reason = reason + "; " + stopReason
						}
					}
				}
				atrRatio := computeATRRatio(candles, symbolCfg.ATRPeriod)
				if verbose {
					last := candles[len(candles)-1]
					fmt.Printf("[scan] %-15s close=%-12.6f ATR/close=%.4f funding=%.4f%% action=%-8s reason=%s\n",
						sym, last.Close, atrRatio, fundingRate*100, sig.Action, sig.Reason)
				}
				switch sig.Action {
				case strategy.ActionEnter:
					if sig.Side == strategy.Long && !allowLong {
						if verbose {
							fmt.Printf("[skip] %-15s long blocked by market filter\n", sym)
						}
						continue
					}
					if sig.Side == strategy.Short && !allowShort {
						if verbose {
							fmt.Printf("[skip] %-15s short blocked by market filter\n", sym)
						}
						continue
					}
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
	saveScannerState(scannerStatePath, sst)

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ATRRatio > candidates[j].ATRRatio
	})

	fmt.Printf("[sigs] ENTER=%d  EXIT=%d\n", len(candidates), len(exits))

	for _, ex := range exits {
		handleScanExit(ctx, c, ex.Symbol, ex.Signal, live, stateDir, envName, open[ex.Symbol])
		delete(sst.Positions, ex.Symbol)
	}
	saveScannerState(scannerStatePath, sst)

	if sst.Frozen {
		fmt.Println("[skip] kill-switch frozen; not opening new positions:", sst.FreezeReason)
		return nil
	}

	if fullSlots && replaceWhenFull && len(candidates) > 0 {
		sort.SliceStable(heldScores, func(i, j int) bool {
			if heldScores[i].PnLPct == heldScores[j].PnLPct {
				return heldScores[i].ATRRatio < heldScores[j].ATRRatio
			}
			return heldScores[i].PnLPct < heldScores[j].PnLPct
		})
		if len(heldScores) == 0 {
			fmt.Println("[replace] no managed held position score available")
			return nil
		}
		best := candidates[0]
		weakest := heldScores[0]
		if weakest.PnLPct > replaceMaxHeldPnL {
			fmt.Printf("[replace] best=%s ATR/close=%.4f but weakest held %s uPnL=%.2f%% > %.2f%%; not replacing winner\n",
				best.Symbol, best.ATRRatio, weakest.Symbol, weakest.PnLPct, replaceMaxHeldPnL)
			return nil
		}
		if best.ATRRatio < weakest.ATRRatio+replaceMinEdge {
			fmt.Printf("[replace] best=%s ATR/close=%.4f not enough edge over weakest %s ATR/close=%.4f + %.4f\n",
				best.Symbol, best.ATRRatio, weakest.Symbol, weakest.ATRRatio, replaceMinEdge)
			return nil
		}
		if _, alreadyOpen := open[best.Symbol]; alreadyOpen {
			fmt.Printf("[replace] best candidate %s is already open\n", best.Symbol)
			return nil
		}
		if !live {
			fmt.Printf("[dry-run] WOULD REPLACE %s uPnL=%.2f%% ATR/close=%.4f WITH %s %s ATR/close=%.4f qty=%.6f stop=%.4f\n",
				weakest.Symbol, weakest.PnLPct, weakest.ATRRatio, best.Symbol, best.Signal.Side, best.ATRRatio, best.Signal.Quantity, best.Signal.StopLoss)
			return nil
		}
		if maxSpreadBps > 0 {
			if !gate.Wait(ctx) {
				return nil
			}
			spread, ok := bookSpreadBps(ctx, c, best.Symbol)
			if !ok {
				fmt.Printf("[replace] skip %s -- book ticker unavailable\n", best.Symbol)
				return nil
			}
			if spread > maxSpreadBps {
				fmt.Printf("[replace] skip %s -- spread %.2fbps > %.2fbps\n", best.Symbol, spread, maxSpreadBps)
				return nil
			}
		}
		fmt.Printf("[replace] closing %s uPnL=%.2f%% ATR/close=%.4f for %s %s ATR/close=%.4f\n",
			weakest.Symbol, weakest.PnLPct, weakest.ATRRatio, best.Symbol, best.Signal.Side, best.ATRRatio)
		if !closeExchangePosition(ctx, c, weakest.Symbol, open[weakest.Symbol], live, "replace with stronger candidate "+best.Symbol) {
			return nil
		}
		delete(sst.Positions, weakest.Symbol)
		res, oerr := c.OpenProtectedMarketPosition(ctx, best.Symbol, best.Signal)
		if oerr != nil {
			fmt.Fprintf(os.Stderr, "[%s] replacement open err: %v\n", best.Symbol, oerr)
			saveScannerState(scannerStatePath, sst)
			return nil
		}
		fmt.Printf("[replace] %s entry=%d stop=%d\n", best.Symbol, res.Entry.OrderID, res.Stop.OrderID)
		sst.Positions[best.Symbol] = managedPosition{
			Symbol:     best.Symbol,
			Side:       string(best.Signal.Side),
			Quantity:   best.Signal.Quantity,
			EntryPrice: best.Signal.Price,
			StopLoss:   best.Signal.StopLoss,
		}
		saveScannerState(scannerStatePath, sst)
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
		if maxSpreadBps > 0 {
			if !gate.Wait(ctx) {
				return nil
			}
			spread, ok := bookSpreadBps(ctx, c, cand.Symbol)
			if !ok {
				fmt.Printf("[skip] %s -- book ticker unavailable\n", cand.Symbol)
				continue
			}
			if spread > maxSpreadBps {
				fmt.Printf("[skip] %s -- spread %.2fbps > %.2fbps\n", cand.Symbol, spread, maxSpreadBps)
				continue
			}
		}
		fmt.Printf("[live] %s ENTERING %s qty=%.6f stop=%.4f ATR/close=%.4f\n",
			cand.Symbol, cand.Signal.Side, cand.Signal.Quantity, cand.Signal.StopLoss, cand.ATRRatio)
		res, oerr := c.OpenProtectedMarketPosition(ctx, cand.Symbol, cand.Signal)
		if oerr != nil {
			fmt.Fprintf(os.Stderr, "[%s] open err: %v\n", cand.Symbol, oerr)
			continue
		}
		fmt.Printf("[live] %s entry=%d stop=%d\n", cand.Symbol, res.Entry.OrderID, res.Stop.OrderID)
		sst.Positions[cand.Symbol] = managedPosition{
			Symbol:     cand.Symbol,
			Side:       string(cand.Signal.Side),
			Quantity:   cand.Signal.Quantity,
			EntryPrice: cand.Signal.Price,
			StopLoss:   cand.Signal.StopLoss,
		}
		saveScannerState(scannerStatePath, sst)
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
	if _, cerr := c.PlaceMarketOrderWithPositionSide(ctx, symbol, closeSide, qty, true, side,
		fmt.Sprintf("coin_exit_%d", time.Now().UnixMilli())); cerr != nil {
		fmt.Fprintf(os.Stderr, "[%s] close err: %v\n", symbol, cerr)
		return
	}
	_ = c.CancelAllProtectionOrders(ctx, symbol)
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

func marketRegime(ctx context.Context, c *binance.FuturesClient, symbol, interval string, lookback int, dur time.Duration, cfg strategy.Config) (bool, bool) {
	end := time.Now().UTC()
	start := end.Add(-time.Duration(lookback) * dur)
	candles, err := c.KlinesRange(ctx, symbol, interval, start, end)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mkt ] %s regime unavailable: %v\n", symbol, err)
		return true, true
	}
	warmup := strategy.WarmupBarsForCandles(candles, cfg)
	if len(candles) < warmup {
		fmt.Fprintf(os.Stderr, "[mkt ] %s regime unavailable: need %d candles, got %d\n",
			symbol, warmup, len(candles))
		return true, true
	}
	closes := strategy.Closes(candles)
	trend := strategy.EMA(closes, cfg.TrendEMA)
	i := len(candles) - 1
	if trend[i] <= 0 {
		return true, true
	}
	close := candles[i].Close
	if close >= trend[i] {
		return true, false
	}
	return false, true
}

func bookSpreadBps(ctx context.Context, c *binance.FuturesClient, symbol string) (float64, bool) {
	bt, err := c.FuturesBookTicker(ctx, symbol)
	if err != nil {
		return 0, false
	}
	mid := (bt.BidPrice + bt.AskPrice) / 2
	if mid <= 0 {
		return 0, false
	}
	return (bt.AskPrice - bt.BidPrice) / mid * 1e4, true
}

func positionPnLPct(pos binance.Position) float64 {
	entry := pos.EntryPrice
	if entry <= 0 {
		entry = pos.MarkPrice
	}
	notional := absFloat(pos.PositionAmt) * entry
	if notional <= 0 {
		return 0
	}
	return pos.UnrealizedProfit / notional * 100
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
