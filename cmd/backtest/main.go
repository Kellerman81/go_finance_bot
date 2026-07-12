// Command backtest replays historical candles through the bot's strategy, risk
// limits and protective exits, then prints a performance report. Data comes
// from a CSV file (-csv) or is synthesised for quick experiments (-generate).
//
//	go run ./cmd/backtest -generate AAPL,MSFT -bars 500
//	go run ./cmd/backtest -csv data/aapl.csv
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

func main() {
	cfgPath := flag.String("config", "data/config.yaml", "config file for strategy/limits/exits")
	csvPath := flag.String("csv", "", "CSV file of OHLCV candles")
	gen := flag.String("generate", "", "comma-separated symbols to synthesise instead of CSV")
	bars := flag.Int("bars", 500, "number of synthetic bars when using -generate")
	cash := flag.Float64("cash", 100000, "starting cash")
	orderSize := flag.Float64("order-size", 0, "per-buy notional (defaults to engine.order_size_usd)")
	save := flag.String("save", "", "save the loaded/generated candles to this CSV for reuse with -csv")
	jsonOut := flag.Bool("json", false, "emit full result (incl. trades) as JSON")
	flag.Parse()

	cfg := backtest.LoadConfig(*cfgPath, os.Stderr)

	var err error
	var data map[string][]market.Candle
	switch {
	case *csvPath != "":
		data, err = backtest.LoadCSV(*csvPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load csv:", err)
			os.Exit(1)
		}
	case *gen != "":
		syms := splitSymbols(*gen)
		data = backtest.Generate(syms, *bars)
	default:
		fmt.Fprintln(os.Stderr, "provide -csv <file> or -generate <symbols>")
		flag.Usage()
		os.Exit(2)
	}
	if len(data) == 0 {
		fmt.Fprintln(os.Stderr, "no candle data loaded")
		os.Exit(1)
	}

	if *save != "" {
		if err := backtest.SaveCSV(*save, data); err != nil {
			fmt.Fprintln(os.Stderr, "save csv:", err)
			os.Exit(1)
		}
		var rows int
		for _, c := range data {
			rows += len(c)
		}
		fmt.Fprintf(os.Stderr, "saved %d symbols / %d bars to %s (reuse with -csv %s)\n",
			len(data), rows, *save, *save)
	}

	size := *orderSize
	if size <= 0 {
		size = cfg.Engine.OrderSizeUSD
	}

	bt := &backtest.Backtester{
		Strategy:  strategy.New(cfg.Strategy),
		Limits:    cfg.Limits,
		Exits:     cfg.Exits,
		Costs:           cfg.Costs,
		Cash:            *cash,
		OrderSize:       size,
		AllowPyramiding: cfg.Engine.AllowPyramiding,
		ConfirmBuy:      cfg.Engine.ConfirmBuy,
		ConfirmSell:     cfg.Engine.ConfirmSell,
		KeepInterval:    cfg.Engine.KeepInterval,
	}
	res := bt.Run(data)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}
	printReport(res, data)
}

func printReport(r backtest.Result, data map[string][]market.Candle) {
	syms := make([]string, 0, len(data))
	for s := range data {
		syms = append(syms, s)
	}
	fmt.Println("================ BACKTEST REPORT ================")
	fmt.Printf("Symbols          : %s\n", strings.Join(syms, ", "))
	fmt.Printf("Bars             : %d\n", r.Bars)
	fmt.Printf("Starting cash    : %12.2f\n", r.StartingCash)
	fmt.Printf("Final equity     : %12.2f\n", r.FinalEquity)
	fmt.Printf("Total return     : %11.2f%%\n", r.TotalReturnPct)
	fmt.Printf("Buy & hold       : %11.2f%%  (equal-weighted benchmark)\n", r.BuyHoldReturnPct)
	fmt.Printf("Max drawdown     : %11.2f%%\n", r.MaxDrawdownPct)
	fmt.Printf("Sharpe (scaled)  : %12.2f\n", r.Sharpe)
	fmt.Printf("Trades           : %12d  (%d wins / %d losses)\n", r.NumTrades, r.Wins, r.Losses)
	fmt.Printf("Win rate         : %11.2f%%\n", r.WinRatePct)
	fmt.Println("================================================")
	if r.NumTrades > 0 {
		fmt.Println("Last trades:")
		start := 0
		if len(r.Trades) > 10 {
			start = len(r.Trades) - 10
		}
		for _, t := range r.Trades[start:] {
			fmt.Printf("  %s  %-4s %-6s qty %.4f @ %.2f  (%s)\n",
				t.Time.Format("2006-01-02 15:04"), t.Side, t.Symbol, t.Qty, t.Price, t.Reason)
		}
	}
}

func splitSymbols(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToUpper(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
