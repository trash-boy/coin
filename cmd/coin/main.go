package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"coin/internal/arbitrage"
	"coin/internal/backtest"
	"coin/internal/binance"
	"coin/internal/portfolio"
	"coin/internal/strategy"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "backtest":
		runBacktest(os.Args[2:])
	case "signal":
		runSignal(os.Args[2:])
	case "portfolio-backtest":
		runPortfolioBacktest(os.Args[2:])
	case "portfolio-signal":
		runPortfolioSignal(os.Args[2:])
	case "funding-arb":
		runFundingArb(os.Args[2:])
	case "basis-arb":
		runBasisArb(os.Args[2:])
	case "live":
		runLive(os.Args[2:])
	case "live-scan":
		runLiveScan(os.Args[2:])
	case "xsmom-backtest":
		runXSMomBacktest(os.Args[2:])
	case "xsmom-signal":
		runXSMomSignal(os.Args[2:])
	case "xsmom-live":
		runXSMomLive(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func runBacktest(args []string) {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	symbol := fs.String("symbol", "BTCUSDT", "USD-M futures symbol")
	interval := fs.String("interval", "1h", "kline interval")
	days := fs.Int("days", 2500, "lookback days")
	equity := fs.Float64("equity", 10000, "initial USDT equity")
	env := fs.String("env", "prod", "prod or testnet")
	useFunding := fs.Bool("funding", true, "include funding-rate history when available")
	configPath := fs.String("config", "", "flat YAML or JSON strategy config path")
	cfg := configFlags(fs)
	_ = fs.Parse(args)
	if *configPath != "" {
		if err := loadConfigFile(*configPath, cfg); err != nil {
			log.Fatal(err)
		}
		_ = fs.Parse(args)
	}

	client := binance.NewFuturesClient(*env)
	loadLeverageBrackets(context.Background(), client, *symbol, cfg)
	end := time.Now().UTC()
	start := end.Add(-time.Duration(*days) * 24 * time.Hour)
	candles, err := client.KlinesRange(context.Background(), *symbol, *interval, start, end)
	if err != nil {
		log.Fatal(err)
	}
	if len(candles) == 0 {
		log.Fatal("no candles returned")
	}
	var funding []strategy.FundingRate
	if *useFunding {
		funding, err = client.FundingRates(context.Background(), *symbol, start, end)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: funding history unavailable, continuing without funding: %v\n", err)
		}
	}
	result, err := backtest.RunWithFunding(candles, *equity, *cfg, funding)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Symbol: %s %s\n", strings.ToUpper(*symbol), *interval)
	fmt.Printf("Candles: %d (%s to %s UTC)\n", len(candles), candles[0].OpenTime.Format(time.RFC3339), candles[len(candles)-1].CloseTime.Format(time.RFC3339))
	fmt.Printf("Initial equity: %.2f USDT\n", result.InitialEquity)
	fmt.Printf("Final equity: %.2f USDT\n", result.FinalEquity)
	fmt.Printf("Return: %.2f%%\n", result.ReturnPct)
	fmt.Printf("Max drawdown: %.2f%%\n", result.MaxDrawdown)
	fmt.Printf("Trades: %d\n", len(result.Trades))
	fmt.Printf("Win rate: %.2f%%\n", result.WinRate)
	fmt.Printf("Profit factor: %.2f\n", result.ProfitFactor)
	fmt.Printf("Funding paid: %.2f USDT\n", result.FundingPaid)
	if len(result.Segments) > 0 {
		fmt.Println("\nSegments by exit year:")
		for _, segment := range result.Segments {
			fmt.Printf("  %s: trades=%d pnl=%.2f win=%.2f%% pf=%.2f\n", segment.Name, segment.Trades, segment.PnL, segment.WinRate, segment.ProfitFactor)
		}
	}
	printSignal("Latest signal", result.LastSignal)
}

