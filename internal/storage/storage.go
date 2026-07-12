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
	SaveTrade(t engine.TradeRecord) error
	RecentTrades(limit int) ([]engine.TradeRecord, error)
	TradeStats(since time.Time) (engine.TradeStats, error)
	SaveWatchlist(symbols []string) error
	LoadWatchlist() ([]string, error)
	SaveDailySpend(day string, amount float64) error
	LoadDailySpend() (day string, amount float64, err error)
	SaveOpenPosition(p engine.OpenPosition) error
	DeleteOpenPosition(symbol string) error
	LoadOpenPositions() ([]engine.OpenPosition, error)
	Close() error
}

// Nop is a no-op store used when persistence is disabled.
type Nop struct{}

func (Nop) SaveTrade(engine.TradeRecord) error                { return nil }
func (Nop) RecentTrades(int) ([]engine.TradeRecord, error)    { return nil, nil }
func (Nop) TradeStats(time.Time) (engine.TradeStats, error)   { return engine.TradeStats{}, nil }
func (Nop) SaveWatchlist([]string) error                      { return nil }
func (Nop) LoadWatchlist() ([]string, error)                  { return nil, nil }
func (Nop) SaveDailySpend(string, float64) error               { return nil }
func (Nop) LoadDailySpend() (string, float64, error)          { return "", 0, nil }
func (Nop) SaveOpenPosition(engine.OpenPosition) error         { return nil }
func (Nop) DeleteOpenPosition(string) error                    { return nil }
func (Nop) LoadOpenPositions() ([]engine.OpenPosition, error)  { return nil, nil }
func (Nop) Close() error                                       { return nil }
