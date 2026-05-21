package strategy

import (
	"fmt"
	"math"
	"time"
)

type Context struct {
	Equity      float64
	FundingRate float64
	RiskScale   float64
}

type Indicators struct {
	Fast       []float64
	Slow       []float64
	Trend      []float64
	ATR        []float64
	ADX        []float64
	Efficiency []float64
	Higher     []int
	Major      []int
}

func BuildIndicators(candles []Candle, cfg Config) Indicators {
	closes := Closes(candles)
	return Indicators{
		Fast:       EMA(closes, cfg.FastEMA),
		Slow:       EMA(closes, cfg.SlowEMA),
		Trend:      EMA(closes, cfg.TrendEMA),
		ATR:        ATR(candles, cfg.ATRPeriod),
		ADX:        ADX(candles, cfg.ADXPeriod),
		Efficiency: EfficiencyRatio(closes, cfg.EfficiencyPeriod),
		Higher:     HigherTrendSeries(candles, time.Duration(cfg.HigherTFHours)*time.Hour, cfg.HigherFastEMA, cfg.HigherSlowEMA),
		Major:      HigherTrendSeries(candles, time.Duration(cfg.MajorTFHours)*time.Hour, cfg.HigherFastEMA, cfg.HigherSlowEMA),
	}
}

func LatestSignal(candles []Candle, ctx Context, cfg Config) (Signal, error) {
	if err := cfg.Validate(); err != nil {
		return Signal{}, err
	}
	warmup := WarmupBarsForCandles(candles, cfg)
	if len(candles) < warmup {
		return Signal{}, fmt.Errorf("need at least %d candles, got %d", warmup, len(candles))
	}
	if ctx.Equity <= 0 {
		return Signal{}, fmt.Errorf("equity must be positive")
	}

	ind := BuildIndicators(candles, cfg)
	i := len(candles) - 1
	side, reason := EntrySide(candles, ind, i, cfg, ctx.FundingRate)
	if side == Flat {
		return Signal{
			Time:   candles[i].CloseTime,
			Action: ActionNone,
			Side:   Flat,
			Price:  candles[i].Close,
			Reason: reason,
		}, nil
	}

	stop, ok, stopReason := StructureStop(candles, ind, i, side, cfg)
	if !ok {
		return Signal{
			Time:   candles[i].CloseTime,
			Action: ActionNone,
			Side:   Flat,
			Price:  candles[i].Close,
			Reason: stopReason,
		}, nil
	}

	sig := BuildEntrySignal(candles[i], side, ctx.Equity, ind.ATR[i], stop, ctx.RiskScale, ctx.FundingRate, cfg)
	sig.Reason = reason + "; " + stopReason
	return sig, nil
}

func EntrySide(candles []Candle, ind Indicators, i int, cfg Config, fundingRate float64) (Side, string) {
	if i < WarmupBarsForCandles(candles, cfg) || candles[i].Close <= 0 {
		return Flat, "not enough warmed-up indicator history"
	}
	volRatio := ind.ATR[i] / candles[i].Close
	if volRatio < cfg.MinATRRatio {
		return Flat, fmt.Sprintf("volatility too low: ATR/close %.4f < %.4f", volRatio, cfg.MinATRRatio)
	}
	if volRatio > cfg.MaxATRRatio {
		return Flat, fmt.Sprintf("volatility too high: ATR/close %.4f > %.4f", volRatio, cfg.MaxATRRatio)
	}
	if ind.Efficiency[i] < cfg.MinEfficiency {
		return Flat, fmt.Sprintf("range regime: efficiency %.2f < %.2f", ind.Efficiency[i], cfg.MinEfficiency)
	}

	longTrend := ind.Fast[i] > ind.Slow[i] && candles[i].Close > ind.Trend[i] && ind.Slow[i] > ind.Slow[i-3] && ind.Trend[i] > ind.Trend[i-5]
	shortTrend := ind.Fast[i] < ind.Slow[i] && candles[i].Close < ind.Trend[i] && ind.Slow[i] < ind.Slow[i-3] && ind.Trend[i] < ind.Trend[i-5]

	if longTrend {
		if ind.ADX[i] < cfg.MinADXLong {
			return Flat, fmt.Sprintf("long trend too weak: ADX %.1f < %.1f", ind.ADX[i], cfg.MinADXLong)
		}
		if ind.Higher[i] <= 0 || ind.Major[i] <= 0 {
			return Flat, "long rejected by higher timeframe filter"
		}
		if fundingRate > cfg.FundingBlockLong {
			return Flat, fmt.Sprintf("long funding too expensive: %.4f%%", fundingRate*100)
		}
		if !hadLongPullback(candles, ind, i, cfg) {
			return Flat, "long trend present but no pullback entry"
		}
		return Long, "long pullback in confirmed trend regime"
	}

	if shortTrend {
		if ind.ADX[i] < cfg.MinADXShort {
			return Flat, fmt.Sprintf("short trend too weak: ADX %.1f < %.1f", ind.ADX[i], cfg.MinADXShort)
		}
		if ind.Higher[i] >= 0 || ind.Major[i] >= 0 {
			return Flat, "short rejected by stricter higher timeframe filter"
		}
		if fundingRate < -cfg.FundingBlockShort {
			return Flat, fmt.Sprintf("short funding too expensive: %.4f%%", fundingRate*100)
		}
		if !hadShortPullback(candles, ind, i, cfg) {
			return Flat, "short trend present but no pullback entry"
		}
		return Short, "short pullback in confirmed trend regime"
	}

	return Flat, "trend filters not aligned"
}