func runSignal(args []string) {
	fs := flag.NewFlagSet("signal", flag.ExitOnError)
	symbol := fs.String("symbol", "BTCUSDT", "USD-M futures symbol")
	interval := fs.String("interval", "1h", "kline interval")
	lookback := fs.Int("lookback", 5000, "recent candles to fetch")
	equity := fs.Float64("equity", 10000, "current USDT equity for sizing")
	env := fs.String("env", "prod", "prod or testnet")
	configPath := fs.String("config", "", "flat YAML or JSON strategy config path")
	cfg := configFlags(fs)
	_ = fs.Parse(args)
	if *configPath != "" {
		if err := loadConfigFile(*configPath, cfg); err != nil {
			log.Fatal(err)
		}
		_ = fs.Parse(args)
	}

	client := binance.NewFuturesClient(*env)
	loadLeverageBrackets(context.Background(), client, *symbol, cfg)
	candles, err := client.Klines(context.Background(), *symbol, *interval, time.Time{}, time.Time{}, *lookback)
	if err != nil {
		log.Fatal(err)
	}
	fundingRate := 0.0
	premium, err := client.PremiumIndex(context.Background(), *symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: premium index unavailable, continuing with funding=0: %v\n", err)
	} else {
		fundingRate = premium.LastFundingRate
	}
	sig, err := strategy.LatestSignal(candles, strategy.Context{Equity: *equity, FundingRate: fundingRate}, *cfg)
	if err != nil {
		log.Fatal(err)
	}
	if sig.Action == strategy.ActionEnter {
		sig = roundSignal(context.Background(), client, *symbol, sig)
	}
	printSignal("Signal", sig)
}

func runPortfolioBacktest(args []string) {
	fs := flag.NewFlagSet("portfolio-backtest", flag.ExitOnError)
	symbolsFlag := fs.String("symbols", "BTCUSDT,ETHUSDT,SOLUSDT", "comma-separated USD-M futures symbols")
	interval := fs.String("interval", "1h", "kline interval")
	days := fs.Int("days", 2500, "lookback days")
	equity := fs.Float64("equity", 10000, "initial USDT equity")
	env := fs.String("env", "prod", "prod or testnet")
	useFunding := fs.Bool("funding", true, "include funding-rate history when available")
	configPath := fs.String("config", "", "flat YAML or JSON strategy config path")
	cfg := configFlags(fs)
	_ = fs.Parse(args)
	if *configPath != "" {
		if err := loadConfigFile(*configPath, cfg); err != nil {
			log.Fatal(err)
		}
		_ = fs.Parse(args)
	}

	symbols := parseSymbols(*symbolsFlag)
	if err := arbitrage.ValidateSymbols(symbols); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	client := binance.NewFuturesClient(*env)
	end := time.Now().UTC()
	start := end.Add(-time.Duration(*days) * 24 * time.Hour)
	data := make(map[string]portfolio.SymbolData, len(symbols))
	for _, symbol := range symbols {
		symbolCfg := *cfg
		loadLeverageBrackets(ctx, client, symbol, &symbolCfg)
		candles, err := client.KlinesRange(ctx, symbol, *interval, start, end)
		if err != nil {
			log.Fatalf("%s klines: %v", symbol, err)
		}
		if len(candles) == 0 {
			log.Fatalf("%s: no candles returned", symbol)
		}
		var funding []strategy.FundingRate
		if *useFunding {
			funding, err = client.FundingRates(ctx, symbol, start, end)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s funding history unavailable, continuing without funding: %v\n", symbol, err)
			}
		}
		data[symbol] = portfolio.SymbolData{Candles: candles, Funding: funding, Config: symbolCfg}
	}
	result, err := portfolio.RunEqualWeight(data, *equity, *cfg)
	if err != nil {
		log.Fatal(err)
	}
	printPortfolioBacktest(result, symbols, *interval)
}

