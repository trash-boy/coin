// Package m15v2 implements the upgraded 15m aggressive trend-breakout strategy.
//
// Core upgrades vs internal/strategy/aggressive_15m:
//   1) Bollinger-band squeeze filter (only trade after compression)
//   2) Pullback limit-order entry on EMA-fast (better fills, higher win-rate)
//   3) Donchian reverse-channel trailing stop (replaces ATR chandelier)
//   4) Two-stage scale-out: TP1 at 1R (1/2), TP2 at 2R (3/10)
//   5) Hard time-based exit (MaxBarsHold) to avoid weekend / dead-tape decay
//
// This package is framework-free so it can be wired into any execution layer
// (live trading or backtest).
package m15v2

import "time"

// Bar is a 15m kline. Time is the open time of the bar.
type Bar struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// Side mirrors strategy.Side but kept local to avoid an import cycle.
type Side int8

const (
	SideNone  Side = 0
	SideLong  Side = 1
	SideShort Side = -1
)

func (s Side) Sign() float64 { return float64(s) }

// Action enumerates the things the engine can ask the executor to do.
type Action int8

const (
	ActNone Action = iota
	ActOpenLimit
	ActCancelEntry
	ActClosePartial
	ActCloseAll
	ActAdjustStop
)

// Signal is the only thing the strategy emits.
type Signal struct {
	Time     time.Time
	Action   Action
	Side     Side
	Price    float64 // limit price, fill price, or new stop (depending on Action)
	Stop     float64 // initial stop or adjusted stop
	SizeQty  float64 // quantity in base asset
	Portion  float64 // 0..1, only valid for ActClosePartial
	Reason   string
	LimitTTL int // for ActOpenLimit: cancel after this many bars if unfilled
}

// Position is the strategy's view of an open position.
type Position struct {
	Side      Side
	Entry     float64
	Stop      float64    // current stop (may be trailed)
	InitStop  float64    // initial stop, used for R calculations
	Qty       float64    // remaining qty in base asset
	OpenTime  time.Time
	HighWater float64
	LowWater  float64
	TP1Done   bool
	TP2Done   bool
	BarsHeld  int
}

// PendingEntry tracks an unfilled limit entry order (pullback entry).
type PendingEntry struct {
	Side       Side
	LimitPrice float64
	Stop       float64
	SizeQty    float64
	BarsLeft   int
	PlacedAt   time.Time
}

// TradeStat is one closed trade, used by the rolling half-Kelly window.
type TradeStat struct {
	Win      bool
	PnL      float64
	ClosedAt time.Time
}