func TrendSide(candles []Candle, fast, slow, trend, atr []float64, i int, cfg Config) (Side, string) {
	ind := Indicators{
		Fast:       fast,
		Slow:       slow,
		Trend:      trend,
		ATR:        atr,
		ADX:        ADX(candles, cfg.ADXPeriod),
		Efficiency: EfficiencyRatio(Closes(candles), cfg.EfficiencyPeriod),
		Higher:     HigherTrendSeries(candles, time.Duration(cfg.HigherTFHours)*time.Hour, cfg.HigherFastEMA, cfg.HigherSlowEMA),
		Major:      HigherTrendSeries(candles, time.Duration(cfg.MajorTFHours)*time.Hour, cfg.HigherFastEMA, cfg.HigherSlowEMA),
	}
	return EntrySide(candles, ind, i, cfg, 0)
}

func StructureStop(candles []Candle, ind Indicators, i int, side Side, cfg Config) (float64, bool, string) {
	price := candles[i].Close
	atr := ind.ATR[i]
	if atr <= 0 || price <= 0 {
		return 0, false, "invalid ATR or price"
	}

	if side == Long {
		swing := LowestLow(candles, i-1, cfg.SwingLookback)
		stop := swing - cfg.StructureBufferATR*atr
		minStop := price - cfg.MinStopATR*atr
		if stop > minStop {
			stop = minStop
		}
		distanceATR := (price - stop) / atr
		if distanceATR > cfg.MaxStopATR {
			return 0, false, fmt.Sprintf("long structure stop too wide: %.2f ATR", distanceATR)
		}
		return stop, true, fmt.Sprintf("structure stop %.2f ATR below entry", distanceATR)
	}

	if side == Short {
		swing := HighestHigh(candles, i-1, cfg.SwingLookback)
		stop := swing + cfg.StructureBufferATR*atr
		minStop := price + cfg.MinStopATR*atr
		if stop < minStop {
			stop = minStop
		}
		distanceATR := (stop - price) / atr
		if distanceATR > cfg.MaxStopATR {
			return 0, false, fmt.Sprintf("short structure stop too wide: %.2f ATR", distanceATR)
		}
		return stop, true, fmt.Sprintf("structure stop %.2f ATR above entry", distanceATR)
	}
	return 0, false, "flat side has no stop"
}

func BuildEntrySignal(c Candle, side Side, equity, atr, stop, riskScale, fundingRate float64, cfg Config) Signal {
	price := c.Close
	stopDistance := math.Abs(price - stop)
	if riskScale <= 0 {
		riskScale = 1
	}
	riskMultiplier := cfg.LongRiskMultiplier
	if side == Short {
		riskMultiplier = cfg.ShortRiskMultiplier
	}
	if isAdverseFunding(side, fundingRate, cfg) {
		riskScale *= 0.50
	}

	riskBudget := equity * cfg.RiskPerTrade * riskMultiplier * riskScale
	riskQty := 0.0
	if stopDistance > 0 {
		riskQty = riskBudget / stopDistance
	}
	leverage := EffectiveLeverage(price, atr, cfg)
	capQty := (equity * leverage * cfg.MaxMarginUse) / price
	qty := math.Min(riskQty, capQty)
	notional := qty * price
	if !LiquidationGuard(price, side, stop, leverage, notional, equity, cfg) {
		return Signal{
			Time:   c.CloseTime,
			Action: ActionNone,
			Side:   Flat,
			Price:  price,
			Reason: "structure stop is too close to estimated liquidation distance",
		}
	}

	return Signal{
		Time:       c.CloseTime,
		Action:     ActionEnter,
		Side:       side,
		Price:      price,
		Quantity:   qty,
		Notional:   notional,
		Leverage:   leverage,
		StopLoss:   stop,
		TakeProfit: 0,
	}
}