func runPortfolioSignal(args []string) {
	fs := flag.NewFlagSet("portfolio-signal", flag.ExitOnError)
	symbolsFlag := fs.String("symbols", "BTCUSDT,ETHUSDT,SOLUSDT", "comma-separated USD-M futures symbols")
	interval := fs.String("interval", "1h", "kline interval")
	lookback := fs.Int("lookback", 5000, "recent candles to fetch")
	equity := fs.Float64("equity", 10000, "current USDT equity for sizing")
	env := fs.String("env", "prod", "prod or testnet")
	configPath := fs.String("config", "", "flat YAML or JSON strategy config path")
	cfg := configFlags(fs)
	_ = fs.Parse(args)
	if *configPath != "" {
		if err := loadConfigFile(*configPath, cfg); err != nil {
			log.Fatal(err)
		}
		_ = fs.Parse(args)
	}

	symbols := parseSymbols(*symbolsFlag)
	if err := arbitrage.ValidateSymbols(symbols); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	client := binance.NewFuturesClient(*env)
	allocation := *equity / float64(len(symbols))
	for _, symbol := range symbols {
		symbolCfg := *cfg
		loadLeverageBrackets(ctx, client, symbol, &symbolCfg)
		candles, err := client.Klines(ctx, symbol, *interval, time.Time{}, time.Time{}, *lookback)
		if err != nil {
			log.Fatalf("%s klines: %v", symbol, err)
		}
		fundingRate := 0.0
		premium, err := client.PremiumIndex(ctx, symbol)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s premium index unavailable, continuing with funding=0: %v\n", symbol, err)
		} else {
			fundingRate = premium.LastFundingRate
		}
		sig, err := strategy.LatestSignal(candles, strategy.Context{Equity: allocation, FundingRate: fundingRate}, symbolCfg)
		if err != nil {
			log.Fatalf("%s signal: %v", symbol, err)
		}
		if sig.Action == strategy.ActionEnter {
			sig = roundSignal(ctx, client, symbol, sig)
		}
		printSignal("Signal "+symbol, sig)
	}
}

func runFundingArb(args []string) {
	fs := flag.NewFlagSet("funding-arb", flag.ExitOnError)
	symbolsFlag := fs.String("symbols", "BTCUSDT,ETHUSDT,SOLUSDT,BNBUSDT,XRPUSDT,DOGEUSDT,ADAUSDT", "comma-separated USD-M futures symbols")
	env := fs.String("env", "prod", "prod or testnet")
	minFundingBps := fs.Float64("min-funding-bps", 1.0, "minimum absolute funding rate in bps per settlement")
	_ = fs.Parse(args)

	symbols := parseSymbols(*symbolsFlag)
	if err := arbitrage.ValidateSymbols(symbols); err != nil {
		log.Fatal(err)
	}
	client := binance.NewFuturesClient(*env)
	minRate := *minFundingBps / 10000
	opps := make([]arbitrage.FundingOpportunity, 0, len(symbols))
	for _, symbol := range symbols {
		premium, err := client.PremiumIndex(context.Background(), symbol)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s premium index unavailable: %v\n", symbol, err)
			continue
		}
		opp, ok := arbitrage.BuildFundingOpportunity(symbol, premium.MarkPrice, premium.IndexPrice, premium.LastFundingRate, minRate)
		if ok {
			opps = append(opps, opp)
		}
	}
	arbitrage.SortFunding(opps)
	printFundingOpportunities(opps, *minFundingBps)
}

func runBasisArb(args []string) {
	fs := flag.NewFlagSet("basis-arb", flag.ExitOnError)
	symbolsFlag := fs.String("symbols", "BTCUSDT,ETHUSDT,SOLUSDT,BNBUSDT,XRPUSDT,DOGEUSDT,ADAUSDT", "comma-separated symbols available on spot and USD-M futures")
	env := fs.String("env", "prod", "prod or testnet")
	costBps := fs.Float64("cost-bps", 8.0, "estimated round-trip taker fee and slippage cost in bps")
	minEdgeBps := fs.Float64("min-edge-bps", 3.0, "minimum net edge after cost in bps")
	_ = fs.Parse(args)

	symbols := parseSymbols(*symbolsFlag)
	if err := arbitrage.ValidateSymbols(symbols); err != nil {
		log.Fatal(err)
	}
	client := binance.NewFuturesClient(*env)
	costRate := *costBps / 10000
	minNetEdge := *minEdgeBps / 10000
	opps := make([]arbitrage.BasisOpportunity, 0, len(symbols))
	for _, symbol := range symbols {
		spot, err := client.SpotBookTicker(context.Background(), symbol)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s spot bookTicker unavailable: %v\n", symbol, err)
			continue
		}
		futures, err := client.FuturesBookTicker(context.Background(), symbol)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s futures bookTicker unavailable: %v\n", symbol, err)
			continue
		}
		opp, ok := arbitrage.BuildBasisOpportunity(symbol,
			spot.BidPrice, spot.BidQty, spot.AskPrice, spot.AskQty,
			futures.BidPrice, futures.BidQty, futures.AskPrice, futures.AskQty,
			costRate, minNetEdge,
		)
		if ok {
			opps = append(opps, opp)
		}
	}
	arbitrage.SortBasis(opps)
	printBasisOpportunities(opps, *costBps, *minEdgeBps)
}

