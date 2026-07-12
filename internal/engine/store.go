package engine

import "time"

// OpenPosition is the bot's durable record of a position it opened and has not
// yet fully sold, including the trailing-stop high-water mark. It is persisted
// immediately on each buy/sell so a restart knows exactly what is still open.
type OpenPosition struct {
	Symbol          string    `json:"symbol"`
	Qty             float64   `json:"qty"`
	EntryPrice      float64   `json:"entry_price"`
	EntryTime       time.Time `json:"entry_time"`
	Peak            float64   `json:"peak"`                       // high-water mark since entry (for trailing stops)
	EntryIndicators string    `json:"entry_indicators,omitempty"` // detector snapshot captured at the position's first open
}

// Store is the persistence contract the engine depends on. Implementations live
// in the storage package; the engine defines the interface here so it does not
// import storage (avoiding an import cycle).
type Store interface {
	SaveTrade(t TradeRecord) error
	RecentTrades(limit int) ([]TradeRecord, error)
	TradeStats(since time.Time) (TradeStats, error)
	SaveWatchlist(symbols []string) error
	LoadWatchlist() ([]string, error)
	SaveDailySpend(day string, amount float64) error
	LoadDailySpend() (day string, amount float64, err error)
	// Open positions — written immediately on buy/sell so they survive restarts.
	SaveOpenPosition(p OpenPosition) error
	DeleteOpenPosition(symbol string) error
	LoadOpenPositions() ([]OpenPosition, error)
	Close() error
}

// nopStore is the default no-persistence implementation.
type nopStore struct{}

func (nopStore) SaveTrade(TradeRecord) error                 { return nil }
func (nopStore) RecentTrades(int) ([]TradeRecord, error)     { return nil, nil }
func (nopStore) TradeStats(time.Time) (TradeStats, error)    { return TradeStats{}, nil }
func (nopStore) SaveWatchlist([]string) error                { return nil }
func (nopStore) LoadWatchlist() ([]string, error)            { return nil, nil }
func (nopStore) SaveDailySpend(string, float64) error        { return nil }
func (nopStore) LoadDailySpend() (string, float64, error)    { return "", 0, nil }
func (nopStore) SaveOpenPosition(OpenPosition) error         { return nil }
func (nopStore) DeleteOpenPosition(string) error             { return nil }
func (nopStore) LoadOpenPositions() ([]OpenPosition, error)  { return nil, nil }
func (nopStore) Close() error                                { return nil }
