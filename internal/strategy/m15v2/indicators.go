package m15v2

import "math"

// Indicators maintains streaming indicator state for the 15m timeframe.
// All updates are O(1) per bar (after initial warmup).
type Indicators struct {
	p Params

	// rolling buffers (capped at max needed window)
	closes []float64
	highs  []float64
	lows   []float64

	// EMA state
	emaFast, emaMid, emaSlow float64
	emaPullback              float64
	emaInit                  bool

	// ATR (Wilder)
	atr     float64
	prevC   float64
	atrInit bool

	// ADX (Wilder)
	adx, plusDI, minusDI       float64
	smPlusDM, smMinusDM, smTR  float64
	adxInit                    bool

	// Bollinger
	bbMid, bbUp, bbLow, bbWidth float64

	// rolling BB-width buffer for squeeze percentile
	widthBuf []float64

	// Donchian
	donHi, donLo       float64 // entry channel
	donTrailHi, donTrailLo float64

	bars int
}

func NewIndicators(p Params) *Indicators {
	cap := p.WarmupBars() + 10
	return &Indicators{
		p:        p,
		closes:   make([]float64, 0, cap),
		highs:    make([]float64, 0, cap),
		lows:     make([]float64, 0, cap),
		widthBuf: make([]float64, 0, p.SqueezeLB+10),
	}
}

func (ind *Indicators) Bars() int { return ind.bars }

// Ready reports whether enough bars have been seen to use indicator values.
func (ind *Indicators) Ready() bool {
	return ind.bars >= ind.p.WarmupBars()
}

// Update ingests one new closed bar.
func (ind *Indicators) Update(b Bar) {
	ind.bars++

	// keep buffers bounded
	maxLen := ind.p.WarmupBars() + 5
	ind.closes = appendCap(ind.closes, b.Close, maxLen)
	ind.highs = appendCap(ind.highs, b.High, maxLen)
	ind.lows = appendCap(ind.lows, b.Low, maxLen)

	ind.updateEMAs(b.Close)
	ind.updateATR(b)
	ind.updateADX(b)
	ind.updateBB()
	ind.updateDonchian()
}

func appendCap(s []float64, v float64, max int) []float64 {
	s = append(s, v)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}

func (ind *Indicators) updateEMAs(c float64) {
	if !ind.emaInit {
		ind.emaFast = c
		ind.emaMid = c
		ind.emaSlow = c
		ind.emaPullback = c
		ind.emaInit = true
		return
	}
	a := func(n int) float64 { return 2.0 / (float64(n) + 1.0) }
	ind.emaFast = a(ind.p.EMAFast)*c + (1-a(ind.p.EMAFast))*ind.emaFast
	ind.emaMid = a(ind.p.EMAMid)*c + (1-a(ind.p.EMAMid))*ind.emaMid
	ind.emaSlow = a(ind.p.EMASlow)*c + (1-a(ind.p.EMASlow))*ind.emaSlow
	ind.emaPullback = a(ind.p.PullbackEMA)*c + (1-a(ind.p.PullbackEMA))*ind.emaPullback
}

func (ind *Indicators) updateATR(b Bar) {
	if !ind.atrInit {
		ind.atr = b.High - b.Low
		ind.prevC = b.Close
		ind.atrInit = true
		return
	}
	tr := math.Max(b.High-b.Low,
		math.Max(math.Abs(b.High-ind.prevC), math.Abs(b.Low-ind.prevC)))
	n := float64(ind.p.ATRPeriod)
	ind.atr = (ind.atr*(n-1) + tr) / n
	ind.prevC = b.Close
}

