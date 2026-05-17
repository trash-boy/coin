package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"coin/internal/binance"
	"coin/internal/strategy"
)

type managedPosition struct {
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Quantity   float64 `json:"quantity"`
	EntryPrice float64 `json:"entry_price"`
	StopLoss   float64 `json:"stop_loss"`
}

func sideFromPosition(pos binance.Position) strategy.Side {
	if pos.PositionAmt > 0 {
		return strategy.Long
	}
	if pos.PositionAmt < 0 {
		return strategy.Short
	}
	return strategy.Flat
}

func stopOrderSide(side strategy.Side) string {
	if side == strategy.Long {
		return "SELL"
	}
	if side == strategy.Short {
		return "BUY"
	}
	return ""
}

func closeExchangePosition(ctx context.Context, c *binance.FuturesClient, symbol string, pos binance.Position, live bool, reason string) bool {
	side := sideFromPosition(pos)
	qty := absFloat(pos.PositionAmt)
	if side == strategy.Flat || qty <= 0 {
		return false
	}
	closeSide := stopOrderSide(side)
	if !live {
		fmt.Printf("[dry-run] %s WOULD EXIT %s qty=%.6f reason=%s\n", symbol, side, qty, reason)
		return true
	}
	fmt.Printf("[live] %s EXITING %s qty=%.6f via %s market reduceOnly reason=%s\n", symbol, side, qty, closeSide, reason)
	if _, err := c.PlaceMarketOrderWithID(ctx, symbol, closeSide, qty, true,
		fmt.Sprintf("coin_exit_%d", time.Now().UnixMilli())); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] close err: %v\n", symbol, err)
		return false
	}
	_ = c.CancelAllOpenOrders(ctx, symbol)
	return true
}

func replaceProtectiveStop(ctx context.Context, c *binance.FuturesClient, symbol string, side strategy.Side, qty, stopPrice float64, live bool, reason string) (float64, bool) {
	if side == strategy.Flat || qty <= 0 || stopPrice <= 0 {
		return stopPrice, false
	}
	rules, err := c.SymbolRules(ctx, symbol)
	if err == nil {
		stopPrice = rules.RoundPrice(stopPrice)
		qty = rules.RoundQuantity(qty)
	} else {
		fmt.Fprintf(os.Stderr, "warning: %s symbol rules unavailable, using raw managed stop: %v\n", strings.ToUpper(symbol), err)
	}
	stopSide := stopOrderSide(side)
	if !live {
		fmt.Printf("[dry-run] %s WOULD REPLACE STOP %s qty=%.6f stop=%.8f reason=%s\n", symbol, stopSide, qty, stopPrice, reason)
		return stopPrice, true
	}
	if err := c.CancelAllOpenOrders(ctx, symbol); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] cancel before stop replace err: %v\n", symbol, err)
		return stopPrice, false
	}
	if _, err := c.PlaceStopMarketOrder(ctx, symbol, stopSide, qty, stopPrice, true,
		fmt.Sprintf("coin_stop_%d", time.Now().UnixMilli())); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] replace stop err: %v\n", symbol, err)
		return stopPrice, false
	}
	fmt.Printf("[live] %s replaced protective stop qty=%.6f stop=%.8f reason=%s\n", symbol, qty, stopPrice, reason)
	return stopPrice, true
}

func manageOpenPosition(ctx context.Context, c *binance.FuturesClient, symbol string, pos binance.Position,
	candles []strategy.Candle, fundingRate float64, cfg strategy.Config, live bool, state *managedPosition) bool {

	side := sideFromPosition(pos)
	if side == strategy.Flat || len(candles) == 0 {
		return false
	}
	qty := absFloat(pos.PositionAmt)
	if state.Symbol == "" {
		state.Symbol = strings.ToUpper(symbol)
	}
	state.Side = string(side)
	state.Quantity = qty
	state.EntryPrice = pos.EntryPrice

	ind := strategy.BuildIndicators(candles, cfg)
	i := len(candles) - 1
	entrySide, reason := strategy.EntrySide(candles, ind, i, cfg, fundingRate)
	if entrySide != strategy.Flat && entrySide != side {
		if closeExchangePosition(ctx, c, symbol, pos, live, "reverse signal: "+reason) {
			state.StopLoss = 0
			return true
		}
		return false
	}

	currentStop := state.StopLoss
	if currentStop <= 0 {
		if stop, ok, stopReason := strategy.StructureStop(candles, ind, i, side, cfg); ok {
			rounded, replaced := replaceProtectiveStop(ctx, c, symbol, side, qty, stop, live, "missing local stop; "+stopReason)
			if replaced {
				state.StopLoss = rounded
			}
			return replaced
		}
		return false
	}

	nextStop, tighten := strategy.ManagedTrailingStop(side, pos.EntryPrice, currentStop, candles[i].Close, ind.ATR[i], cfg)
	if !tighten {
		return false
	}
	rounded, replaced := replaceProtectiveStop(ctx, c, symbol, side, qty, nextStop, live, "managed trailing stop")
	if replaced {
		state.StopLoss = rounded
	}
	return replaced
}
