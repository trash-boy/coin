package strategy

import (
	"math"
	"time"
)

func EMA(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if len(values) < period || period <= 0 {
		return out
	}
	alpha := 2.0 / float64(period+1)
	seed := 0.0
	for i := 0; i < period; i++ {
		seed += values[i]
	}
	out[period-1] = seed / float64(period)
	for i := period; i < len(values); i++ {
		out[i] = alpha*values[i] + (1-alpha)*out[i-1]
	}
	return out
}

func ATR(candles []Candle, period int) []float64 {
	out := make([]float64, len(candles))
	if len(candles) <= period || period <= 0 {
		return out
	}
	tr := make([]float64, len(candles))
	for i := range candles {
		if i == 0 {
			tr[i] = candles[i].High - candles[i].Low
			continue
		}
		hl := candles[i].High - candles[i].Low
		hc := math.Abs(candles[i].High - candles[i-1].Close)
		lc := math.Abs(candles[i].Low - candles[i-1].Close)
		tr[i] = math.Max(hl, math.Max(hc, lc))
	}
	seed := 0.0
	for i := 1; i <= period; i++ {
		seed += tr[i]
	}
	out[period] = seed / float64(period)
	for i := period + 1; i < len(tr); i++ {
		out[i] = (out[i-1]*float64(period-1) + tr[i]) / float64(period)
	}
	return out
}

func ADX(candles []Candle, period int) []float64 {
	out := make([]float64, len(candles))
	if len(candles) == 0 || period <= 0 {
		return out
	}

	tr := make([]float64, len(candles))
	plusDM := make([]float64, len(candles))
	minusDM := make([]float64, len(candles))
	for i := 1; i < len(candles); i++ {
		upMove := candles[i].High - candles[i-1].High
		downMove := candles[i-1].Low - candles[i].Low
		if upMove > downMove && upMove > 0 {
			plusDM[i] = upMove
		}
		if downMove > upMove && downMove > 0 {
			minusDM[i] = downMove
		}
		hl := candles[i].High - candles[i].Low
		hc := math.Abs(candles[i].High - candles[i-1].Close)
		lc := math.Abs(candles[i].Low - candles[i-1].Close)
		tr[i] = math.Max(hl, math.Max(hc, lc))
	}

	smTR := wilder(tr, period)
	smPlus := wilder(plusDM, period)
	smMinus := wilder(minusDM, period)
	dx := make([]float64, len(candles))
	for i := range candles {
		if smTR[i] == 0 {
			continue
		}
		plusDI := 100 * smPlus[i] / smTR[i]
		minusDI := 100 * smMinus[i] / smTR[i]
		denom := plusDI + minusDI
		if denom == 0 {
			continue
		}
		dx[i] = 100 * math.Abs(plusDI-minusDI) / denom
	}
	return wilder(dx, period)
}

func EfficiencyRatio(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if period <= 0 {
		return out
	}
	for i := range values {
		if i < period {
			continue
		}
		direction := math.Abs(values[i] - values[i-period])
		noise := 0.0
		for j := i - period + 1; j <= i; j++ {
			noise += math.Abs(values[j] - values[j-1])
		}
		if noise > 0 {
			out[i] = direction / noise
		}
	}
	return out
}

func wilder(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if len(values) <= period || period <= 0 {
		return out
	}
	seed := 0.0
	for i := 1; i <= period; i++ {
		seed += values[i]
	}
	out[period] = seed / float64(period)
	for i := period + 1; i < len(values); i++ {
		out[i] = (out[i-1]*float64(period-1) + values[i]) / float64(period)
	}
	return out
}

func Closes(candles []Candle) []float64 {
	out := make([]float64, len(candles))
	for i := range candles {
		out[i] = candles[i].Close
	}
	return out
}

func WarmupBars(cfg Config) int {
	return WarmupBarsForDuration(cfg, time.Hour)
}