func configFlags(fs *flag.FlagSet) *strategy.Config {
	cfg := strategy.DefaultConfig()
	fs.Float64Var(&cfg.MaxLeverage, "leverage", cfg.MaxLeverage, "strategy leverage cap")
	fs.Float64Var(&cfg.MaxMarginUse, "max-margin-use", cfg.MaxMarginUse, "maximum fraction of leveraged notional to use")
	fs.Float64Var(&cfg.RiskPerTrade, "risk", cfg.RiskPerTrade, "fraction of equity risked per trade")
	fs.Float64Var(&cfg.MaxDrawdown, "max-drawdown", cfg.MaxDrawdown, "account drawdown stop")
	fs.Float64Var(&cfg.DailyLossLimit, "daily-loss", cfg.DailyLossLimit, "daily loss stop")
	fs.StringVar((*string)(&cfg.MarginMode), "margin-mode", string(cfg.MarginMode), "margin mode: isolated or cross")
	fs.Float64Var(&cfg.FeeRate, "fee", cfg.FeeRate, "taker fee rate per side")
	fs.Float64Var(&cfg.SlippageBps, "slippage-bps", cfg.SlippageBps, "slippage in basis points")
	fs.IntVar(&cfg.FastEMA, "fast", cfg.FastEMA, "fast EMA period")
	fs.IntVar(&cfg.SlowEMA, "slow", cfg.SlowEMA, "slow EMA period")
	fs.IntVar(&cfg.TrendEMA, "trend", cfg.TrendEMA, "trend EMA period")
	fs.IntVar(&cfg.ATRPeriod, "atr", cfg.ATRPeriod, "ATR period")
	fs.Float64Var(&cfg.MinATRRatio, "min-atr-ratio", cfg.MinATRRatio, "minimum ATR/close ratio (volatility floor)")
	fs.Float64Var(&cfg.MaxATRRatio, "max-atr-ratio", cfg.MaxATRRatio, "maximum ATR/close ratio (volatility ceiling)")
	fs.Float64Var(&cfg.TrailATR, "trail-atr", cfg.TrailATR, "trailing stop ATR multiple")
	fs.IntVar(&cfg.HigherTFHours, "higher-tf-hours", cfg.HigherTFHours, "higher timeframe filter in hours")
	fs.IntVar(&cfg.MajorTFHours, "major-tf-hours", cfg.MajorTFHours, "major timeframe filter in hours")
	fs.IntVar(&cfg.HigherFastEMA, "higher-fast", cfg.HigherFastEMA, "higher timeframe fast EMA period")
	fs.IntVar(&cfg.HigherSlowEMA, "higher-slow", cfg.HigherSlowEMA, "higher timeframe slow EMA period")
	fs.Float64Var(&cfg.MinADXLong, "min-adx-long", cfg.MinADXLong, "minimum ADX for long entries")
	fs.Float64Var(&cfg.MinADXShort, "min-adx-short", cfg.MinADXShort, "minimum ADX for short entries")
	fs.Float64Var(&cfg.MinEfficiency, "min-efficiency", cfg.MinEfficiency, "minimum efficiency ratio for trend regime")
	fs.IntVar(&cfg.PullbackBars, "pullback-bars", cfg.PullbackBars, "pullback lookback bars")
	fs.IntVar(&cfg.SwingLookback, "swing-lookback", cfg.SwingLookback, "swing high/low stop lookback")
	fs.Float64Var(&cfg.MinStopATR, "min-stop-atr", cfg.MinStopATR, "minimum structure stop ATR multiple")
	fs.Float64Var(&cfg.MaxStopATR, "max-stop-atr", cfg.MaxStopATR, "skip entries whose structure stop exceeds this ATR multiple")
	fs.Float64Var(&cfg.BreakEvenR, "break-even-r", cfg.BreakEvenR, "R multiple before moving trailing stop to break-even")
	fs.Float64Var(&cfg.TrailActivationR, "trail-activation-r", cfg.TrailActivationR, "R multiple before activating ATR trailing stop")
	fs.IntVar(&cfg.MaxAdds, "max-adds", cfg.MaxAdds, "maximum pyramid adds")
	fs.IntVar(&cfg.MinAddSpacingBars, "min-add-spacing", cfg.MinAddSpacingBars, "minimum bars between pyramid adds")
	fs.Float64Var(&cfg.PyramidATR, "pyramid-atr", cfg.PyramidATR, "favorable ATR move required before adding")
	fs.Float64Var(&cfg.ShortRiskMultiplier, "short-risk-mult", cfg.ShortRiskMultiplier, "short-side risk multiplier")
	fs.Float64Var(&cfg.VolTargetATRRatio, "vol-target-atr", cfg.VolTargetATRRatio, "target ATR/price ratio for volatility leverage")
	fs.Float64Var(&cfg.LiquidationStopBuffer, "liq-stop-buffer", cfg.LiquidationStopBuffer, "require stop distance below this fraction of estimated liquidation distance")
	fs.Float64Var(&cfg.FallbackMMR, "fallback-mmr", cfg.FallbackMMR, "fallback maintenance margin ratio when leverage brackets are unavailable")
	fs.Float64Var(&cfg.FundingBlockLong, "funding-block-long", cfg.FundingBlockLong, "block longs when funding is above this rate")
	fs.Float64Var(&cfg.FundingBlockShort, "funding-block-short", cfg.FundingBlockShort, "block shorts when funding is below negative this rate")
	return &cfg
}

