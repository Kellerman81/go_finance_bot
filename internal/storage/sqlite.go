package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// SQLite is a Store backed by a pure-Go SQLite database.
type SQLite struct {
	db        *sql.DB
	saveCount atomic.Int64 // trades saved since open; schedules pruning
}

// Trade-log pruning: realized (closed) trades — rows with win set — are kept
// forever, since /api/stats aggregates them over arbitrary periods. Everything
// else (dry-run, blocked, error, unclosed fills) is decision noise that the
// engine can re-emit every evaluation tick, so only the newest maxNoiseTrades
// rows are retained. The DELETE runs every pruneEvery inserts to amortise cost.
const (
	maxNoiseTrades = 5000
	pruneEvery     = 500
)

const schema = `
CREATE TABLE IF NOT EXISTS trades (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	ts               TEXT NOT NULL,
	symbol           TEXT NOT NULL,
	side             TEXT NOT NULL,
	qty              REAL NOT NULL,
	price            REAL NOT NULL,
	value            REAL NOT NULL,
	status           TEXT NOT NULL,
	reason           TEXT,
	order_id         TEXT,
	err              TEXT,
	pnl              REAL,
	pnl_pct          REAL,
	win              INTEGER,
	indicators       TEXT,
	entry_indicators TEXT
);
CREATE INDEX IF NOT EXISTS idx_trades_symbol ON trades(symbol);
CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS open_positions (
	symbol           TEXT PRIMARY KEY,
	qty              REAL NOT NULL,
	entry_price      REAL NOT NULL,
	entry_time       TEXT NOT NULL,
	peak             REAL NOT NULL,
	entry_indicators TEXT
);
`

// legacyColumns lists columns added to originally-shipped tables after their
// initial release. CREATE TABLE IF NOT EXISTS (used for schema above) is a
// no-op against a table that already exists, so a database file created
// before one of these columns existed needs it added explicitly.
var legacyColumns = []struct{ table, column, decl string }{
	{"trades", "pnl", "REAL"},
	{"trades", "pnl_pct", "REAL"},
	{"trades", "win", "INTEGER"},
	{"trades", "indicators", "TEXT"},
	{"trades", "entry_indicators", "TEXT"},
	{"open_positions", "entry_indicators", "TEXT"},
}

// ensureColumn adds column to table with the given SQL type/decl if it does
// not already exist. table/column/decl are always compile-time constants
// controlled by this package (see legacyColumns), never user input.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.QueryContext(context.Background(), fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)

		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}

		if name == column {
			return rows.Err()
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.ExecContext(
		context.Background(),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl),
	)

	return err
}

// OpenSQLite opens (and migrates) the SQLite database at path, creating the
// parent directory if needed (e.g. the data/ dir).
func OpenSQLite(path string) (*SQLite, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1) // serialise writes; simplest correct behaviour

	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	for _, c := range legacyColumns {
		if err := ensureColumn(db, c.table, c.column, c.decl); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate add column %s.%s: %w", c.table, c.column, err)
		}
	}

	return &SQLite{db: db}, nil
}

// Close closes the database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// nullFloat maps an optional float onto sql.NullFloat64.
func nullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}

	return sql.NullFloat64{Float64: *p, Valid: true}
}

// nullBool maps an optional bool onto sql.NullBool.
func nullBool(p *bool) sql.NullBool {
	if p == nil {
		return sql.NullBool{}
	}

	return sql.NullBool{Bool: *p, Valid: true}
}

// SaveTrade inserts a trade record, pruning old decision noise periodically.
func (s *SQLite) SaveTrade(t engine.TradeRecord) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO trades (ts, symbol, side, qty, price, value, status, reason, order_id, err,
		                      pnl, pnl_pct, win, indicators, entry_indicators)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Time.Format(time.RFC3339Nano), t.Symbol, string(t.Side), t.Qty, t.Price,
		t.Value, t.Status, t.Reason, t.OrderID, t.Err,
		nullFloat(t.PnL), nullFloat(t.PnLPct), nullBool(t.Win), t.Indicators, t.EntryIndicators,
	)
	if err == nil && s.saveCount.Add(1)%pruneEvery == 0 {
		if perr := s.pruneTrades(); perr != nil {
			return fmt.Errorf("prune trades: %w", perr)
		}
	}

	return err
}