func WarmupBarsForDuration(cfg Config, candleDuration time.Duration) int {
	if candleDuration <= 0 {
		candleDuration = time.Hour
	}
	barsPerHigher := int(math.Ceil(float64(time.Duration(cfg.HigherTFHours)*time.Hour) / float64(candleDuration)))
	if barsPerHigher < 1 {
		barsPerHigher = 1
	}
	barsPerMajor := int(math.Ceil(float64(time.Duration(cfg.MajorTFHours)*time.Hour) / float64(candleDuration)))
	if barsPerMajor < 1 {
		barsPerMajor = 1
	}
	base := maxInt(cfg.TrendEMA*3, maxInt(cfg.SlowEMA*3, maxInt(cfg.ATRPeriod*3, maxInt(cfg.ADXPeriod*3, cfg.EfficiencyPeriod*3))))
	base = maxInt(base, cfg.HigherSlowEMA*barsPerHigher*3)
	base = maxInt(base, cfg.HigherSlowEMA*barsPerMajor*3)
	return base + maxInt(cfg.PullbackBars, cfg.SwingLookback) + 5
}

func WarmupBarsForCandles(candles []Candle, cfg Config) int {
	if len(candles) < 2 {
		return WarmupBars(cfg)
	}
	return WarmupBarsForDuration(cfg, candles[1].OpenTime.Sub(candles[0].OpenTime))
}

func AggregateCandles(candles []Candle, bucket time.Duration) []Candle {
	if len(candles) == 0 || bucket <= 0 {
		return nil
	}
	out := make([]Candle, 0, len(candles))
	var cur Candle
	var curBucket int64
	for i, c := range candles {
		b := c.OpenTime.UTC().UnixNano() / int64(bucket)
		if i == 0 || b != curBucket {
			if i > 0 {
				out = append(out, cur)
			}
			curBucket = b
			cur = Candle{
				OpenTime:  time.Unix(0, b*int64(bucket)).UTC(),
				CloseTime: c.CloseTime,
				Open:      c.Open,
				High:      c.High,
				Low:       c.Low,
				Close:     c.Close,
				Volume:    c.Volume,
			}
			continue
		}
		if c.High > cur.High {
			cur.High = c.High
		}
		if c.Low < cur.Low {
			cur.Low = c.Low
		}
		cur.Close = c.Close
		cur.CloseTime = c.CloseTime
		cur.Volume += c.Volume
	}
	out = append(out, cur)
	return out
}

func HigherTrendSeries(candles []Candle, bucket time.Duration, fastPeriod, slowPeriod int) []int {
	out := make([]int, len(candles))
	agg := AggregateCandles(candles, bucket)
	if len(agg) == 0 {
		return out
	}
	closes := Closes(agg)
	fast := EMA(closes, fastPeriod)
	slow := EMA(closes, slowPeriod)
	trend := make([]int, len(agg))
	warmup := slowPeriod * 3
	for i := range agg {
		if i < warmup || slow[i] == 0 || fast[i] == 0 {
			continue
		}
		slope := slow[i] - slow[i-3]
		if fast[i] > slow[i] && slope > 0 {
			trend[i] = 1
		}
		if fast[i] < slow[i] && slope < 0 {
			trend[i] = -1
		}
	}
	j := 0
	for i, c := range candles {
		for j+1 < len(agg) && !agg[j+1].CloseTime.After(c.CloseTime) {
			j++
		}
		if !agg[j].CloseTime.After(c.CloseTime) {
			out[i] = trend[j]
		}
	}
	return out
}

func LowestLow(candles []Candle, end, lookback int) float64 {
	start := end - lookback + 1
	if start < 0 {
		start = 0
	}
	low := math.Inf(1)
	for i := start; i <= end && i < len(candles); i++ {
		if candles[i].Low < low {
			low = candles[i].Low
		}
	}
	return low
}

func HighestHigh(candles []Candle, end, lookback int) float64 {
	start := end - lookback + 1
	if start < 0 {
		start = 0
	}
	high := math.Inf(-1)
	for i := start; i <= end && i < len(candles); i++ {
		if candles[i].High > high {
			high = candles[i].High
		}
	}
	return high
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
