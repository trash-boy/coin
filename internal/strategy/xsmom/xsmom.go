// Package xsmom implements a cross-sectional momentum strategy on USD-M
// perpetual futures: rank an eligible universe by past N-day return,
// long the top K (optionally short the bottom M), rebalance every H days.
//
// Validated on 360d Binance USDT-perp (Top 100 by 24h volume) backtest:
//   - L10 / 21d / 7d : +205% total / Sharpe 2.89 / MDD -28% (full 360d, with funding + 8 bps slip)
//   - Walk-forward 60d IS / 30d OOS : 5/7 positive, median OOS sharpe 3.36
//
// IMPORTANT
//   - In current bull regime longs received funding (mean -3.3 bps/day).
//     L/S market-neutral underperformed long-only.
//   - Strategy fails in chop / mild bear regimes (Q1 sharpe -3 in 360d test).
//     Caller is expected to combine with a regime filter (e.g. BTC EMA20).
package xsmom

import (
	"fmt"
	"math"
	"sort"
	"time"

	"coin/internal/strategy"
)

// Config controls universe filtering, ranking and rebalance cadence.
type Config struct {
	// Lookback in days used to rank momentum (e.g. 7 / 14 / 21).
	LookbackDays int
	// HoldDays = rebalance period in days. Rebalance triggers when
	// (i - startIdx) % HoldDays == 0.
	HoldDays int
	// TopN longs to hold; 0 disables long leg (rare).
	TopN int
	// BotN shorts to hold; 0 = long-only (recommended in bull regime).
	BotN int
	// Leverage = gross exposure as a fraction of equity. 1.0 means
	// each leg uses 0.5 equity (or 1.0 if long-only). Avoid > 2x with
	// this strategy: OOS std sharpe is high.
	Leverage float64
	// MinHistDays: a symbol must have at least this many days of price
	// history (since first valid close) before being eligible. Default 60.
	MinHistDays int
	// MinQuoteVolUSD: rolling 30d mean daily quote volume floor in USD.
	// Default 10_000_000 to filter dead pairs and microcaps.
	MinQuoteVolUSD float64
	// FeeBps: per-leg taker fee in basis points (Binance VIP0 = 4 bps).
	FeeBps float64
	// SlipBps: per-leg slippage assumption in basis points.
	// 8 bps reflects avg 30m-VWAP impact for top 100 USDT-perps.
	SlipBps float64
	// IncludeFunding: subtract daily-aggregated funding for longs and
	// add it for shorts (Binance funding settles every 8h).
	IncludeFunding bool
}

// DefaultConfig returns the validated 360d champion config:
// L10 / 21d / 7d, leverage 1x, full bias correction.
func DefaultConfig() Config {
	return Config{
		LookbackDays:   21,
		HoldDays:       7,
		TopN:           10,
		BotN:           0,
		Leverage:       1.0,
		MinHistDays:    60,
		MinQuoteVolUSD: 10_000_000,
		FeeBps:         4.0,
		SlipBps:        8.0,
		IncludeFunding: true,
	}
}

// Validate enforces sane bounds.
func (c Config) Validate() error {
	if c.LookbackDays < 2 {
		return fmt.Errorf("LookbackDays must be >= 2")
	}
	if c.HoldDays < 1 {
		return fmt.Errorf("HoldDays must be >= 1")
	}
	if c.TopN < 0 || c.BotN < 0 || c.TopN+c.BotN == 0 {
		return fmt.Errorf("TopN+BotN must be > 0")
	}
	if c.Leverage <= 0 || c.Leverage > 4 {
		return fmt.Errorf("Leverage must be in (0, 4]; backtest std sharpe forbids >2x in practice")
	}
	if c.MinHistDays < 7 {
		return fmt.Errorf("MinHistDays must be >= 7 to filter brand-new listings")
	}
	if c.MinQuoteVolUSD < 0 {
		return fmt.Errorf("MinQuoteVolUSD cannot be negative")
	}
	if c.FeeBps < 0 || c.SlipBps < 0 {
		return fmt.Errorf("FeeBps and SlipBps cannot be negative")
	}
	return nil
}