func loadLeverageBrackets(ctx context.Context, client *binance.FuturesClient, symbol string, cfg *strategy.Config) {
	if client.APIKey == "" || client.Secret == "" {
		return
	}
	brackets, err := client.LeverageBrackets(ctx, symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s leverage brackets unavailable, using fallback MMR: %v\n", strings.ToUpper(symbol), err)
		return
	}
	cfg.MaintenanceBrackets = brackets
}

func loadConfigFile(path string, cfg *strategy.Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		return json.Unmarshal(data, cfg)
	}
	if err := applyMaintenanceBracketsYAML(data, cfg); err != nil {
		return err
	}
	return applyFlatYAML(data, cfg)
}

func applyFlatYAML(data []byte, cfg *strategy.Config) error {
	fields := configFieldMap(cfg)
	inBrackets := false
	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		keyOnly := strings.TrimSpace(strings.TrimSuffix(line, ":"))
		if normalizeKey(keyOnly) == "maintenancebrackets" {
			inBrackets = true
			if strings.Contains(line, "[") && strings.Contains(line, "]") {
				inBrackets = false
			}
			continue
		}
		if inBrackets {
			if strings.HasPrefix(line, "-") || strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t") {
				continue
			}
			inBrackets = false
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid config line %d: %s", lineNo+1, raw)
		}
		key := normalizeKey(parts[0])
		field, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown config key %q on line %d", strings.TrimSpace(parts[0]), lineNo+1)
		}
		if err := setConfigField(field, strings.TrimSpace(parts[1])); err != nil {
			return fmt.Errorf("config line %d: %w", lineNo+1, err)
		}
	}
	return nil
}

func applyMaintenanceBracketsYAML(data []byte, cfg *strategy.Config) error {
	text := string(data)
	if !strings.Contains(normalizeKey(text), "maintenancebrackets") {
		return nil
	}
	re := regexp.MustCompile(`(?s)maintenance[_-]?brackets\s*:\s*(\[.*\])`)
	match := re.FindStringSubmatch(text)
	if len(match) != 2 {
		return nil
	}
	var brackets []strategy.MaintenanceBracket
	if err := json.Unmarshal([]byte(match[1]), &brackets); err != nil {
		return fmt.Errorf("maintenance_brackets must be a JSON array in YAML config: %w", err)
	}
	cfg.MaintenanceBrackets = brackets
	return nil
}

func configFieldMap(cfg *strategy.Config) map[string]reflect.Value {
	out := make(map[string]reflect.Value)
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if !field.CanSet() || field.Kind() == reflect.Slice {
			continue
		}
		out[normalizeKey(t.Field(i).Name)] = field
	}
	return out
}

