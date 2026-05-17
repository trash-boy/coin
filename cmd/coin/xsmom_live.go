package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
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
	"coin/internal/strategy/xsmom"
)

// xsmomLiveFlags holds the flags specific to the live runner.
type xsmomLiveFlags struct {
	xsmomFlags

	dryRun           bool
	anchorWeekday    int     // 0=Sunday..6=Saturday; rebalance only when (today.Weekday()) == anchor && (today-genesis).days%hold==0
	checkInterval    time.Duration
	maxSymbolFracEq  float64 // single-symbol notional cap as fraction of equity
	maxGrossFracEq   float64 // total gross notional cap as fraction of equity
	maxBasketTurnover float64 // 0..1; if > this, alert and skip
	resizeThreshold  float64 // re-use 5% by default; override here
	flipDelay        time.Duration
	logFile          string
	useAccountEquity bool // if true, fetch USDT MarginBalance and override -equity
	once             bool // run one rebalance check immediately and exit
	leverageInt      int  // exchange leverage to set on each symbol (0 = skip)
}

func xsmomLiveFlagSet(name string) (*flag.FlagSet, *xsmomLiveFlags) {
	fs, base := xsmomFlagSet(name)
	f := &xsmomLiveFlags{xsmomFlags: *base}
	// rebind base flags onto fs into f.xsmomFlags fields - tricky, so we
	// actually need to wire fresh because xsmomFlagSet captured *base.
	// Simpler: re-define a fresh set here with shared defaults.
	_ = fs
	fs2 := flag.NewFlagSet(name, flag.ExitOnError)
	cfg := xsmom.DefaultConfig()
	fs2.StringVar(&f.env, "env", "prod", "prod or testnet")
	fs2.IntVar(&f.top, "top", 100, "top N symbols by 24h quote volume")
	fs2.Float64Var(&f.minListVol, "min-listing-vol-usdt", 5_000_000, "min 24h quote-vol USD entering the candidate list")
	fs2.IntVar(&f.days, "days", 360, "lookback days for panel construction")
	fs2.StringVar(&f.interval, "interval", "30m", "source kline interval")
	fs2.IntVar(&f.maxConcurrent, "max-concurrent", 6, "max concurrent klines fetches")
	fs2.Float64Var(&f.equity, "equity", 10000, "equity in USDT (overridden by -use-account-equity)")
	fs2.IntVar(&f.lookback, "lookback", cfg.LookbackDays, "ranking lookback days")
	fs2.IntVar(&f.hold, "hold", cfg.HoldDays, "rebalance period in days")
	fs2.IntVar(&f.topN, "topn", cfg.TopN, "longs basket size")
	fs2.IntVar(&f.botN, "botn", cfg.BotN, "shorts basket size (0 = long-only)")
	fs2.Float64Var(&f.leverage, "leverage", cfg.Leverage, "gross leverage (avoid > 2)")
	fs2.IntVar(&f.minHist, "min-hist-days", cfg.MinHistDays, "min listing-age days for eligibility")
	fs2.Float64Var(&f.minQuoteVol, "min-quote-vol-usd", cfg.MinQuoteVolUSD, "rolling 30d quote-vol floor in USD")
	fs2.Float64Var(&f.feeBps, "fee-bps", cfg.FeeBps, "per-leg taker fee in basis points")
	fs2.Float64Var(&f.slipBps, "slip-bps", cfg.SlipBps, "per-leg slippage in basis points")
	fs2.BoolVar(&f.includeFunding, "funding", cfg.IncludeFunding, "include funding cost in signal")
	fs2.BoolVar(&f.symbolsOverride, "symbols-override", false, "treat -symbols as the universe directly")
	fs2.StringVar(&f.symbols, "symbols", "", "comma-separated symbols when -symbols-override")

	fs2.BoolVar(&f.dryRun, "dry-run", true, "do not send orders; print the plan only")
	fs2.IntVar(&f.anchorWeekday, "anchor-weekday", 1, "weekday to rebalance: 0=Sun..6=Sat (default Mon)")
	fs2.DurationVar(&f.checkInterval, "check-interval", 5*time.Minute, "polling interval to re-check rebalance time")
	fs2.Float64Var(&f.maxSymbolFracEq, "max-symbol-frac", 0.20, "single-symbol notional cap as fraction of equity")
	fs2.Float64Var(&f.maxGrossFracEq, "max-gross-frac", 1.10, "total gross notional cap as fraction of equity*leverage")
	fs2.Float64Var(&f.maxBasketTurnover, "max-turnover", 0.80, "max fraction of basket replaced; abort above")
	fs2.Float64Var(&f.resizeThreshold, "resize-threshold", 0.05, "skip resize when |delta|/target <= this")
	fs2.DurationVar(&f.flipDelay, "flip-delay", 5*time.Second, "wait between close-leg and open-leg orders")
	fs2.StringVar(&f.logFile, "log", "~/files/xsmom-trades.log", "JSON-lines log file for executed rebalances")
	fs2.BoolVar(&f.useAccountEquity, "use-account-equity", false, "fetch USDT margin balance and override -equity")
	fs2.BoolVar(&f.once, "once", false, "run one rebalance check now (ignoring weekday gate) and exit")
	fs2.IntVar(&f.leverageInt, "exchange-leverage", 3, "exchange-side leverage to set on each symbol; 0 = skip")
	return fs2, f
}