func EstimatedLiquidationPrice(entry float64, side Side, leverage float64, notional float64, accountEquity float64, cfg Config) float64 {
	if entry <= 0 || leverage <= 0 || notional <= 0 {
		return 0
	}
	bracket := MaintenanceBracketForNotional(notional, cfg)
	qty := notional / entry
	margin := notional / leverage
	if cfg.MarginMode == MarginCross && accountEquity > margin {
		margin = accountEquity
	}
	if side == Long {
		numerator := entry*qty - margin - bracket.Cum
		denominator := qty * (1 - bracket.MaintMarginRatio)
		if denominator <= 0 {
			return 0
		}
		return numerator / denominator
	}
	if side == Short {
		numerator := margin + entry*qty + bracket.Cum
		denominator := qty * (1 + bracket.MaintMarginRatio)
		if denominator <= 0 {
			return 0
		}
		return numerator / denominator
	}
	return 0
}

func LiquidationGuard(entry float64, side Side, stop float64, leverage float64, notional float64, accountEquity float64, cfg Config) bool {
	if leverage <= 1 {
		return true
	}
	if notional <= 0 {
		return false
	}
	liq := EstimatedLiquidationPrice(entry, side, leverage, notional, accountEquity, cfg)
	if liq <= 0 {
		return false
	}
	stopDistance := math.Abs(entry - stop)
	liqDistance := math.Abs(entry - liq)
	return stopDistance < cfg.LiquidationStopBuffer*liqDistance
}

func MaintenanceBracketForNotional(notional float64, cfg Config) MaintenanceBracket {
	for _, bracket := range cfg.MaintenanceBrackets {
		if notional >= bracket.NotionalFloor && (bracket.NotionalCap <= 0 || notional < bracket.NotionalCap) {
			return bracket
		}
	}
	return MaintenanceBracket{
		NotionalFloor:    0,
		NotionalCap:      0,
		MaintMarginRatio: cfg.FallbackMMR,
		Cum:              0,
		InitialLeverage:  int(cfg.MaxLeverage),
	}
}

func EffectiveLeverage(price, atr float64, cfg Config) float64 {
	if price <= 0 || atr <= 0 {
		return cfg.MinEffectiveLeverage
	}
	volRatio := atr / price
	lev := cfg.VolTargetATRRatio / volRatio
	if lev < cfg.MinEffectiveLeverage {
		return cfg.MinEffectiveLeverage
	}
	if lev > cfg.MaxLeverage {
		return cfg.MaxLeverage
	}
	return lev
}

func ManagedTrailingStop(side Side, entryPrice, currentStop, closePrice, atr float64, cfg Config) (float64, bool) {
	if entryPrice <= 0 || currentStop <= 0 || closePrice <= 0 || atr <= 0 {
		return currentStop, false
	}
	risk := math.Abs(entryPrice - currentStop)
	if risk <= 0 {
		return currentStop, false
	}
	next := currentStop
	if side == Long {
		r := (closePrice - entryPrice) / risk
		if r >= cfg.BreakEvenR && entryPrice > next {
			next = entryPrice
		}
		if r >= cfg.TrailActivationR {
			trail := closePrice - atr*cfg.TrailATR
			if trail > next {
				next = trail
			}
		}
		return next, next > currentStop
	}
	if side == Short {
		r := (entryPrice - closePrice) / risk
		if r >= cfg.BreakEvenR && entryPrice < next {
			next = entryPrice
		}
		if r >= cfg.TrailActivationR {
			trail := closePrice + atr*cfg.TrailATR
			if trail < next {
				next = trail
			}
		}
		return next, next < currentStop
	}
	return currentStop, false
}

func hadLongPullback(candles []Candle, ind Indicators, i int, cfg Config) bool {
	start := i - cfg.PullbackBars
	if start < 1 {
		start = 1
	}
	touched := false
	for j := start; j < i; j++ {
		if candles[j].Low <= ind.Fast[j] || candles[j].Low <= ind.Slow[j] {
			touched = true
			break
		}
	}
	if !touched {
		return false
	}
	distanceATR := math.Abs(candles[i].Close-ind.Fast[i]) / ind.ATR[i]
	return candles[i].Close > ind.Fast[i] && candles[i].Close > candles[i].Open && distanceATR <= cfg.MaxEntryATRDistance
}

func hadShortPullback(candles []Candle, ind Indicators, i int, cfg Config) bool {
	start := i - cfg.PullbackBars
	if start < 1 {
		start = 1
	}
	touched := false
	for j := start; j < i; j++ {
		if candles[j].High >= ind.Fast[j] || candles[j].High >= ind.Slow[j] {
			touched = true
			break
		}
	}
	if !touched {
		return false
	}
	distanceATR := math.Abs(candles[i].Close-ind.Fast[i]) / ind.ATR[i]
	return candles[i].Close < ind.Fast[i] && candles[i].Close < candles[i].Open && distanceATR <= cfg.MaxEntryATRDistance
}

func isAdverseFunding(side Side, fundingRate float64, cfg Config) bool {
	return (side == Long && fundingRate > cfg.FundingRiskCut) || (side == Short && fundingRate < -cfg.FundingRiskCut)
}
