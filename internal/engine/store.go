package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/protect"
)

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

	// OrigQty is the quantity the position reached before any scale-out. The
	// ladder's rungs are fractions of THIS, not of what is left — otherwise
	// three 0.33 rungs would sell 70% of the position rather than all of it.
	OrigQty float64 `json:"orig_qty,omitempty"`
	// PartialsTaken is how many scale-out rungs have already fired, so each
	// fires once per position and survives a restart.
	PartialsTaken int `json:"partials_taken,omitempty"`
	// Adds is how many scale-in steps have been added to this position, capped
	// by engine.scale_in.max_adds.
	Adds int `json:"adds,omitempty"`
	// EntryCommission is the modelled commission charged to OPEN the position
	// so far, accumulated across every buy/scale-in that built it. closePnL
	// apportions it by the fraction of the position being sold, so a realized
	// P&L is net of the round trip's real cost — the same basis
	// internal/backtest books (see RoundTrip.PnL, "net of round-trip
	// commission"). Without it the live track record was gross while every
	// backtested candidate was net, and internal/autotune compared the two
	// directly. See CODE_REVIEW_FINDINGS.md's EN-1 finding.
	EntryCommission float64 `json:"entry_commission,omitempty"`
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
	// SavePinned replaces the persisted set of hand-picked symbols.
	SavePinned(symbols []string) error
	// LoadPinned returns the persisted set of hand-picked symbols.
	LoadPinned() ([]string, error)
	// SaveNames replaces the persisted symbol -> company name cache.
	SaveNames(names map[string]string) error
	// LoadNames returns the persisted symbol -> company name cache.
	LoadNames() (map[string]string, error)
	// SaveSectors replaces the persisted symbol -> auto-learned sector
	// cache (config.yaml's own sectors: block is a separate, always-wins
	// override layer — this is only what was learned automatically).
	SaveSectors(sectors map[string]string) error
	// LoadSectors returns the persisted auto-learned sector cache.
	LoadSectors() (map[string]string, error)
	// SaveAssetClasses replaces the persisted symbol -> auto-learned asset
	// class cache (config.yaml's own asset_classes: block is a separate,
	// always-wins override layer — this is only what the data provider or
	// the broker classified automatically).
	SaveAssetClasses(classes map[string]string) error
	// LoadAssetClasses returns the persisted auto-learned asset class cache.
	LoadAssetClasses() (map[string]string, error)
	// SaveAutotuneApplies replaces the persisted map of symbol -> the time
	// the scheduled or manual auto-optimizer (internal/autotune) last
	// applied a strategy_overrides change to it — so a future pass can
	// judge how that change has performed in REAL trading since, not just
	// how the winning candidate backtested at proposal time.
	SaveAutotuneApplies(applies map[string]time.Time) error
	// LoadAutotuneApplies returns the persisted map.
	LoadAutotuneApplies() (map[string]time.Time, error)
	// SaveLossStreaks replaces the persisted map of symbol -> how many
	// realized closes in a row it has lost, which drives
	// autotune.on_loss. Persisted so a restart does not forgive a streak
	// in progress.
	SaveLossStreaks(streaks map[string]int) error
	// LoadLossStreaks returns the persisted consecutive-loss counters.
	LoadLossStreaks() (map[string]int, error)
	// SaveDailySpend upserts the rolling daily-spend counter for day.
	SaveDailySpend(day string, amount float64) error
	// LoadDailySpend returns the persisted day and spend amount.
	LoadDailySpend() (day string, amount float64, err error)
	// SaveProtectionState upserts the protections' halt/peak/day-equity state.
	SaveProtectionState(s protect.State) error
	// LoadProtectionState returns the persisted protections state. ok is
	// false when nothing has ever been saved.
	LoadProtectionState() (s protect.State, ok bool, err error)
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

// SavePinned is a no-op.
func (nopStore) SavePinned([]string) error { return nil }

// LoadPinned is a no-op.
func (nopStore) LoadPinned() ([]string, error) { return nil, nil }

// SaveNames is a no-op.
func (nopStore) SaveNames(map[string]string) error { return nil }

// LoadNames is a no-op.
func (nopStore) LoadNames() (map[string]string, error) { return nil, nil }

// SaveSectors is a no-op.
func (nopStore) SaveSectors(map[string]string) error { return nil }

// LoadSectors is a no-op.
func (nopStore) LoadSectors() (map[string]string, error) { return nil, nil }

func (nopStore) SaveAssetClasses(map[string]string) error { return nil }

func (nopStore) LoadAssetClasses() (map[string]string, error) { return nil, nil }

func (nopStore) SaveLossStreaks(map[string]int) error { return nil }

func (nopStore) LoadLossStreaks() (map[string]int, error) { return nil, nil }

// SaveAutotuneApplies is a no-op.
func (nopStore) SaveAutotuneApplies(map[string]time.Time) error { return nil }

// LoadAutotuneApplies is a no-op.
func (nopStore) LoadAutotuneApplies() (map[string]time.Time, error) { return nil, nil }

// SaveDailySpend is a no-op.
func (nopStore) SaveDailySpend(string, float64) error { return nil }

// LoadDailySpend is a no-op.
func (nopStore) LoadDailySpend() (string, float64, error) { return "", 0, nil }

// SaveProtectionState is a no-op.
func (nopStore) SaveProtectionState(protect.State) error { return nil }

// LoadProtectionState is a no-op.
func (nopStore) LoadProtectionState() (protect.State, bool, error) {
	return protect.State{}, false, nil
}

// SaveOpenPosition is a no-op.
func (nopStore) SaveOpenPosition(OpenPosition) error { return nil }

// DeleteOpenPosition is a no-op.
func (nopStore) DeleteOpenPosition(string) error { return nil }

// LoadOpenPositions is a no-op.
func (nopStore) LoadOpenPositions() ([]OpenPosition, error) { return nil, nil }

// Close is a no-op.
func (nopStore) Close() error { return nil }
