package xsmom

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"coin/internal/binance"
	"coin/internal/strategy"
)

// FetchOptions configures the BuildPanelFromBinance helper.
type FetchOptions struct {
	Symbols  []string
	Days     int    // lookback days, default 360
	Interval string // kline interval used for source bars; default "30m"
	// MaxConcurrent limits parallel HTTP fetches; 0 = serial. Binance
	// rate limits are forgiving for klines so 4-8 is fine.
	MaxConcurrent int
	// MinSampleBars: a symbol must yield at least this many source bars
	// (post download) to be included. Default 1000.
	MinSampleBars int
	// IncludeFunding pulls funding history for each symbol. Default true.
	IncludeFunding bool
}

// SymbolFailure is returned alongside the panel for any symbol that
// could not be loaded.
type SymbolFailure struct {
	Symbol string
	Err    error
}

// BuildPanelFromBinance downloads klines + funding for `opts.Symbols`,
// resamples to daily UTC bars and returns an aligned Panel.
func BuildPanelFromBinance(ctx context.Context, client *binance.FuturesClient, opts FetchOptions) (*Panel, []SymbolFailure, error) {
	if len(opts.Symbols) == 0 {
		return nil, nil, fmt.Errorf("xsmom: no symbols")
	}
	if opts.Days <= 0 {
		opts.Days = 360
	}
	if opts.Interval == "" {
		opts.Interval = "30m"
	}
	if opts.MinSampleBars <= 0 {
		opts.MinSampleBars = 1000
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 1
	}
	end := time.Now().UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(opts.Days) * 24 * time.Hour)

	type job struct {
		bars SymbolBars
		err  error
	}
	results := make([]job, len(opts.Symbols))
	sem := make(chan struct{}, opts.MaxConcurrent)
	var wg sync.WaitGroup
	for idx, sym := range opts.Symbols {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, sym string) {
			defer wg.Done()
			defer func() { <-sem }()
			candles, err := client.KlinesRange(ctx, sym, opts.Interval, start, end)
			if err != nil {
				results[idx] = job{err: fmt.Errorf("klines: %w", err)}
				return
			}
			if len(candles) < opts.MinSampleBars {
				results[idx] = job{err: fmt.Errorf("only %d %s bars (< %d)", len(candles), opts.Interval, opts.MinSampleBars)}
				return
			}
			var funding []strategy.FundingRate
			if opts.IncludeFunding {
				f, ferr := client.FundingRates(ctx, sym, start, end)
				if ferr != nil {
					funding = nil
				} else {
					funding = f
				}
			}
			bars := resampleDaily(sym, candles, funding)
			results[idx] = job{bars: bars}
		}(idx, sym)
	}
	wg.Wait()

	bars := make([]SymbolBars, 0, len(results))
	failures := make([]SymbolFailure, 0)
	for i, r := range results {
		if r.err != nil {
			failures = append(failures, SymbolFailure{Symbol: opts.Symbols[i], Err: r.err})
			continue
		}
		bars = append(bars, r.bars)
	}
	if len(bars) == 0 {
		return nil, failures, fmt.Errorf("xsmom: no symbols produced bars")
	}
	p, err := BuildPanel(bars)
	if err != nil {
		return nil, failures, err
	}
	return p, failures, nil
}

// resampleDaily rolls up intraday klines into UTC-midnight daily bars.
//
//	close   = last close in day
//	qvol    = sum(typical_price * volume) over the day
//	funding = sum of fundingRate timestamps inside the day
func resampleDaily(sym string, candles []strategy.Candle, funding []strategy.FundingRate) SymbolBars {
	if len(candles) == 0 {
		return SymbolBars{Symbol: sym}
	}
	type acc struct {
		lastClose float64
		qvolSum   float64
		any       bool
	}
	bucket := map[time.Time]*acc{}
	for _, c := range candles {
		day := time.Date(c.OpenTime.Year(), c.OpenTime.Month(), c.OpenTime.Day(), 0, 0, 0, 0, time.UTC)
		a := bucket[day]
		if a == nil {
			a = &acc{lastClose: math.NaN()}
			bucket[day] = a
		}
		a.lastClose = c.Close
		typ := (c.High + c.Low + c.Close) / 3
		a.qvolSum += typ * c.Volume
		a.any = true
	}
	days := make([]time.Time, 0, len(bucket))
	for t := range bucket {
		days = append(days, t)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	closes := make([]float64, len(days))
	qvols := make([]float64, len(days))
	for i, d := range days {
		a := bucket[d]
		closes[i] = a.lastClose
		qvols[i] = a.qvolSum
	}
	fundMap := map[time.Time]float64{}
	for _, fr := range funding {
		day := time.Date(fr.Time.Year(), fr.Time.Month(), fr.Time.Day(), 0, 0, 0, 0, time.UTC)
		fundMap[day] += fr.Rate
	}
	fundArr := make([]float64, len(days))
	for i, d := range days {
		fundArr[i] = fundMap[d]
	}

	return SymbolBars{
		Symbol:   sym,
		Days:     days,
		Close:    closes,
		QuoteVol: qvols,
		Funding:  fundArr,
	}
}

// TopUSDTPerpsByVolume queries Binance USDM-futures and returns the
// top-N USDT-quoted PERPETUAL symbols by 24h quote volume, with
// leveraged tokens (UP/DOWN/BULL/BEAR suffixes) excluded.
func TopUSDTPerpsByVolume(ctx context.Context, client *binance.FuturesClient, topN int, minQuoteVolUSDT float64) ([]string, error) {
	listed, err := client.ListUSDTPerpetuals(ctx, minQuoteVolUSDT)
	if err != nil {
		return nil, err
	}
	filtered := listed[:0]
	for _, s := range listed {
		u := strings.ToUpper(s.Symbol)
		if strings.HasSuffix(u, "UPUSDT") || strings.HasSuffix(u, "DOWNUSDT") ||
			strings.HasSuffix(u, "BULLUSDT") || strings.HasSuffix(u, "BEARUSDT") {
			continue
		}
		filtered = append(filtered, s)
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].QuoteVolumeUSDT > filtered[j].QuoteVolumeUSDT
	})
	if topN > 0 && topN < len(filtered) {
		filtered = filtered[:topN]
	}
	out := make([]string, len(filtered))
	for i, s := range filtered {
		out[i] = s.Symbol
	}
	return out, nil
}