// pruneTrades caps the unrealized decision noise at maxNoiseTrades rows,
// keeping the newest; realized trades (win IS NOT NULL) are never deleted.
func (s *SQLite) pruneTrades() error {
	_, err := s.db.ExecContext(context.Background(),
		`DELETE FROM trades WHERE win IS NULL AND id NOT IN (
		   SELECT id FROM trades WHERE win IS NULL ORDER BY id DESC LIMIT ?)`,
		maxNoiseTrades)

	return err
}

// RecentTrades returns up to limit of the most recent trade records.
func (s *SQLite) RecentTrades(limit int) ([]engine.TradeRecord, error) {
	if limit <= 0 {
		limit = 250
	}

	rows, err := s.db.QueryContext(context.Background(),
		`SELECT ts, symbol, side, qty, price, value, status, reason, order_id, err,
		        pnl, pnl_pct, win, indicators, entry_indicators
		 FROM trades ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []engine.TradeRecord

	for rows.Next() {
		var (
			t                               engine.TradeRecord
			ts, side, reason, orderID, errs string
			pnl, pnlPct                     sql.NullFloat64
			win                             sql.NullBool
			indicators, entryIndicators     sql.NullString
		)

		if err := rows.Scan(&ts, &t.Symbol, &side, &t.Qty, &t.Price, &t.Value,
			&t.Status, &reason, &orderID, &errs,
			&pnl, &pnlPct, &win, &indicators, &entryIndicators); err != nil {
			return nil, err
		}

		t.Time, _ = time.Parse(time.RFC3339Nano, ts)
		t.Side = broker.Side(side)
		t.Reason, t.OrderID, t.Err = reason, orderID, errs

		if pnl.Valid {
			v := pnl.Float64

			t.PnL = &v
		}

		if pnlPct.Valid {
			v := pnlPct.Float64

			t.PnLPct = &v
		}

		if win.Valid {
			v := win.Bool

			t.Win = &v
		}

		t.Indicators = indicators.String
		t.EntryIndicators = entryIndicators.String
		out = append(out, t)
	}

	// Return chronological (oldest first) so the engine's newest-first view is correct.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	return out, rows.Err()
}

// SaveWatchlist replaces the persisted watchlist.
func (s *SQLite) SaveWatchlist(symbols []string) error {
	b, err := json.Marshal(symbols)
	if err != nil {
		return err
	}

	return s.kvSet("watchlist", string(b))
}

// LoadWatchlist returns the persisted watchlist.
func (s *SQLite) LoadWatchlist() ([]string, error) {
	v, ok, err := s.kvGet("watchlist")
	if err != nil || !ok {
		return nil, err
	}

	var out []string

	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}

	return out, nil
}

// SaveDailySpend upserts the rolling daily-spend counter for day.
func (s *SQLite) SaveDailySpend(day string, amount float64) error {
	return s.kvSet("daily_spend", fmt.Sprintf("%s:%f", day, amount))
}

// LoadDailySpend returns the persisted day and spend amount.
func (s *SQLite) LoadDailySpend() (string, float64, error) {
	v, ok, err := s.kvGet("daily_spend")
	if err != nil || !ok {
		return "", 0, err
	}

	var (
		day    string
		amount float64
	)

	if _, err := fmt.Sscanf(v, "%10s:%f", &day, &amount); err != nil {
		return "", 0, nil //nolint:nilerr // malformed value = start fresh, not a failure
	}

	return day, amount, nil
}

// SaveOpenPosition writes (or replaces) a symbol's open-position row.
func (s *SQLite) SaveOpenPosition(p engine.OpenPosition) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO open_positions (symbol, qty, entry_price, entry_time, peak, entry_indicators)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(symbol) DO UPDATE SET
		   qty=excluded.qty, entry_price=excluded.entry_price,
		   entry_time=excluded.entry_time, peak=excluded.peak,
		   entry_indicators=excluded.entry_indicators`,
		p.Symbol,
		p.Qty,
		p.EntryPrice,
		p.EntryTime.Format(time.RFC3339Nano),
		p.Peak,
		p.EntryIndicators,
	)

	return err
}