// SymbolBars stores the daily-resampled close + quote volume + funding
// for a single symbol.  Index ordering is irrelevant; xsmom aligns by
// time internally.
type SymbolBars struct {
	Symbol string
	// Daily bars (UTC midnight aligned).
	Days []time.Time
	// Close price aligned to Days. NaN denotes missing bar.
	Close []float64
	// Quote volume (typical_price * volume) aligned to Days.  Units = USDT.
	QuoteVol []float64
	// Daily-aggregated funding rate aligned to Days. Positive value
	// means longs pay; negative means shorts pay.
	Funding []float64
}

// Panel is the daily price/volume/funding panel across all symbols.
type Panel struct {
	Symbols    []string
	Days       []time.Time
	close      [][]float64 // shape [nDays][nSymbols]; NaN = missing
	qvol       [][]float64
	funding    [][]float64
	firstValid []int // per symbol, index of first non-NaN close
}

// BuildPanel aligns multi-symbol daily bars into a unified panel.
// Symbols whose data is missing > 50% of days are dropped.
func BuildPanel(bars []SymbolBars) (*Panel, error) {
	if len(bars) == 0 {
		return nil, fmt.Errorf("xsmom: empty bars")
	}
	dayset := map[time.Time]bool{}
	for _, b := range bars {
		for _, t := range b.Days {
			dayset[t] = true
		}
	}
	if len(dayset) == 0 {
		return nil, fmt.Errorf("xsmom: no days found")
	}
	days := make([]time.Time, 0, len(dayset))
	for t := range dayset {
		days = append(days, t)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	dayIdx := make(map[time.Time]int, len(days))
	for i, t := range days {
		dayIdx[t] = i
	}

	nDays := len(days)
	keep := make([]SymbolBars, 0, len(bars))
	for _, b := range bars {
		nValid := 0
		for _, c := range b.Close {
			if !math.IsNaN(c) && c > 0 {
				nValid++
			}
		}
		if nValid*2 >= len(b.Days) {
			keep = append(keep, b)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].Symbol < keep[j].Symbol })

	syms := make([]string, len(keep))
	for i, b := range keep {
		syms[i] = b.Symbol
	}
	nSym := len(keep)

	closeM := make([][]float64, nDays)
	qvolM := make([][]float64, nDays)
	fundM := make([][]float64, nDays)
	for i := range closeM {
		closeM[i] = make([]float64, nSym)
		qvolM[i] = make([]float64, nSym)
		fundM[i] = make([]float64, nSym)
		for j := 0; j < nSym; j++ {
			closeM[i][j] = math.NaN()
		}
	}
	for sIdx, b := range keep {
		for k, t := range b.Days {
			i, ok := dayIdx[t]
			if !ok {
				continue
			}
			if k < len(b.Close) {
				closeM[i][sIdx] = b.Close[k]
			}
			if k < len(b.QuoteVol) {
				qvolM[i][sIdx] = b.QuoteVol[k]
			}
			if k < len(b.Funding) {
				fundM[i][sIdx] = b.Funding[k]
			}
		}
	}
	// forward-fill close gaps up to 1 day, mirroring Python pipeline.
	for j := 0; j < nSym; j++ {
		for i := 1; i < nDays; i++ {
			if math.IsNaN(closeM[i][j]) && !math.IsNaN(closeM[i-1][j]) {
				closeM[i][j] = closeM[i-1][j]
			}
		}
	}
	firstValid := make([]int, nSym)
	for j := 0; j < nSym; j++ {
		fv := -1
		for i := 0; i < nDays; i++ {
			if !math.IsNaN(closeM[i][j]) && closeM[i][j] > 0 {
				fv = i
				break
			}
		}
		firstValid[j] = fv
	}
	return &Panel{
		Symbols:    syms,
		Days:       days,
		close:      closeM,
		qvol:       qvolM,
		funding:    fundM,
		firstValid: firstValid,
	}, nil
}