func setConfigField(field reflect.Value, raw string) error {
	raw = strings.Trim(raw, `"'`)
	switch field.Kind() {
	case reflect.Int:
		value, err := strconv.Atoi(raw)
		if err != nil {
			return err
		}
		field.SetInt(int64(value))
	case reflect.Float64:
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		field.SetFloat(value)
	case reflect.String:
		field.SetString(raw)
	default:
		return fmt.Errorf("unsupported config field kind %s", field.Kind())
	}
	return nil
}

func stripComment(line string) string {
	if idx := strings.Index(line, "#"); idx >= 0 {
		return line[:idx]
	}
	return line
}

func normalizeKey(key string) string {
	key = strings.TrimSpace(key)
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, "_", "")
	return strings.ToLower(key)
}

func printSignal(title string, sig strategy.Signal) {
	fmt.Printf("\n%s:\n", title)
	fmt.Printf("  time: %s\n", sig.Time.Format(time.RFC3339))
	fmt.Printf("  action: %s\n", sig.Action)
	fmt.Printf("  side: %s\n", sig.Side)
	fmt.Printf("  price: %.8f\n", sig.Price)
	if sig.Action == strategy.ActionEnter && sig.Quantity > 0 {
		fmt.Printf("  leverage: %.2fx\n", sig.Leverage)
		fmt.Printf("  quantity: %s\n", trimFloat(sig.Quantity))
		fmt.Printf("  notional: %.2f USDT\n", sig.Notional)
		fmt.Printf("  stop_loss: %.8f\n", sig.StopLoss)
		if sig.TakeProfit > 0 {
			fmt.Printf("  take_profit: %.8f\n", sig.TakeProfit)
		}
	}
	fmt.Printf("  reason: %s\n", sig.Reason)
}

func roundSignal(ctx context.Context, client *binance.FuturesClient, symbol string, sig strategy.Signal) strategy.Signal {
	rules, err := client.SymbolRules(ctx, symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s symbol rules unavailable, leaving raw signal size: %v\n", strings.ToUpper(symbol), err)
		return sig
	}
	sig.Quantity = rules.RoundQuantity(sig.Quantity)
	sig.StopLoss = rules.RoundPrice(sig.StopLoss)
	sig.TakeProfit = rules.RoundPrice(sig.TakeProfit)
	sig.Notional = sig.Quantity * sig.Price
	if !rules.ValidNotional(sig.Quantity, sig.Price) {
		sig.Action = strategy.ActionNone
		sig.Side = strategy.Flat
		sig.Reason = fmt.Sprintf("rounded notional %.4f is below exchange minimum %.4f", sig.Notional, rules.MinNotional)
	}
	return sig
}