// runXSMomLive is the always-on runner. It wakes every check-interval,
// decides whether today is a rebalance day, and if so:
//
//	1. fetches current positions from Binance
//	2. computes target signal + deltas
//	3. runs risk gates
//	4. (unless dry-run) sends close-leg market orders, waits flip-delay,
//	   then sends open-leg market orders
//	5. appends a JSON-lines record of the rebalance
func runXSMomLive(args []string) {
	fs, f := xsmomLiveFlagSet("xsmom-live")
	_ = fs.Parse(args)
	cfg := f.toConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	if f.anchorWeekday < 0 || f.anchorWeekday > 6 {
		log.Fatalf("invalid -anchor-weekday %d", f.anchorWeekday)
	}
	if f.maxSymbolFracEq <= 0 || f.maxGrossFracEq <= 0 {
		log.Fatalf("risk caps must be > 0")
	}

	logPath := expandHome(f.logFile)
	if err := ensureLogDir(logPath); err != nil {
		log.Fatalf("log dir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		fmt.Fprintf(os.Stderr, "\nreceived %s, shutting down...\n", s)
		cancel()
	}()

	client := binance.NewFuturesClient(f.env)
	if client.APIKey == "" || client.Secret == "" {
		log.Fatal("BINANCE_API_KEY / BINANCE_API_SECRET must be set for xsmom-live")
	}
	if err := client.SyncTime(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: server time sync failed: %v\n", err)
	}

	mode := "DRY-RUN"
	if !f.dryRun {
		mode = "LIVE"
	}
	fmt.Printf("xsmom-live starting in %s mode (env=%s, anchor=%s)\n",
		mode, f.env, weekdayName(f.anchorWeekday))
	fmt.Printf("config: L%d/S%d lookback=%dd hold=%dd lev=%.2fx exchange-lev=%dx\n",
		cfg.TopN, cfg.BotN, cfg.LookbackDays, cfg.HoldDays, cfg.Leverage, f.leverageInt)
	fmt.Printf("risk caps: per-symbol=%.0f%% equity, gross=%.0f%% (equity*lev), max-turnover=%.0f%%\n",
		f.maxSymbolFracEq*100, f.maxGrossFracEq*100, f.maxBasketTurnover*100)
	fmt.Printf("log: %s\n", logPath)

	if f.once {
		_ = runOneRebalance(ctx, client, cfg, f, logPath, true)
		return
	}

	var lastRebalanceDay time.Time
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		now := time.Now().UTC()
		if shouldRebalance(now, f.anchorWeekday, f.hold, lastRebalanceDay) {
			if ok := runOneRebalance(ctx, client, cfg, f, logPath, false); ok {
				lastRebalanceDay = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(f.checkInterval):
		}
	}
}

// shouldRebalance returns true once per anchor-weekday-aligned UTC day.
// Within the day, it triggers around 00:00..00:30 UTC and only if not
// already done today.
func shouldRebalance(now time.Time, anchorWeekday, holdDays int, last time.Time) bool {
	if int(now.Weekday()) != anchorWeekday {
		return false
	}
	// Trigger window: first 30 minutes of the UTC day to avoid running
	// mid-day if the runner just started.
	if now.Hour() != 0 || now.Minute() > 30 {
		return false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if last.Equal(today) {
		return false
	}
	return true
}

// runOneRebalance executes a single end-to-end cycle. It returns true
// if the rebalance ran (whether dry or live), false on hard error or
// risk-gate abort so the scheduler can retry next interval.
func runOneRebalance(ctx context.Context, client *binance.FuturesClient, cfg xsmom.Config, f *xsmomLiveFlags, logPath string, force bool) bool {
	t0 := time.Now().UTC()
	fmt.Printf("\n[%s] rebalance cycle start (force=%v)\n", t0.Format(time.RFC3339), force)

	// 1) Equity (optional auto-fetch)
	equity := f.equity
	if f.useAccountEquity {
		bal, err := client.AccountBalance(ctx, "USDT")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: AccountBalance failed: %v -- using -equity flag\n", err)
		} else {
			equity = bal.MarginBalance
			fmt.Printf("  account equity (USDT MarginBalance) = %.2f\n", equity)
		}
	}
	if equity <= 0 {
		fmt.Fprintf(os.Stderr, "  ABORT: equity <= 0\n")
		return false
	}

	// 2) Universe + panel
	universe, err := f.resolveUniverse(ctx, client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ABORT: universe: %v\n", err)
		return false
	}
	fmt.Printf("  universe: %d symbols\n", len(universe))

	panel, failures, err := xsmom.BuildPanelFromBinance(ctx, client, xsmom.FetchOptions{
		Symbols:        universe,
		Days:           f.days,
		Interval:       f.interval,
		MaxConcurrent:  f.maxConcurrent,
		IncludeFunding: f.includeFunding,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ABORT: panel: %v\n", err)
		return false
	}
	if len(failures) > 0 {
		fmt.Printf("  panel: %d symbols x %d days (%d skipped)\n",
			len(panel.Symbols), len(panel.Days), len(failures))
	} else {
		fmt.Printf("  panel: %d symbols x %d days\n", len(panel.Symbols), len(panel.Days))
	}

	// 3) Latest signal
	signalSet, err := xsmom.LatestSignal(panel, cfg, equity)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ABORT: signal: %v\n", err)
		return false
	}
	if signalSet.Skipped {
		fmt.Printf("  SIGNAL SKIPPED: %s\n", signalSet.SkipReason)
		return false
	}
	fmt.Printf("  signal: %d targets, eligible universe=%d\n", len(signalSet.Targets), signalSet.UniverseSize)

	// 4) Current positions
	current, marks, err := fetchCurrentXSMomPositions(ctx, client, panel.Symbols, signalSet.Targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ABORT: fetch positions: %v\n", err)
		return false
	}

	// 5) Order deltas
	deltas := xsmom.ComputeOrderDeltas(signalSet, current)
	deltas = filterTinyResizes(deltas, current, f.resizeThreshold)
	if len(deltas) == 0 {
		fmt.Printf("  no order deltas (current matches target within %.0f%% threshold)\n", f.resizeThreshold*100)
		writeRebalanceLog(logPath, rebalanceRecord{
			Time:    t0,
			DryRun:  f.dryRun,
			Note:    "no-op: deltas empty",
			Targets: signalSet.Targets,
		})
		return true
	}

	// 6) Risk gates
	if reason, ok := riskGate(signalSet.Targets, deltas, current, equity, cfg, f); !ok {
		fmt.Fprintf(os.Stderr, "  ABORT (risk): %s\n", reason)
		writeRebalanceLog(logPath, rebalanceRecord{
			Time:   t0,
			DryRun: f.dryRun,
			Note:   "risk-gate-abort: " + reason,
		})
		return false
	}

	// 7) Print plan
	fmt.Printf("  plan: %d order deltas\n", len(deltas))
	for _, d := range deltas {
		fmt.Printf("    %-12s side=%-5s targetNotional=%10.2f  reason=%s\n",
			d.Symbol, d.Side, d.TargetNotional, d.Reason)
	}

	if f.dryRun {
		fmt.Printf("  DRY-RUN: not sending orders\n")
		writeRebalanceLog(logPath, rebalanceRecord{
			Time:    t0,
			DryRun:  true,
			Targets: signalSet.Targets,
			Deltas:  deltas,
			Note:    "dry-run plan",
		})
		return true
	}

	// 8) Execute: closes first, then opens. Flips become close+open.
	closeOrders, openOrders := splitDeltas(deltas, current)
	executed := executeOrders(ctx, client, closeOrders, marks, true)
	if len(openOrders) > 0 && f.flipDelay > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(f.flipDelay):
		}
	}
	executed = append(executed, executeOrders(ctx, client, openOrders, marks, false)...)

	// 9) Set exchange leverage on opens (best-effort, idempotent)
	if f.leverageInt > 0 {
		setLeverages(ctx, client, openOrders, f.leverageInt)
	}

	writeRebalanceLog(logPath, rebalanceRecord{
		Time:     t0,
		DryRun:   false,
		Equity:   equity,
		Targets:  signalSet.Targets,
		Deltas:   deltas,
		Executed: executed,
		Note:     "live rebalance",
	})
	fmt.Printf("  done: %d orders executed (%s)\n", len(executed), time.Since(t0).Round(time.Millisecond))
	return true
}