// DeleteOpenPosition removes a symbol's open-position row.
func (s *SQLite) DeleteOpenPosition(symbol string) error {
	_, err := s.db.ExecContext(
		context.Background(),
		`DELETE FROM open_positions WHERE symbol=?`,
		symbol,
	)

	return err
}

// LoadOpenPositions returns all persisted open positions.
func (s *SQLite) LoadOpenPositions() ([]engine.OpenPosition, error) {
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT symbol, qty, entry_price, entry_time, peak, entry_indicators FROM open_positions`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []engine.OpenPosition

	for rows.Next() {
		var (
			p               engine.OpenPosition
			ts              string
			entryIndicators sql.NullString
		)

		if err := rows.Scan(
			&p.Symbol,
			&p.Qty,
			&p.EntryPrice,
			&ts,
			&p.Peak,
			&entryIndicators,
		); err != nil {
			return nil, err
		}

		p.EntryTime, _ = time.Parse(time.RFC3339Nano, ts)
		p.EntryIndicators = entryIndicators.String
		out = append(out, p)
	}

	return out, rows.Err()
}

// TradeStats aggregates realized (closed) trades — rows where win IS NOT
// NULL — since the given cutoff, overall and per symbol.
//
//nolint:cyclop // two straight-line queries plus result shaping
func (s *SQLite) TradeStats(since time.Time) (engine.TradeStats, error) {
	sinceStr := since.Format(time.RFC3339Nano)

	var (
		stats           engine.TradeStats
		winPnL, lossPnL sql.NullFloat64
	)

	err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN win=1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN win=0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(pnl), 0),
		        SUM(CASE WHEN win=1 THEN pnl END),
		        SUM(CASE WHEN win=0 THEN pnl END)
		 FROM trades WHERE win IS NOT NULL AND ts >= ?`, sinceStr,
	).Scan(&stats.Trades, &stats.Wins, &stats.Losses, &stats.TotalPnL, &winPnL, &lossPnL)
	if err != nil {
		return engine.TradeStats{}, err
	}

	if stats.Trades > 0 {
		stats.WinRatePct = 100 * float64(stats.Wins) / float64(stats.Trades)
	}

	if stats.Wins > 0 && winPnL.Valid {
		stats.AvgWin = winPnL.Float64 / float64(stats.Wins)
	}

	if stats.Losses > 0 && lossPnL.Valid {
		stats.AvgLoss = lossPnL.Float64 / float64(stats.Losses)
	}

	rows, err := s.db.QueryContext(context.Background(),
		`SELECT symbol, COUNT(*),
		        COALESCE(SUM(CASE WHEN win=1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN win=0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(pnl), 0)
		 FROM trades WHERE win IS NOT NULL AND ts >= ?
		 GROUP BY symbol ORDER BY symbol`, sinceStr)
	if err != nil {
		return engine.TradeStats{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var sym engine.SymbolStats

		if err := rows.Scan(
			&sym.Symbol,
			&sym.Trades,
			&sym.Wins,
			&sym.Losses,
			&sym.TotalPnL,
		); err != nil {
			return engine.TradeStats{}, err
		}

		if sym.Trades > 0 {
			sym.WinRatePct = 100 * float64(sym.Wins) / float64(sym.Trades)
		}

		stats.BySymbol = append(stats.BySymbol, sym)
	}

	return stats, rows.Err()
}

// kvSet upserts a key/value pair.
func (s *SQLite) kvSet(key, value string) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO kv (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// kvGet returns the value for key and whether it exists.
func (s *SQLite) kvGet(key string) (string, bool, error) {
	var v string

	err := s.db.QueryRowContext(context.Background(), `SELECT value FROM kv WHERE key=?`, key).
		Scan(&v)

	if err == sql.ErrNoRows {
		return "", false, nil
	}

	if err != nil {
		return "", false, err
	}

	return v, true, nil
}
