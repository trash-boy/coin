package strategy

import (
	"fmt"
	"math"
	"time"
)

// Aggressive15mConfig returns a tuned configuration for an aggressive
// 15-minute breakout strategy. Caller must still validate the result.
//
// Profile (vs DefaultConfig):
//   - shorter EMAs (21/55/200) suited to 15m
//   - higher leverage cap (5x) and bigger per-trade risk (1.5%)
//   - tighter ATR stop (1.5 ATR) and wider trail (2.5 ATR)
//   - lower trend filters (ADX 20, efficiency 0.20) so it actually trades on 15m
//   - 1h higher TF and 4h major TF, so we still respect direction
func Aggressive15mConfig() Config {
	c := DefaultConfig()

	c.Mode = "aggressive_15m"

	// Faster EMAs for 15m
	c.FastEMA = 21
	c.SlowEMA = 55
	c.TrendEMA = 200
	c.ATRPeriod = 14

	// Higher TF filters: 1h and 4h (vs 4h/24h in default)
	c.HigherTFHours = 1
	c.MajorTFHours = 4
	c.HigherFastEMA = 21
	c.HigherSlowEMA = 55

	// Trend / regime: looser, because 15m has less persistence
	c.MinADXLong = 20
	c.MinADXShort = 22
	c.MinEfficiency = 0.20

	// Breakout entry
	c.BreakoutLookback = 20
	c.VolumeMultiplier = 1.5
	c.EntryATRStop = 1.5

	// Stops
	c.MinStopATR = 1.0
	c.MaxStopATR = 2.5
	c.SwingLookback = 10
	c.StructureBufferATR = 0.20

	// Risk (aggressive)
	c.RiskPerTrade = 0.015
	c.MaxLeverage = 5.0
	c.MaxMarginUse = 0.85
	c.LongRiskMultiplier = 1.0
	c.ShortRiskMultiplier = 0.85

	// Pyramiding: faster / more aggressive
	c.MaxAdds = 1
	c.MinAddSpacingBars = 4
	c.PyramidATR = 1.0
	c.PyramidRiskScale = 0.50

	// Trailing: take profits aggressively
	c.BreakEvenR = 1.0
	c.TrailActivationR = 1.5
	c.TrailATR = 2.5

	// Account-level risk
	c.MaxDrawdown = 0.20
	c.DailyLossLimit = 0.03
	c.CooldownBars = 8

	// Volatility leverage targeting
	c.VolTargetATRRatio = 0.005

	// Volatility floor/ceiling for 15m
	c.MinATRRatio = 0.0008
	c.MaxATRRatio = 0.05

	// Fees and slippage already reasonable; keep defaults.
	return c
}

