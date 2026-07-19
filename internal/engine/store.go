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
	// SaveTrade persists one trade decision.
	SaveTrade(t TradeRecord) error
	// RecentTrades returns up to limit of the most recent trade records.
	RecentTrades(limit int) ([]TradeRecord, error)
	// TradeStats aggregates realized win/loss outcomes since the cutoff.
	TradeStats(since time.Time) (TradeStats, error)
	// SaveWatchlist replaces the persisted watchlist.
	SaveWatchlist(symbols []string) error
	// LoadWatchlist returns the persisted watchlist.
	LoadWatchlist() ([]string, error)
	// SaveDailySpend upserts the rolling daily-spend counter for day.
	SaveDailySpend(day string, amount float64) error
	// LoadDailySpend returns the persisted day and spend amount.
	LoadDailySpend() (day string, amount float64, err error)
	// SaveOpenPosition writes a durable open-position row — immediately on
	// buy/sell so positions survive restarts.
	SaveOpenPosition(p OpenPosition) error
	// DeleteOpenPosition removes a symbol's open-position row.
	DeleteOpenPosition(symbol string) error
	// LoadOpenPositions returns all persisted open positions.
	LoadOpenPositions() ([]OpenPosition, error)
	// Close releases the underlying resources.
	Close() error
}

// nopStore is the default no-persistence implementation.
type nopStore struct{}

// SaveTrade is a no-op.
func (nopStore) SaveTrade(TradeRecord) error { return nil }

// RecentTrades is a no-op.
func (nopStore) RecentTrades(int) ([]TradeRecord, error) { return nil, nil }

// TradeStats is a no-op.
func (nopStore) TradeStats(time.Time) (TradeStats, error) { return TradeStats{}, nil }

// SaveWatchlist is a no-op.
func (nopStore) SaveWatchlist([]string) error { return nil }

// LoadWatchlist is a no-op.
func (nopStore) LoadWatchlist() ([]string, error) { return nil, nil }

// SaveDailySpend is a no-op.
func (nopStore) SaveDailySpend(string, float64) error { return nil }

// LoadDailySpend is a no-op.
func (nopStore) LoadDailySpend() (string, float64, error) { return "", 0, nil }

// SaveOpenPosition is a no-op.
func (nopStore) SaveOpenPosition(OpenPosition) error { return nil }

// DeleteOpenPosition is a no-op.
func (nopStore) DeleteOpenPosition(string) error { return nil }

// LoadOpenPositions is a no-op.
func (nopStore) LoadOpenPositions() ([]OpenPosition, error) { return nil, nil }

// Close is a no-op.
func (nopStore) Close() error { return nil }
