// Package storage persists trades and bot state. The SQLite implementation is
// pure-Go (modernc.org/sqlite, no cgo) so the database file can be inspected
// with any SQLite tool. A Nop implementation provides an in-memory-only mode.
package storage

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/engine"
)

// Store is the persistence contract. It matches engine.Store so the engine can
// use any implementation without importing this package.
type Store interface {
	// SaveTrade persists one trade decision.
	SaveTrade(t engine.TradeRecord) error
	// RecentTrades returns up to limit of the most recent trade records.
	RecentTrades(limit int) ([]engine.TradeRecord, error)
	// TradeStats aggregates realized win/loss outcomes since the cutoff.
	TradeStats(since time.Time) (engine.TradeStats, error)
	// SaveWatchlist replaces the persisted watchlist.
	SaveWatchlist(symbols []string) error
	// LoadWatchlist returns the persisted watchlist.
	LoadWatchlist() ([]string, error)
	// SavePinned replaces the persisted set of hand-picked symbols.
	SavePinned(symbols []string) error
	// LoadPinned returns the persisted set of hand-picked symbols.
	LoadPinned() ([]string, error)
	// SaveNames replaces the persisted symbol -> company name cache.
	SaveNames(names map[string]string) error
	// LoadNames returns the persisted symbol -> company name cache.
	LoadNames() (map[string]string, error)
	// SaveSectors replaces the persisted symbol -> auto-learned sector cache
	// (config.yaml's own sectors: block is a separate, always-wins override
	// layer — this is only what was learned automatically).
	SaveSectors(sectors map[string]string) error
	// LoadSectors returns the persisted auto-learned sector cache.
	LoadSectors() (map[string]string, error)
	// SaveAssetClasses replaces the persisted symbol -> auto-learned asset
	// class cache (config.yaml's own asset_classes: block is a separate,
	// always-wins override layer).
	SaveAssetClasses(classes map[string]string) error
	// LoadAssetClasses returns the persisted auto-learned asset class cache.
	LoadAssetClasses() (map[string]string, error)
	// SaveLossStreaks replaces the persisted symbol -> consecutive realized
	// losses counters that drive autotune.on_loss.
	SaveLossStreaks(streaks map[string]int) error
	// LoadLossStreaks returns the persisted consecutive-loss counters.
	LoadLossStreaks() (map[string]int, error)
	// SaveAutotuneApplies replaces the persisted map of symbol -> the time
	// the scheduled or manual auto-optimizer last applied a
	// strategy_overrides change to it.
	SaveAutotuneApplies(applies map[string]time.Time) error
	// LoadAutotuneApplies returns the persisted map.
	LoadAutotuneApplies() (map[string]time.Time, error)
	// SaveDailySpend upserts the rolling daily-spend counter for day.
	SaveDailySpend(day string, amount float64) error
	// LoadDailySpend returns the persisted day and spend amount.
	LoadDailySpend() (day string, amount float64, err error)
	// SaveOpenPosition writes a durable open-position row.
	SaveOpenPosition(p engine.OpenPosition) error
	// DeleteOpenPosition removes a symbol's open-position row.
	DeleteOpenPosition(symbol string) error
	// LoadOpenPositions returns all persisted open positions.
	LoadOpenPositions() ([]engine.OpenPosition, error)
	// Close releases the underlying resources.
	Close() error
}

// Nop is a no-op store used when persistence is disabled.
type Nop struct{}

// SaveTrade is a no-op.
func (Nop) SaveTrade(engine.TradeRecord) error { return nil }

// RecentTrades is a no-op.
func (Nop) RecentTrades(int) ([]engine.TradeRecord, error) { return nil, nil }

// TradeStats is a no-op.
func (Nop) TradeStats(time.Time) (engine.TradeStats, error) { return engine.TradeStats{}, nil }

// SaveWatchlist is a no-op.
func (Nop) SaveWatchlist([]string) error { return nil }

// LoadWatchlist is a no-op.
func (Nop) LoadWatchlist() ([]string, error) { return nil, nil }

// SavePinned is a no-op.
func (Nop) SavePinned([]string) error { return nil }

// LoadPinned is a no-op.
func (Nop) LoadPinned() ([]string, error) { return nil, nil }

// SaveNames is a no-op.
func (Nop) SaveNames(map[string]string) error { return nil }

// LoadNames is a no-op.
func (Nop) LoadNames() (map[string]string, error) { return nil, nil }

// SaveSectors is a no-op.
func (Nop) SaveSectors(map[string]string) error { return nil }

// LoadSectors is a no-op.
func (Nop) LoadSectors() (map[string]string, error) { return nil, nil }

// SaveAssetClasses is a no-op.
func (Nop) SaveAssetClasses(map[string]string) error { return nil }

// LoadAssetClasses is a no-op.
func (Nop) LoadAssetClasses() (map[string]string, error) { return nil, nil }

// SaveLossStreaks is a no-op.
func (Nop) SaveLossStreaks(map[string]int) error { return nil }

// LoadLossStreaks is a no-op.
func (Nop) LoadLossStreaks() (map[string]int, error) { return nil, nil }

// SaveAutotuneApplies is a no-op.
func (Nop) SaveAutotuneApplies(map[string]time.Time) error { return nil }

// LoadAutotuneApplies is a no-op.
func (Nop) LoadAutotuneApplies() (map[string]time.Time, error) { return nil, nil }

// SaveDailySpend is a no-op.
func (Nop) SaveDailySpend(string, float64) error { return nil }

// LoadDailySpend is a no-op.
func (Nop) LoadDailySpend() (string, float64, error) { return "", 0, nil }

// SaveOpenPosition is a no-op.
func (Nop) SaveOpenPosition(engine.OpenPosition) error { return nil }

// DeleteOpenPosition is a no-op.
func (Nop) DeleteOpenPosition(string) error { return nil }

// LoadOpenPositions is a no-op.
func (Nop) LoadOpenPositions() ([]engine.OpenPosition, error) { return nil, nil }

// Close is a no-op.
func (Nop) Close() error { return nil }