// entrySideAggressive15m implements the 15m aggressive breakout signal.
//
// Long entry conditions:
//   - 15m close > Donchian-N high of prior bars
//   - bar volume >= avg(volume, N) * VolumeMultiplier
//   - EMA21 > EMA55 > EMA200, EMA21 > EMA21[-3] (rising)
//   - ADX(14) >= MinADXLong
//   - 1h higher trend AND 4h major trend both bullish
//   - funding rate not extreme against us
//   - ATR/close inside [MinATRRatio, MaxATRRatio]
//
// Short conditions are symmetric.
func entrySideAggressive15m(candles []Candle, ind Indicators, i int, cfg Config, fundingRate float64) (Side, string) {
	warmup := WarmupBarsForCandles(candles, cfg)
	lookback := cfg.BreakoutLookback
	if lookback < 5 {
		lookback = 20
	}
	if i < warmup || i < lookback+1 {
		return Flat, "not enough warmed-up indicator history"
	}
	if candles[i].Close <= 0 || ind.ATR[i] <= 0 {
		return Flat, "invalid price or ATR"
	}

	price := candles[i].Close
	volRatio := ind.ATR[i] / price
	if volRatio < cfg.MinATRRatio {
		return Flat, fmt.Sprintf("volatility too low: ATR/close %.4f < %.4f", volRatio, cfg.MinATRRatio)
	}
	if volRatio > cfg.MaxATRRatio {
		return Flat, fmt.Sprintf("volatility too high: ATR/close %.4f > %.4f", volRatio, cfg.MaxATRRatio)
	}
	if ind.Efficiency[i] < cfg.MinEfficiency {
		return Flat, fmt.Sprintf("range regime: efficiency %.2f < %.2f", ind.Efficiency[i], cfg.MinEfficiency)
	}

	// Donchian breakout reference uses bars [i-lookback, i-1] (do not include current bar).
	hh := HighestHigh(candles, i-1, lookback)
	ll := LowestLow(candles, i-1, lookback)

	// Average volume over the same window.
	volSum := 0.0
	for j := i - lookback; j <= i-1; j++ {
		if j < 0 {
			continue
		}
		volSum += candles[j].Volume
	}
	volAvg := volSum / float64(lookback)
	volOK := volAvg <= 0 || candles[i].Volume >= volAvg*cfg.VolumeMultiplier

	emaUp := ind.Fast[i] > ind.Slow[i] && ind.Slow[i] > ind.Trend[i] &&
		ind.Fast[i] > ind.Fast[i-3] && ind.Trend[i] >= ind.Trend[i-5]
	emaDown := ind.Fast[i] < ind.Slow[i] && ind.Slow[i] < ind.Trend[i] &&
		ind.Fast[i] < ind.Fast[i-3] && ind.Trend[i] <= ind.Trend[i-5]

	longBreakout := candles[i].Close > hh && emaUp
	shortBreakout := candles[i].Close < ll && emaDown

	if longBreakout {
		if !volOK {
			return Flat, fmt.Sprintf("long breakout but weak volume: %.2f < avg*%.2f", candles[i].Volume, cfg.VolumeMultiplier)
		}
		if ind.ADX[i] < cfg.MinADXLong {
			return Flat, fmt.Sprintf("long ADX too weak: %.1f < %.1f", ind.ADX[i], cfg.MinADXLong)
		}
		if ind.Higher[i] <= 0 || ind.Major[i] <= 0 {
			return Flat, "long rejected by 1h/4h higher timeframe filter"
		}
		if fundingRate > cfg.FundingBlockLong {
			return Flat, fmt.Sprintf("long funding too expensive: %.4f%%", fundingRate*100)
		}
		return Long, fmt.Sprintf("15m long breakout: close %.2f > hh%d %.2f", price, lookback, hh)
	}
	if shortBreakout {
		if !volOK {
			return Flat, fmt.Sprintf("short breakout but weak volume: %.2f < avg*%.2f", candles[i].Volume, cfg.VolumeMultiplier)
		}
		if ind.ADX[i] < cfg.MinADXShort {
			return Flat, fmt.Sprintf("short ADX too weak: %.1f < %.1f", ind.ADX[i], cfg.MinADXShort)
		}
		if ind.Higher[i] >= 0 || ind.Major[i] >= 0 {
			return Flat, "short rejected by 1h/4h higher timeframe filter"
		}
		if fundingRate < -cfg.FundingBlockShort {
			return Flat, fmt.Sprintf("short funding too expensive: %.4f%%", fundingRate*100)
		}
		return Short, fmt.Sprintf("15m short breakout: close %.2f < ll%d %.2f", price, lookback, ll)
	}

	return Flat, "no 15m breakout aligned with trend"
}

// structureStopAggressive15m sets a tight ATR-based initial stop, optionally
// pulled back to the recent swing if the swing is closer (gives breakout
// pullbacks some room without giving up much risk).
func structureStopAggressive15m(candles []Candle, ind Indicators, i int, side Side, cfg Config) (float64, bool, string) {
	if i < 1 || ind.ATR[i] <= 0 || candles[i].Close <= 0 {
		return 0, false, "invalid ATR or price"
	}
	price := candles[i].Close
	atr := ind.ATR[i]
	stopMult := cfg.EntryATRStop
	if stopMult <= 0 {
		stopMult = 1.5
	}
	swingLookback := cfg.SwingLookback
	if swingLookback < 4 {
		swingLookback = 10
	}

	if side == Long {
		atrStop := price - stopMult*atr
		swing := LowestLow(candles, i-1, swingLookback) - cfg.StructureBufferATR*atr
		stop := atrStop
		if swing > stop && swing < price { // pull stop tighter if recent swing is closer
			stop = swing
		}
		distanceATR := (price - stop) / atr
		if distanceATR > cfg.MaxStopATR {
			return 0, false, fmt.Sprintf("long stop too wide: %.2f ATR", distanceATR)
		}
		if distanceATR < math.Min(cfg.MinStopATR, stopMult)-0.05 {
			// Should not happen, but guard against zero distance
			return 0, false, "long stop too close"
		}
		return stop, true, fmt.Sprintf("aggressive long stop %.2f ATR below entry", distanceATR)
	}
	if side == Short {
		atrStop := price + stopMult*atr
		swing := HighestHigh(candles, i-1, swingLookback) + cfg.StructureBufferATR*atr
		stop := atrStop
		if swing < stop && swing > price {
			stop = swing
		}
		distanceATR := (stop - price) / atr
		if distanceATR > cfg.MaxStopATR {
			return 0, false, fmt.Sprintf("short stop too wide: %.2f ATR", distanceATR)
		}
		if distanceATR < math.Min(cfg.MinStopATR, stopMult)-0.05 {
			return 0, false, "short stop too close"
		}
		return stop, true, fmt.Sprintf("aggressive short stop %.2f ATR above entry", distanceATR)
	}
	return 0, false, "flat side has no stop"
}

// Compile-time guard: keep the time import used even if future edits remove
// the higher-timeframe references.
var _ = time.Hour
