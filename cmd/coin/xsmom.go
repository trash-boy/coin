package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"coin/internal/binance"
	"coin/internal/strategy"
	"coin/internal/strategy/xsmom"
)

// xsmomFlagSet wires the shared flags used by both backtest and signal
// subcommands. Defaults match xsmom.DefaultConfig() (validated 360d
// champion: L10 / 21d / 7d, leverage 1x, funding on).
func xsmomFlagSet(name string) (*flag.FlagSet, *xsmomFlags) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	f := &xsmomFlags{}
	cfg := xsmom.DefaultConfig()
	fs.StringVar(&f.env, "env", "prod", "prod or testnet")
	fs.IntVar(&f.top, "top", 100, "top N symbols by 24h quote volume to use as universe (before filters)")
	fs.Float64Var(&f.minListVol, "min-listing-vol-usdt", 5_000_000, "min 24h quote-vol USD for a symbol to enter the universe candidate list")
	fs.IntVar(&f.days, "days", 360, "lookback days for panel construction")
	fs.StringVar(&f.interval, "interval", "30m", "source kline interval (resampled to daily)")
	fs.IntVar(&f.maxConcurrent, "max-concurrent", 6, "max concurrent klines fetches")
	fs.Float64Var(&f.equity, "equity", 10000, "equity in USDT")
	fs.IntVar(&f.lookback, "lookback", cfg.LookbackDays, "ranking lookback days")
	fs.IntVar(&f.hold, "hold", cfg.HoldDays, "rebalance period in days")
	fs.IntVar(&f.topN, "topn", cfg.TopN, "longs basket size")
	fs.IntVar(&f.botN, "botn", cfg.BotN, "shorts basket size (0 = long-only)")
	fs.Float64Var(&f.leverage, "leverage", cfg.Leverage, "gross leverage (avoid > 2 on this strategy)")
	fs.IntVar(&f.minHist, "min-hist-days", cfg.MinHistDays, "minimum listing-age days for eligibility")
	fs.Float64Var(&f.minQuoteVol, "min-quote-vol-usd", cfg.MinQuoteVolUSD, "rolling 30d quote-vol floor in USD")
	fs.Float64Var(&f.feeBps, "fee-bps", cfg.FeeBps, "per-leg taker fee in basis points")
	fs.Float64Var(&f.slipBps, "slip-bps", cfg.SlipBps, "per-leg slippage in basis points")
	fs.BoolVar(&f.includeFunding, "funding", cfg.IncludeFunding, "include funding cost in backtest / live signal")
	fs.BoolVar(&f.symbolsOverride, "symbols-override", false, "if set, treat -symbols as the universe directly")
	fs.StringVar(&f.symbols, "symbols", "", "comma-separated symbols when -symbols-override")
	return fs, f
}

type xsmomFlags struct {
	env             string
	top             int
	minListVol      float64
	days            int
	interval        string
	maxConcurrent   int
	equity          float64
	lookback        int
	hold            int
	topN            int
	botN            int
	leverage        float64
	minHist         int
	minQuoteVol     float64
	feeBps          float64
	slipBps         float64
	includeFunding  bool
	symbolsOverride bool
	symbols         string
}

func (f *xsmomFlags) toConfig() xsmom.Config {
	return xsmom.Config{
		LookbackDays:   f.lookback,
		HoldDays:       f.hold,
		TopN:           f.topN,
		BotN:           f.botN,
		Leverage:       f.leverage,
		MinHistDays:    f.minHist,
		MinQuoteVolUSD: f.minQuoteVol,
		FeeBps:         f.feeBps,
		SlipBps:        f.slipBps,
		IncludeFunding: f.includeFunding,
	}
}

func (f *xsmomFlags) resolveUniverse(ctx context.Context, client *binance.FuturesClient) ([]string, error) {
	if f.symbolsOverride {
		syms := parseSymbols(f.symbols)
		if len(syms) == 0 {
			return nil, fmt.Errorf("-symbols-override set but -symbols is empty")
		}
		return syms, nil
	}
	return xsmom.TopUSDTPerpsByVolume(ctx, client, f.top, f.minListVol)
}

