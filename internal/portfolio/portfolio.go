package portfolio

import (
	"fmt"
	"sort"
	"strings"

	"coin/internal/backtest"
	"coin/internal/strategy"
)

type SymbolData struct {
	Candles []strategy.Candle
	Funding []strategy.FundingRate
	Config  strategy.Config
}

type SymbolResult struct {
	Symbol     string
	Allocation float64
	Result     backtest.Result
}

type Result struct {
	InitialEquity float64
	FinalEquity   float64
	ReturnPct     float64
	TotalTrades   int
	Results       []SymbolResult
}

func RunEqualWeight(data map[string]SymbolData, initialEquity float64, cfg strategy.Config) (Result, error) {
	if initialEquity <= 0 {
		return Result{}, fmt.Errorf("initial equity must be positive")
	}
	symbols := sortedSymbols(data)
	if len(symbols) == 0 {
		return Result{}, fmt.Errorf("no symbols provided")
	}
	allocation := initialEquity / float64(len(symbols))
	out := Result{
		InitialEquity: initialEquity,
		Results:       make([]SymbolResult, 0, len(symbols)),
	}
	for _, symbol := range symbols {
		item := data[symbol]
		symbolCfg := cfg
		if item.Config.FastEMA > 0 {
			symbolCfg = item.Config
		}
		result, err := backtest.RunWithFunding(item.Candles, allocation, symbolCfg, item.Funding)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", symbol, err)
		}
		out.FinalEquity += result.FinalEquity
		out.TotalTrades += len(result.Trades)
		out.Results = append(out.Results, SymbolResult{
			Symbol:     strings.ToUpper(symbol),
			Allocation: allocation,
			Result:     result,
		})
	}
	out.ReturnPct = (out.FinalEquity/out.InitialEquity - 1) * 100
	return out, nil
}

type SignalResult struct {
	Symbol string
	Signal strategy.Signal
}

func LatestSignals(data map[string][]strategy.Candle, equity float64, cfg strategy.Config, fundingRates map[string]float64) ([]SignalResult, error) {
	symbols := sortedCandleSymbols(data)
	if len(symbols) == 0 {
		return nil, fmt.Errorf("no symbols provided")
	}
	allocation := equity / float64(len(symbols))
	out := make([]SignalResult, 0, len(symbols))
	for _, symbol := range symbols {
		sig, err := strategy.LatestSignal(data[symbol], strategy.Context{
			Equity:      allocation,
			FundingRate: fundingRates[strings.ToUpper(symbol)],
		}, cfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", symbol, err)
		}
		out = append(out, SignalResult{Symbol: strings.ToUpper(symbol), Signal: sig})
	}
	return out, nil
}

func sortedSymbols(data map[string]SymbolData) []string {
	out := make([]string, 0, len(data))
	for symbol := range data {
		out = append(out, symbol)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToUpper(out[i]) < strings.ToUpper(out[j])
	})
	return out
}

func sortedCandleSymbols(data map[string][]strategy.Candle) []string {
	out := make([]string, 0, len(data))
	for symbol := range data {
		out = append(out, symbol)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToUpper(out[i]) < strings.ToUpper(out[j])
	})
	return out
}