func (ind *Indicators) updateADX(b Bar) {
	n := len(ind.highs)
	if n < 2 {
		return
	}
	upMove := b.High - ind.highs[n-2]
	downMove := ind.lows[n-2] - b.Low
	plusDM := 0.0
	minusDM := 0.0
	if upMove > downMove && upMove > 0 {
		plusDM = upMove
	}
	if downMove > upMove && downMove > 0 {
		minusDM = downMove
	}
	tr := math.Max(b.High-b.Low,
		math.Max(math.Abs(b.High-ind.prevC), math.Abs(b.Low-ind.prevC)))

	period := float64(ind.p.ADXPeriod)
	if !ind.adxInit {
		ind.smPlusDM = plusDM
		ind.smMinusDM = minusDM
		ind.smTR = tr
		ind.adxInit = true
		return
	}
	ind.smPlusDM = ind.smPlusDM - ind.smPlusDM/period + plusDM
	ind.smMinusDM = ind.smMinusDM - ind.smMinusDM/period + minusDM
	ind.smTR = ind.smTR - ind.smTR/period + tr

	if ind.smTR == 0 {
		return
	}
	ind.plusDI = 100 * ind.smPlusDM / ind.smTR
	ind.minusDI = 100 * ind.smMinusDM / ind.smTR
	denom := ind.plusDI + ind.minusDI
	if denom == 0 {
		return
	}
	dx := 100 * math.Abs(ind.plusDI-ind.minusDI) / denom
	if ind.adx == 0 {
		ind.adx = dx
	} else {
		ind.adx = (ind.adx*(period-1) + dx) / period
	}
}

func (ind *Indicators) updateBB() {
	n := ind.p.BBPeriod
	if len(ind.closes) < n {
		return
	}
	window := ind.closes[len(ind.closes)-n:]
	mean := 0.0
	for _, v := range window {
		mean += v
	}
	mean /= float64(n)
	v := 0.0
	for _, x := range window {
		d := x - mean
		v += d * d
	}
	std := math.Sqrt(v / float64(n))
	ind.bbMid = mean
	ind.bbUp = mean + ind.p.BBStdMult*std
	ind.bbLow = mean - ind.p.BBStdMult*std
	ind.bbWidth = ind.bbUp - ind.bbLow
	ind.widthBuf = appendCap(ind.widthBuf, ind.bbWidth, ind.p.SqueezeLB)
}

func (ind *Indicators) updateDonchian() {
	hN := ind.p.DonchianEntry
	if len(ind.highs) >= hN {
		hi := ind.highs[len(ind.highs)-hN]
		lo := ind.lows[len(ind.lows)-hN]
		for i := len(ind.highs) - hN; i < len(ind.highs); i++ {
			if ind.highs[i] > hi {
				hi = ind.highs[i]
			}
			if ind.lows[i] < lo {
				lo = ind.lows[i]
			}
		}
		ind.donHi, ind.donLo = hi, lo
	}
	tN := ind.p.DonchianTrail
	if len(ind.highs) >= tN {
		hi := ind.highs[len(ind.highs)-tN]
		lo := ind.lows[len(ind.lows)-tN]
		for i := len(ind.highs) - tN; i < len(ind.highs); i++ {
			if ind.highs[i] > hi {
				hi = ind.highs[i]
			}
			if ind.lows[i] < lo {
				lo = ind.lows[i]
			}
		}
		ind.donTrailHi, ind.donTrailLo = hi, lo
	}
}

// --- accessors used by strategy/filter ---

func (ind *Indicators) EMAFast() float64    { return ind.emaFast }
func (ind *Indicators) EMAMid() float64     { return ind.emaMid }
func (ind *Indicators) EMASlow() float64    { return ind.emaSlow }
func (ind *Indicators) EMAPullback() float64 { return ind.emaPullback }
func (ind *Indicators) ATR() float64        { return ind.atr }
func (ind *Indicators) ADX() float64        { return ind.adx }
func (ind *Indicators) BBWidth() float64    { return ind.bbWidth }
func (ind *Indicators) DonHi() float64      { return ind.donHi }
func (ind *Indicators) DonLo() float64      { return ind.donLo }
func (ind *Indicators) DonTrailHi() float64 { return ind.donTrailHi }
func (ind *Indicators) DonTrailLo() float64 { return ind.donTrailLo }

// SqueezePercentile returns the rank (0..1) of current BB width within the lookback buffer.
// Lower percentile = tighter squeeze.
func (ind *Indicators) SqueezePercentile() float64 {
	if len(ind.widthBuf) < ind.p.BBPeriod {
		return 1.0
	}
	cur := ind.bbWidth
	cnt := 0
	for _, w := range ind.widthBuf {
		if w <= cur {
			cnt++
		}
	}
	return float64(cnt) / float64(len(ind.widthBuf))
}