func parseSymbols(raw string) []string {
	seen := make(map[string]bool)
	symbols := make([]string, 0)
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	for _, field := range fields {
		symbol := strings.ToUpper(strings.TrimSpace(field))
		if symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	return symbols
}

func printPortfolioBacktest(result portfolio.Result, symbols []string, interval string) {
	fmt.Printf("Portfolio: %s %s equal-weight\n", strings.Join(symbols, ","), interval)
	fmt.Printf("Initial equity: %.2f USDT\n", result.InitialEquity)
	fmt.Printf("Final equity: %.2f USDT\n", result.FinalEquity)
	fmt.Printf("Return: %.2f%%\n", result.ReturnPct)
	fmt.Printf("Trades: %d\n", result.TotalTrades)
	fmt.Println("\nPer symbol:")
	for _, item := range result.Results {
		r := item.Result
		fmt.Printf("  %s allocation=%.2f final=%.2f return=%.2f%% maxDD=%.2f%% trades=%d win=%.2f%% pf=%.2f funding=%.2f\n",
			item.Symbol, item.Allocation, r.FinalEquity, r.ReturnPct, r.MaxDrawdown, len(r.Trades), r.WinRate, r.ProfitFactor, r.FundingPaid)
	}
}

func printFundingOpportunities(opps []arbitrage.FundingOpportunity, minFundingBps float64) {
	fmt.Printf("Funding arbitrage scan: min_abs_funding=%.2f bps/settlement\n", minFundingBps)
	if len(opps) == 0 {
		fmt.Println("No funding opportunities above threshold.")
		return
	}
	fmt.Println("Symbol      Direction                 Funding/8h  CarryAnn    Basis      Mark        Index")
	for _, opp := range opps {
		fmt.Printf("%-11s %-25s %9.3f%% %10.2f%% %8.3f%% %10.4f %10.4f\n",
			opp.Symbol,
			opp.Direction,
			opp.FundingRate*100,
			math.Abs(opp.AnnualizedRate)*100,
			opp.BasisRate*100,
			opp.MarkPrice,
			opp.IndexPrice,
		)
	}
}

func printBasisOpportunities(opps []arbitrage.BasisOpportunity, costBps, minEdgeBps float64) {
	fmt.Printf("Basis arbitrage scan: cost=%.2f bps min_net_edge=%.2f bps\n", costBps, minEdgeBps)
	if len(opps) == 0 {
		fmt.Println("No basis opportunities above threshold.")
		return
	}
	fmt.Println("Symbol      Direction                 GrossEdge  NetEdge  SpotEntry   FuturesEntry  MaxBaseQty")
	for _, opp := range opps {
		fmt.Printf("%-11s %-25s %8.2f %8.2f %11.4f %13.4f %11s\n",
			opp.Symbol,
			opp.Direction,
			opp.GrossEdge*10000,
			opp.NetEdge*10000,
			opp.SpotEntry,
			opp.FuturesEntry,
			trimFloat(opp.MaxBaseQty),
		)
	}
}

func trimFloat(v float64) string {
	if math.Abs(v) >= 1 {
		return fmt.Sprintf("%.4f", v)
	}
	return fmt.Sprintf("%.8f", v)
}

func usage() {
	fmt.Println("Usage:")
	fmt.Println("  go run ./cmd/coin backtest [flags]")
	fmt.Println("  go run ./cmd/coin signal [flags]")
	fmt.Println("  go run ./cmd/coin portfolio-backtest [flags]")
	fmt.Println("  go run ./cmd/coin portfolio-signal [flags]")
	fmt.Println("  go run ./cmd/coin funding-arb [flags]")
	fmt.Println("  go run ./cmd/coin basis-arb [flags]")
	fmt.Println("  go run ./cmd/coin live [flags]   # paper / live trading loop (testnet by default, dry-run by default)")
	fmt.Println("  go run ./cmd/coin xsmom-backtest [flags]   # cross-sectional momentum backtest on top USDT-perps")
	fmt.Println("  go run ./cmd/coin xsmom-signal [flags]     # latest xsmom target portfolio + order deltas")
	fmt.Println("  go run ./cmd/coin xsmom-live [flags]       # always-on xsmom rebalancer (default dry-run)")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  go run ./cmd/coin backtest -symbol BTCUSDT -interval 1h -days 2500 -equity 10000 -leverage 2")
	fmt.Println("  go run ./cmd/coin signal -symbol ETHUSDT -interval 1h -equity 10000 -leverage 2")
	fmt.Println("  go run ./cmd/coin portfolio-backtest -symbols BTCUSDT,ETHUSDT,SOLUSDT -interval 1h -equity 10000")
	fmt.Println("  go run ./cmd/coin funding-arb -symbols BTCUSDT,ETHUSDT,SOLUSDT -min-funding-bps 1")
	fmt.Println("  go run ./cmd/coin basis-arb -symbols BTCUSDT,ETHUSDT,SOLUSDT -cost-bps 8 -min-edge-bps 3")
	fmt.Println("  go run ./cmd/coin live -symbol BTCUSDT -interval 1h -env testnet -dry-run=true")
	fmt.Println("  go run ./cmd/coin xsmom-backtest -top 100 -days 360 -equity 10000 -lookback 21 -hold 7 -topn 10 -botn 0 -leverage 1")
	fmt.Println("  go run ./cmd/coin xsmom-signal -top 100 -days 360 -equity 10000 -current ./positions.json")
	fmt.Println("  go run ./cmd/coin xsmom-live -top 100 -days 360 -use-account-equity -dry-run=true   # safe preview")
	fmt.Println("  go run ./cmd/coin xsmom-live -top 100 -days 360 -use-account-equity -dry-run=false  # LIVE TRADING")
}