func runXSMomBacktest(args []string) {
	fs, f := xsmomFlagSet("xsmom-backtest")
	jsonOut := fs.Bool("json", false, "emit Result as JSON to stdout (suppresses summary)")
	_ = fs.Parse(args)
	cfg := f.toConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	ctx := context.Background()
	client := binance.NewFuturesClient(f.env)
	universe, err := f.resolveUniverse(ctx, client)
	if err != nil {
		log.Fatalf("universe: %v", err)
	}
	if len(universe) == 0 {
		log.Fatal("universe is empty after filtering")
	}
	fmt.Fprintf(os.Stderr, "universe: %d symbols, fetching %dd of %s bars...\n", len(universe), f.days, f.interval)

	panel, failures, err := xsmom.BuildPanelFromBinance(ctx, client, xsmom.FetchOptions{
		Symbols:        universe,
		Days:           f.days,
		Interval:       f.interval,
		MaxConcurrent:  f.maxConcurrent,
		IncludeFunding: f.includeFunding,
	})
	if err != nil {
		log.Fatalf("panel: %v", err)
	}
	for _, fail := range failures {
		fmt.Fprintf(os.Stderr, "  skip %s: %v\n", fail.Symbol, fail.Err)
	}
	fmt.Fprintf(os.Stderr, "panel: %d symbols x %d days\n", len(panel.Symbols), len(panel.Days))

	res, err := xsmom.Run(panel, cfg, 0, 0, f.equity)
	if err != nil {
		log.Fatalf("backtest: %v", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}

	printXSMomResult(res, cfg, len(panel.Symbols))
}

func runXSMomSignal(args []string) {
	fs, f := xsmomFlagSet("xsmom-signal")
	currentPath := fs.String("current", "", "JSON file with current open positions: [{symbol,side,notional}]")
	jsonOut := fs.Bool("json", false, "emit signal+deltas as JSON")
	_ = fs.Parse(args)
	cfg := f.toConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	ctx := context.Background()
	client := binance.NewFuturesClient(f.env)
	universe, err := f.resolveUniverse(ctx, client)
	if err != nil {
		log.Fatalf("universe: %v", err)
	}
	if len(universe) == 0 {
		log.Fatal("universe is empty after filtering")
	}
	fmt.Fprintf(os.Stderr, "universe: %d symbols, fetching %dd of %s bars...\n", len(universe), f.days, f.interval)

	panel, failures, err := xsmom.BuildPanelFromBinance(ctx, client, xsmom.FetchOptions{
		Symbols:        universe,
		Days:           f.days,
		Interval:       f.interval,
		MaxConcurrent:  f.maxConcurrent,
		IncludeFunding: f.includeFunding,
	})
	if err != nil {
		log.Fatalf("panel: %v", err)
	}
	for _, fail := range failures {
		fmt.Fprintf(os.Stderr, "  skip %s: %v\n", fail.Symbol, fail.Err)
	}
	fmt.Fprintf(os.Stderr, "panel: %d symbols x %d days\n", len(panel.Symbols), len(panel.Days))

	signal, err := xsmom.LatestSignal(panel, cfg, f.equity)
	if err != nil {
		log.Fatalf("signal: %v", err)
	}

	current := loadCurrentPositions(*currentPath)
	deltas := xsmom.ComputeOrderDeltas(signal, current)

	if *jsonOut {
		out := map[string]interface{}{
			"signal": signal,
			"deltas": deltas,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	printXSMomSignal(signal, deltas, cfg, f.equity)
}

func loadCurrentPositions(path string) []xsmom.CurrentPosition {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read current positions: %v", err)
	}
	type rawPos struct {
		Symbol   string  `json:"symbol"`
		Side     string  `json:"side"`
		Notional float64 `json:"notional"`
	}
	var raws []rawPos
	if err := json.Unmarshal(data, &raws); err != nil {
		log.Fatalf("parse current positions: %v", err)
	}
	out := make([]xsmom.CurrentPosition, 0, len(raws))
	for _, r := range raws {
		side := strategy.Flat
		switch strings.ToUpper(r.Side) {
		case "LONG":
			side = strategy.Long
		case "SHORT":
			side = strategy.Short
		case "FLAT", "":
			side = strategy.Flat
		default:
			log.Fatalf("invalid side %q for %s", r.Side, r.Symbol)
		}
		out = append(out, xsmom.CurrentPosition{
			Symbol:   strings.ToUpper(r.Symbol),
			Side:     side,
			Notional: r.Notional,
		})
	}
	return out
}

func printXSMomResult(r xsmom.Result, cfg xsmom.Config, panelSyms int) {
	fmt.Printf("xsmom backtest config: L%d/S%d lookback=%dd hold=%dd lev=%.2fx funding=%v slip=%.1fbps fee=%.1fbps\n",
		cfg.TopN, cfg.BotN, cfg.LookbackDays, cfg.HoldDays, cfg.Leverage, cfg.IncludeFunding, cfg.SlipBps, cfg.FeeBps)
	fmt.Printf("panel symbols: %d   eligibility: minHist=%dd minQVol=%.0f USDT\n", panelSyms, cfg.MinHistDays, cfg.MinQuoteVolUSD)
	fmt.Printf("window: %s -> %s (%d days, idx %d..%d)\n",
		r.StartDate.Format("2006-01-02"), r.EndDate.Format("2006-01-02"), r.Days, r.StartIdx, r.EndIdx)
	fmt.Printf("equity: %.2f -> %.2f USDT\n", r.InitialEquity, r.FinalEquity)
	fmt.Printf("total return: %.2f%%   ann return: %.2f%%   sharpe: %.2f\n",
		r.TotalReturn*100, r.AnnReturn*100, r.Sharpe)
	fmt.Printf("max drawdown: %.2f%%   funding paid: %.4f%% of equity\n",
		r.MaxDrawdown*100, r.FundingPaidPct)
	fmt.Printf("rebalances: %d   avg universe: %.1f   avg basket turnover: %.1f%%\n",
		r.NumRebalances, r.AvgUniverse, r.SymbolTurnover*100)
	if len(r.Trades) > 0 {
		fmt.Printf("first 3 rebalances:\n")
		for i, t := range r.Trades {
			if i >= 3 {
				break
			}
			fmt.Printf("  %s  +L=%v  -L=%v  +S=%v  -S=%v  cost=%.4f%%\n",
				t.Time.Format("2006-01-02"), t.NewLongs, t.DroppedLongs, t.NewShorts, t.DroppedShorts, t.CostFraction*100)
		}
		last := r.Trades[len(r.Trades)-1]
		fmt.Printf("last rebalance:\n  %s  +L=%v  -L=%v  +S=%v  -S=%v  cost=%.4f%%\n",
			last.Time.Format("2006-01-02"), last.NewLongs, last.DroppedLongs, last.NewShorts, last.DroppedShorts, last.CostFraction*100)
	}
}

func printXSMomSignal(s xsmom.SignalSet, deltas []xsmom.OrderDelta, cfg xsmom.Config, equity float64) {
	fmt.Printf("xsmom signal generated at %s (panel day %s)\n",
		time.Now().UTC().Format(time.RFC3339), s.GeneratedAt.Format("2006-01-02"))
	fmt.Printf("config: L%d/S%d lookback=%dd hold=%dd lev=%.2fx equity=%.2f USDT\n",
		cfg.TopN, cfg.BotN, cfg.LookbackDays, cfg.HoldDays, cfg.Leverage, equity)
	if s.Skipped {
		fmt.Printf("SKIPPED: %s\n", s.SkipReason)
		return
	}
	fmt.Printf("eligible universe: %d   reason: %s\n", s.UniverseSize, s.Reason)

	longs := make([]xsmom.Target, 0, len(s.Targets))
	shorts := make([]xsmom.Target, 0, len(s.Targets))
	for _, t := range s.Targets {
		if t.Side == strategy.Long {
			longs = append(longs, t)
		} else if t.Side == strategy.Short {
			shorts = append(shorts, t)
		}
	}
	if len(longs) > 0 {
		fmt.Println("\nlong basket:")
		for _, t := range longs {
			fmt.Printf("  %-12s side=LONG  weight=%.2f%%  notional=%.2f USDT\n",
				t.Symbol, t.WeightFrac*100, t.Notional)
		}
	}
	if len(shorts) > 0 {
		fmt.Println("\nshort basket:")
		for _, t := range shorts {
			fmt.Printf("  %-12s side=SHORT weight=%.2f%%  notional=%.2f USDT\n",
				t.Symbol, t.WeightFrac*100, t.Notional)
		}
	}

	fmt.Println("\ntop long candidates by lookback return:")
	limit := 5
	if len(s.LongCandidates) < limit {
		limit = len(s.LongCandidates)
	}
	for i := 0; i < limit; i++ {
		r := s.LongCandidates[i]
		fmt.Printf("  %-12s ret=%.2f%%\n", r.Symbol, r.Ret*100)
	}
	if len(s.ShortCandidates) > 0 {
		fmt.Println("\ntop short candidates (worst lookback return):")
		// SelectLongShort returns shorts already sliced; sort ascending for clarity
		ss := append([]xsmom.RankedReturn{}, s.ShortCandidates...)
		sort.Slice(ss, func(a, b int) bool { return ss[a].Ret < ss[b].Ret })
		for _, r := range ss {
			fmt.Printf("  %-12s ret=%.2f%%\n", r.Symbol, r.Ret*100)
		}
	}

	if len(deltas) > 0 {
		fmt.Println("\norder deltas:")
		for _, d := range deltas {
			fmt.Printf("  %-12s side=%-5s targetNotional=%10.2f  reason=%s\n",
				d.Symbol, d.Side, d.TargetNotional, d.Reason)
		}
	} else {
		fmt.Println("\nno order deltas (current matches target within 5% threshold)")
	}
}