// fetchCurrentXSMomPositions queries /positionRisk for all panel symbols
// and union'd target symbols, returning xsmom.CurrentPosition slice plus
// a side-map of mark prices for qty conversion later.
func fetchCurrentXSMomPositions(ctx context.Context, client *binance.FuturesClient, panelSyms []string, targets []xsmom.Target) ([]xsmom.CurrentPosition, map[string]float64, error) {
	all, err := client.Positions(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	want := map[string]struct{}{}
	for _, s := range panelSyms {
		want[s] = struct{}{}
	}
	for _, t := range targets {
		want[t.Symbol] = struct{}{}
	}
	out := make([]xsmom.CurrentPosition, 0)
	marks := map[string]float64{}
	for _, p := range all {
		if _, ok := want[p.Symbol]; !ok {
			continue
		}
		marks[p.Symbol] = p.MarkPrice
		if p.PositionAmt == 0 {
			continue
		}
		side := strategy.Long
		if p.PositionAmt < 0 {
			side = strategy.Short
		}
		notional := math.Abs(p.PositionAmt) * p.MarkPrice
		out = append(out, xsmom.CurrentPosition{
			Symbol:   p.Symbol,
			Side:     side,
			Notional: notional,
		})
	}
	return out, marks, nil
}

// filterTinyResizes drops same-side resize deltas whose absolute change
// is below threshold. ComputeOrderDeltas already applies a 5% gate, but
// the live runner's threshold is configurable.
func filterTinyResizes(deltas []xsmom.OrderDelta, current []xsmom.CurrentPosition, threshold float64) []xsmom.OrderDelta {
	if threshold <= 0 {
		return deltas
	}
	curBy := map[string]xsmom.CurrentPosition{}
	for _, c := range current {
		curBy[c.Symbol] = c
	}
	out := make([]xsmom.OrderDelta, 0, len(deltas))
	for _, d := range deltas {
		if d.Side == strategy.Flat {
			out = append(out, d)
			continue
		}
		c, has := curBy[d.Symbol]
		if !has || c.Side != d.Side {
			out = append(out, d)
			continue
		}
		diff := math.Abs(d.TargetNotional - c.Notional)
		if d.TargetNotional > 0 && diff/d.TargetNotional <= threshold {
			continue
		}
		out = append(out, d)
	}
	return out
}

// riskGate enforces sizing caps. Returns (reason, false) on violation.
func riskGate(targets []xsmom.Target, deltas []xsmom.OrderDelta, current []xsmom.CurrentPosition, equity float64, cfg xsmom.Config, f *xsmomLiveFlags) (string, bool) {
	if equity <= 0 {
		return "equity <= 0", false
	}
	// Single-symbol cap
	for _, t := range targets {
		if t.Notional > f.maxSymbolFracEq*equity {
			return fmt.Sprintf("symbol %s notional %.2f > cap %.2f", t.Symbol, t.Notional, f.maxSymbolFracEq*equity), false
		}
	}
	// Gross cap
	gross := 0.0
	for _, t := range targets {
		gross += t.Notional
	}
	cap := f.maxGrossFracEq * equity * cfg.Leverage
	if cfg.Leverage <= 0 {
		cap = f.maxGrossFracEq * equity
	}
	if gross > cap {
		return fmt.Sprintf("gross notional %.2f > cap %.2f", gross, cap), false
	}
	// Turnover cap (count flips/opens/closes against basket size)
	basket := cfg.TopN + cfg.BotN
	if basket > 0 {
		churn := 0
		for _, d := range deltas {
			if d.Side == strategy.Flat {
				churn++
			} else {
				// new entry or flip
				curHas := false
				for _, c := range current {
					if c.Symbol == d.Symbol && c.Side == d.Side {
						curHas = true
						break
					}
				}
				if !curHas {
					churn++
				}
			}
		}
		if frac := float64(churn) / float64(2*basket); frac > f.maxBasketTurnover {
			return fmt.Sprintf("turnover %.0f%% > cap %.0f%%", frac*100, f.maxBasketTurnover*100), false
		}
	}
	return "", true
}

// splitDeltas separates the delta list into close-first and open-second
// slices. A "flip" (current side != target side and current is non-empty)
// becomes a close in pass 1 and an open in pass 2.
func splitDeltas(deltas []xsmom.OrderDelta, current []xsmom.CurrentPosition) (closes, opens []xsmom.OrderDelta) {
	curBy := map[string]xsmom.CurrentPosition{}
	for _, c := range current {
		curBy[c.Symbol] = c
	}
	for _, d := range deltas {
		if d.Side == strategy.Flat {
			closes = append(closes, d)
			continue
		}
		c, has := curBy[d.Symbol]
		if has && c.Side != strategy.Flat && c.Side != d.Side {
			// flip: emit close + open
			closes = append(closes, xsmom.OrderDelta{
				Symbol: d.Symbol, Side: strategy.Flat, Reason: "flip-close",
			})
			opens = append(opens, xsmom.OrderDelta{
				Symbol: d.Symbol, Side: d.Side, TargetNotional: d.TargetNotional, Reason: "flip-open",
			})
			continue
		}
		opens = append(opens, d)
	}
	sort.Slice(closes, func(i, j int) bool { return closes[i].Symbol < closes[j].Symbol })
	sort.Slice(opens, func(i, j int) bool { return opens[i].Symbol < opens[j].Symbol })
	return
}

// executedOrder records what actually happened on the exchange for a delta.
type executedOrder struct {
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Qty        float64 `json:"qty"`
	ReduceOnly bool    `json:"reduceOnly"`
	OrderID    int64   `json:"orderId"`
	Status     string  `json:"status"`
	AvgPrice   string  `json:"avgPrice"`
	Error      string  `json:"error,omitempty"`
	Reason     string  `json:"reason"`
}

// executeOrders converts each delta into a Binance market order. Closes
// use reduceOnly=true. Opens compute qty = TargetNotional / mark, then
// round to step size; orders failing min-notional / min-qty are skipped
// with a warning. Concurrency: single-threaded to keep ordering simple
// and avoid hammering the exchange.
func executeOrders(ctx context.Context, client *binance.FuturesClient, deltas []xsmom.OrderDelta, marks map[string]float64, isClose bool) []executedOrder {
	out := make([]executedOrder, 0, len(deltas))
	for _, d := range deltas {
		mark := marks[d.Symbol]
		if mark <= 0 {
			// Fall back to a fresh book ticker if mark missing.
			bt, err := client.FuturesBookTicker(ctx, d.Symbol)
			if err == nil {
				mark = (bt.BidPrice + bt.AskPrice) / 2
			}
		}
		rules, err := client.SymbolRules(ctx, d.Symbol)
		if err != nil {
			out = append(out, executedOrder{Symbol: d.Symbol, Reason: d.Reason, Error: "SymbolRules: " + err.Error()})
			continue
		}

		var side string
		var qty float64
		if isClose || d.Side == strategy.Flat {
			pos, perr := client.Positions(ctx, d.Symbol)
			if perr != nil {
				out = append(out, executedOrder{Symbol: d.Symbol, Reason: d.Reason, Error: "Positions: " + perr.Error()})
				continue
			}
			amt := 0.0
			for _, p := range pos {
				if p.Symbol == d.Symbol {
					amt = p.PositionAmt
					break
				}
			}
			if amt == 0 {
				continue
			}
			if amt > 0 {
				side = "SELL"
				qty = amt
			} else {
				side = "BUY"
				qty = -amt
			}
		} else {
			if mark <= 0 {
				out = append(out, executedOrder{Symbol: d.Symbol, Reason: d.Reason, Error: "no mark price"})
				continue
			}
			qty = d.TargetNotional / mark
			if d.Side == strategy.Long {
				side = "BUY"
			} else {
				side = "SELL"
			}
		}

		qty = rules.RoundQuantity(qty)
		if qty < rules.MinQty || qty <= 0 {
			out = append(out, executedOrder{Symbol: d.Symbol, Side: side, Qty: qty, Reason: d.Reason, Error: fmt.Sprintf("qty %g < minQty %g", qty, rules.MinQty)})
			continue
		}
		if !rules.ValidNotional(qty, mark) && !isClose {
			out = append(out, executedOrder{Symbol: d.Symbol, Side: side, Qty: qty, Reason: d.Reason, Error: fmt.Sprintf("notional %.4f < minNotional %.4f", qty*mark, rules.MinNotional)})
			continue
		}

		reduceOnly := isClose || d.Side == strategy.Flat
		resp, err := client.PlaceMarketOrder(ctx, d.Symbol, side, qty, reduceOnly)
		ex := executedOrder{
			Symbol:     d.Symbol,
			Side:       side,
			Qty:        qty,
			ReduceOnly: reduceOnly,
			Reason:     d.Reason,
		}
		if err != nil {
			ex.Error = err.Error()
		} else {
			ex.OrderID = resp.OrderID
			ex.Status = resp.Status
			ex.AvgPrice = resp.AveragePrice
		}
		out = append(out, ex)
	}
	return out
}

// setLeverages sets exchange-side leverage on each open-side symbol.
// Best effort: errors are logged but do not abort.
func setLeverages(ctx context.Context, client *binance.FuturesClient, opens []xsmom.OrderDelta, lev int) {
	seen := map[string]struct{}{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, d := range opens {
		if d.Side == strategy.Flat {
			continue
		}
		if _, ok := seen[d.Symbol]; ok {
			continue
		}
		seen[d.Symbol] = struct{}{}
		wg.Add(1)
		sem <- struct{}{}
		go func(sym string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := client.ChangeLeverage(ctx, sym, lev); err != nil {
				fmt.Fprintf(os.Stderr, "  WARN: ChangeLeverage %s -> %d: %v\n", sym, lev, err)
			}
		}(d.Symbol)
	}
	wg.Wait()
}

// rebalanceRecord is one line in the JSON-lines audit log.
type rebalanceRecord struct {
	Time     time.Time            `json:"time"`
	DryRun   bool                 `json:"dryRun"`
	Equity   float64              `json:"equity,omitempty"`
	Targets  []xsmom.Target       `json:"targets,omitempty"`
	Deltas   []xsmom.OrderDelta   `json:"deltas,omitempty"`
	Executed []executedOrder      `json:"executed,omitempty"`
	Note     string               `json:"note,omitempty"`
}

func writeRebalanceLog(path string, rec rebalanceRecord) {
	fp, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  log open: %v\n", err)
		return
	}
	defer fp.Close()
	enc := json.NewEncoder(fp)
	if err := enc.Encode(rec); err != nil {
		fmt.Fprintf(os.Stderr, "  log encode: %v\n", err)
	}
}

func ensureLogDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0755)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func weekdayName(d int) string {
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	if d < 0 || d > 6 {
		return fmt.Sprintf("?(%d)", d)
	}
	return names[d]
}
