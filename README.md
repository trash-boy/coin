# coin

Go CLI for a Binance USD-M futures leveraged trend strategy.

The current strategy is a v2 leveraged trend follower:

- filters for trend regime with ADX, efficiency ratio, 4h direction, and 1d direction;
- enters only after a pullback inside a confirmed trend;
- uses swing high/low structure stops and trailing exits, with no fixed take-profit by default;
- supports controlled pyramiding after favorable ATR movement;
- applies asymmetric long/short risk, because BTC short drift is usually worse;
- sizes by structure-stop risk, volatility-targeted leverage, drawdown tiers, and recent PnL scaling;
- caps gross exposure by configured leverage and margin-use limit;
- executes signals on the next candle open to avoid look-ahead bias;
- includes taker fee, slippage, gap stops, and funding-rate cost in backtests when available;
- stops for the rest of the day after daily loss and halts after max drawdown.

It defaults to signal generation and backtesting. It does not place live orders from the CLI.

The CLI also supports:

- equal-weight multi-symbol backtests and signal scans;
- funding-rate carry scans for spot/perp or margin/perp hedges;
- spot-vs-USD-M futures basis scans using best bid/ask quotes.

Arbitrage commands are scanners only. They do not solve borrow availability, transfer latency, order-book depth beyond the top level, liquidation of the hedge leg, tax, or VIP-tier fee differences.

## Run

```bash
go test ./...
go run ./cmd/coin backtest -symbol BTCUSDT -interval 1h -days 2500 -equity 10000 -leverage 2
go run ./cmd/coin signal -symbol ETHUSDT -interval 1h -equity 10000 -leverage 2
go run ./cmd/coin portfolio-backtest -symbols BTCUSDT,ETHUSDT,SOLUSDT -interval 1h -days 2500 -equity 10000
go run ./cmd/coin portfolio-signal -symbols BTCUSDT,ETHUSDT,SOLUSDT -interval 1h -equity 10000
go run ./cmd/coin funding-arb -symbols BTCUSDT,ETHUSDT,SOLUSDT -min-funding-bps 1
go run ./cmd/coin basis-arb -symbols BTCUSDT,ETHUSDT,SOLUSDT -cost-bps 8 -min-edge-bps 3
```

You can move strategy parameters into a flat YAML or JSON config and still override values with flags:

```yaml
risk_per_trade: 0.006
max_leverage: 2
break_even_r: 1.5
trail_activation_r: 2.0
funding_block_long: 0.00015
funding_block_short: 0.00012
```

```bash
go run ./cmd/coin backtest -config strategy.yaml -symbol BTCUSDT
```

Signed Binance futures methods read credentials from:

```bash
export BINANCE_API_KEY=...
export BINANCE_API_SECRET=...
```

Use testnet first:

```bash
go run ./cmd/coin signal -env testnet
```
