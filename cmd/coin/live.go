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
	"strings"
	"syscall"
	"time"

	"coin/internal/binance"
	"coin/internal/strategy"
)

type LiveState struct {
	Env             string    `json:"env"`
	Symbol          string    `json:"symbol"`
	HasPosition     bool      `json:"has_position"`
	Side            string    `json:"side"`
	Quantity        float64   `json:"quantity"`
	EntryPrice      float64   `json:"entry_price"`
	StopLoss        float64   `json:"stop_loss"`
	OpenedAt        time.Time `json:"opened_at"`
	LastSignalTime  time.Time `json:"last_signal_time"`
	DailyDate       string    `json:"daily_date"`
	DailyStartEquit float64   `json:"daily_start_equity"`
	Halted          bool      `json:"halted"`
	HaltReason      string    `json:"halt_reason"`
}

func runLive(args []string) {
	fs := flag.NewFlagSet("live", flag.ExitOnError)
	symbol := fs.String("symbol", "BTCUSDT", "USD-M futures symbol")
	interval := fs.String("interval", "1h", "kline interval")
	envName := fs.String("env", "testnet", "prod or testnet")
	equityFlag := fs.Float64("equity", 0, "override sizing equity (USDT). 0 = fetch from balance")
	lookback := fs.Int("lookback", 5000, "klines fetched per cycle")
	pollEvery := fs.Duration("poll", 30*time.Second, "polling interval after candle close")
	dryRun := fs.Bool("dry-run", true, "if true, NEVER sends real orders")
	confirm := fs.Bool("i-understand-risk", false, "must be true to enable real orders")
	maxDailyLossPct := fs.Float64("max-daily-loss-pct", 5.0, "halt for the rest of UTC day if equity DD this %% from day start")
	stateDir := fs.String("state-dir", ".", "directory for live state json")
	configPath := fs.String("config", "", "flat YAML or JSON strategy config path")
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
		fmt.Println("  Symbol  :", strings.ToUpper(*symbol))
		fmt.Println("  Strategy: leverage_trend (low PF in backtest, see README)")
		fmt.Println("  You assume full responsibility for fund loss.")
		fmt.Println("==========================================================")
	}
	if !live {
		fmt.Println("[mode] DRY-RUN")
	} else {
		fmt.Println("[mode] LIVE")
	}
	fmt.Printf("[env ] %s    [symbol] %s    [interval] %s    [poll] %v\n",
		*envName, strings.ToUpper(*symbol), *interval, *pollEvery)

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
	loadLeverageBrackets(ctx, client, *symbol, cfg)

	statePath := filepath.Join(*stateDir, fmt.Sprintf(".live_state.%s.%s.json",
		strings.ToLower(*envName), strings.ToUpper(*symbol)))
	state := loadLiveState(statePath, *envName, *symbol)
	reconcile(ctx, client, *symbol, &state)
	saveLiveState(statePath, &state)

	dur, err := candleDuration(*interval)
	if err != nil {
		log.Fatal(err)
	}
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
		if err := liveCycle(ctx, client, *symbol, *interval, *lookback, *equityFlag,
			*maxDailyLossPct, live, cfg, &state, statePath); err != nil {
			fmt.Fprintf(os.Stderr, "[cycle err] %v\n", err)
		}
		if !sleepWithCancel(ctx, *pollEvery) {
			return
		}
	}
}