// EligibleAt returns indices (into Panel.Symbols) that pass the
// listing-age and 30d quote-volume floor at day index `i`.
func (p *Panel) EligibleAt(i int, cfg Config) []int {
	out := make([]int, 0, len(p.Symbols))
	today := p.Days[i]
	lo := i - 30
	if lo < 0 {
		lo = 0
	}
	for j := range p.Symbols {
		fv := p.firstValid[j]
		if fv < 0 {
			continue
		}
		if today.Sub(p.Days[fv]).Hours()/24 < float64(cfg.MinHistDays) {
			continue
		}
		if math.IsNaN(p.close[i][j]) || p.close[i][j] <= 0 {
			continue
		}
		// 30d mean quote vol
		sum := 0.0
		cnt := 0
		for k := lo; k < i; k++ {
			v := p.qvol[k][j]
			if !math.IsNaN(v) {
				sum += v
				cnt++
			}
		}
		if cnt == 0 {
			continue
		}
		if sum/float64(cnt) < cfg.MinQuoteVolUSD {
			continue
		}
		out = append(out, j)
	}
	return out
}

// RankedReturn pairs a symbol index with its lookback return.
type RankedReturn struct {
	SymIdx int
	Symbol string
	Ret    float64
}

// RankAt computes lookback returns for eligible symbols at day index `i`,
// sorted descending by return.
func (p *Panel) RankAt(i int, cfg Config) []RankedReturn {
	elig := p.EligibleAt(i, cfg)
	past := i - cfg.LookbackDays
	if past < 0 {
		return nil
	}
	out := make([]RankedReturn, 0, len(elig))
	for _, j := range elig {
		pp := p.close[past][j]
		pn := p.close[i][j]
		if math.IsNaN(pp) || math.IsNaN(pn) || pp <= 0 {
			continue
		}
		out = append(out, RankedReturn{
			SymIdx: j,
			Symbol: p.Symbols[j],
			Ret:    pn/pp - 1,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Ret > out[b].Ret })
	return out
}

// SelectLongShort returns the long basket and short basket for a
// rebalance at day index `i`. Returns (nil, nil) when the eligible
// universe is too small.
func (p *Panel) SelectLongShort(i int, cfg Config) (longs []RankedReturn, shorts []RankedReturn) {
	r := p.RankAt(i, cfg)
	if len(r) < cfg.TopN+cfg.BotN+5 {
		return nil, nil
	}
	if cfg.TopN > 0 {
		longs = append(longs, r[:cfg.TopN]...)
	}
	if cfg.BotN > 0 {
		shorts = append(shorts, r[len(r)-cfg.BotN:]...)
	}
	return
}

// Position is a held leg of the portfolio.
type Position struct {
	Symbol string
	Side   strategy.Side
	Weight float64 // fraction of equity allocated (always positive)
}

// EnsureCompatible double-checks Panel was built with consistent inputs.
func (p *Panel) EnsureCompatible() error {
	if len(p.Days) == 0 || len(p.Symbols) == 0 {
		return fmt.Errorf("xsmom: panel is empty")
	}
	if len(p.close) != len(p.Days) {
		return fmt.Errorf("xsmom: panel close shape mismatch")
	}
	return nil
}

// LegWeights returns the per-symbol weight on each leg given Leverage.
//   long-only: each long gets Leverage/TopN
//   long-short: each long gets Leverage/2/TopN, each short gets Leverage/2/BotN
func (c Config) LegWeights() (wLong, wShort float64) {
	if c.BotN == 0 {
		if c.TopN > 0 {
			wLong = c.Leverage / float64(c.TopN)
		}
		return
	}
	if c.TopN > 0 {
		wLong = c.Leverage / 2 / float64(c.TopN)
	}
	if c.BotN > 0 {
		wShort = c.Leverage / 2 / float64(c.BotN)
	}
	return
}