func liveCycle(ctx context.Context, c *binance.FuturesClient, symbol, interval string, lookback int,
	equityOverride float64, maxDailyLossPct float64, live bool, cfg *strategy.Config,
	state *LiveState, statePath string) error {

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
	if state.DailyDate != today || state.DailyStartEquit == 0 {
		state.DailyDate = today
		state.DailyStartEquit = equity
		state.Halted = false
		state.HaltReason = ""
	}
	if state.DailyStartEquit > 0 {
		drawdown := (state.DailyStartEquit - equity) / state.DailyStartEquit * 100
		if drawdown >= maxDailyLossPct {
			state.Halted = true
			state.HaltReason = fmt.Sprintf("daily DD %.2f%% >= %.2f%%", drawdown, maxDailyLossPct)
		}
	}
	saveLiveState(statePath, state)

	fmt.Printf("\n[cycle %s] equity=%.4f USDT  available=%.4f  uPnL=%.4f  dailyStart=%.4f  halted=%v\n",
		time.Now().UTC().Format("2006-01-02 15:04:05Z"),
		equity, balance.AvailableBalance, balance.CrossUnPnL, state.DailyStartEquit, state.Halted)

	end := time.Now().UTC()
	start := end.Add(-time.Duration(lookback) * mustCandleDur(interval))
	candles, err := c.KlinesRange(ctx, symbol, interval, start, end)
	if err != nil {
		return fmt.Errorf("klines: %w", err)
	}
	if len(candles) == 0 {
		return fmt.Errorf("no candles")
	}
	fundingRate := 0.0
	if pi, perr := c.PremiumIndex(ctx, symbol); perr == nil {
		fundingRate = pi.LastFundingRate
	}
	sig, err := strategy.LatestSignal(candles, strategy.Context{Equity: equity, FundingRate: fundingRate}, *cfg)
	if err != nil {
		return fmt.Errorf("strategy: %w", err)
	}
	if sig.Action == strategy.ActionEnter {
		sig = roundSignal(ctx, c, symbol, sig)
	}
	printSignal("Signal", sig)
	state.LastSignalTime = sig.Time

	reconcile(ctx, c, symbol, state)

	if state.Halted {
		fmt.Println("[skip] halted by kill-switch:", state.HaltReason)
		saveLiveState(statePath, state)
		return nil
	}

	switch sig.Action {
	case strategy.ActionEnter:
		if state.HasPosition {
			fmt.Println("[skip] already in position, ignoring new ENTER signal")
			return nil
		}
		if !live {
			fmt.Printf("[dry-run] WOULD ENTER %s qty=%.6f price~=%.2f stop=%.2f notional=%.2f lev=%.0f\n",
				sig.Side, sig.Quantity, sig.Price, sig.StopLoss, sig.Notional, sig.Leverage)
			return nil
		}
		fmt.Printf("[live] ENTERING %s qty=%.6f stop=%.2f\n", sig.Side, sig.Quantity, sig.StopLoss)
		res, oerr := c.OpenProtectedMarketPosition(ctx, symbol, sig)
		if oerr != nil {
			return fmt.Errorf("open: %w", oerr)
		}
		state.HasPosition = true
		state.Side = string(sig.Side)
		state.Quantity = sig.Quantity
		state.EntryPrice = sig.Price
		state.StopLoss = sig.StopLoss
		state.OpenedAt = time.Now().UTC()
		fmt.Printf("[live] entry=%d stop=%d\n", res.Entry.OrderID, res.Stop.OrderID)
	case strategy.ActionExit:
		if !state.HasPosition {
			fmt.Println("[skip] no position, ignoring EXIT signal")
			return nil
		}
		if !live {
			fmt.Printf("[dry-run] WOULD EXIT %s qty=%.6f at market\n", state.Side, state.Quantity)
			return nil
		}
		closeSide := "SELL"
		if state.Side == string(strategy.Short) {
			closeSide = "BUY"
		}
		fmt.Printf("[live] EXITING %s qty=%.6f via %s market reduceOnly\n", state.Side, state.Quantity, closeSide)
		_, cerr := c.PlaceMarketOrderWithID(ctx, symbol, closeSide, state.Quantity, true,
			fmt.Sprintf("coin_exit_%d", time.Now().UnixMilli()))
		if cerr != nil {
			return fmt.Errorf("close: %w", cerr)
		}
		_ = c.CancelAllOpenOrders(ctx, symbol)
		state.HasPosition = false
		state.Side = ""
		state.Quantity = 0
		state.EntryPrice = 0
		state.StopLoss = 0
	default:
		fmt.Println("[hold] no action")
	}
	saveLiveState(statePath, state)
	return nil
}

func reconcile(ctx context.Context, c *binance.FuturesClient, symbol string, st *LiveState) {
	positions, err := c.Positions(ctx, symbol)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warn] reconcile failed: %v\n", err)
		return
	}
	var exchangeAmt float64
	var entry float64
	for _, p := range positions {
		if strings.EqualFold(p.Symbol, symbol) && math.Abs(p.PositionAmt) > 0 {
			exchangeAmt = p.PositionAmt
			entry = p.EntryPrice
			break
		}
	}
	if math.Abs(exchangeAmt) < 1e-12 {
		if st.HasPosition {
			fmt.Println("[reconcile] local thought we had a position; exchange flat. resetting local state.")
		}
		st.HasPosition = false
		st.Side = ""
		st.Quantity = 0
		st.EntryPrice = 0
		st.StopLoss = 0
		return
	}
	side := string(strategy.Long)
	if exchangeAmt < 0 {
		side = string(strategy.Short)
	}
	if !st.HasPosition || st.Side != side || math.Abs(st.Quantity-math.Abs(exchangeAmt)) > 1e-9 {
		fmt.Printf("[reconcile] adopting exchange position: side=%s qty=%.6f entry=%.2f\n",
			side, math.Abs(exchangeAmt), entry)
	}
	st.HasPosition = true
	st.Side = side
	st.Quantity = math.Abs(exchangeAmt)
	st.EntryPrice = entry
}

func loadLiveState(path, env, symbol string) LiveState {
	st := LiveState{Env: env, Symbol: strings.ToUpper(symbol)}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	st.Env = env
	st.Symbol = strings.ToUpper(symbol)
	return st
}

func saveLiveState(path string, st *LiveState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

func candleDuration(interval string) (time.Duration, error) {
	switch interval {
	case "1m":
		return time.Minute, nil
	case "3m":
		return 3 * time.Minute, nil
	case "5m":
		return 5 * time.Minute, nil
	case "15m":
		return 15 * time.Minute, nil
	case "30m":
		return 30 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "2h":
		return 2 * time.Hour, nil
	case "4h":
		return 4 * time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "8h":
		return 8 * time.Hour, nil
	case "12h":
		return 12 * time.Hour, nil
	case "1d":
		return 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unsupported interval %s", interval)
}

func mustCandleDur(interval string) time.Duration {
	d, err := candleDuration(interval)
	if err != nil {
		return time.Hour
	}
	return d
}

func nextCloseTime(now time.Time, dur time.Duration) time.Time {
	bucket := now.Truncate(dur)
	closeAt := bucket.Add(dur)
	if !closeAt.After(now) {
		closeAt = closeAt.Add(dur)
	}
	return closeAt
}

func sleepWithCancel(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
