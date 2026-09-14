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
	"github.com/Kellerman81/go_finance_bot/internal/protect"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// tsLayout is a fixed-width RFC3339 variant used to write the trades table's
// "ts" column — always 9 fractional digits, unlike time.RFC3339Nano
// (`.999999999`), which trims trailing zeros. TradeStats compares this
// column with a plain SQL string inequality (`ts >= ?`), not a datetime
// function, so its ordering has to match chronological order byte-for-byte.
// RFC3339Nano's trimming breaks that: "10:00:00.5Z" (a half-second past the
// hour) sorts BEFORE "10:00:00Z" (exactly on the hour) as a string, because
// '.' (0x2E) sorts before 'Z' (0x5A) — the exact opposite of chronological
// order — which can silently drop a row right at a query's cutoff boundary.
// Reading is unaffected: time.Parse(time.RFC3339Nano, ...) accepts either
// width. See CODE_REVIEW_FINDINGS.md's "TradeStats compares RFC3339Nano
// timestamps as strings".
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// dbTimeout bounds every database call. SetMaxOpenConns(1) serializes all
// access through one connection, and every call here used bare
// context.Background() — no bound at all on how long a caller would wait
// for that connection, or on how long a query itself could run once it had
// it. A slow, unindexed query (TradeStats' full scan over the trades table
// with no index on ts) could hold the one connection for its entire
// duration, during which the trading loop's own SaveTrade call — needed to
// book a fill — blocked for exactly as long, with nothing to make it give
// up. See CODE_REVIEW_FINDINGS.md's "No bound on connection hold time".
var dbTimeout = 10 * time.Second //nolint:gochecknoglobals // test-overridable; see sqlite_timeout_test.go

// dbCtx returns a context bounded by dbTimeout. The returned cancel must be
// called (typically via defer) once the guarded call returns.
func dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}

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
	entry_indicators TEXT,
	strategy_config  TEXT,
	simulated        INTEGER
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
	entry_indicators TEXT,
	orig_qty         REAL,
	partials_taken   INTEGER,
	adds             INTEGER,
	entry_commission REAL
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
	{"trades", "strategy_config", "TEXT"},
	{"trades", "simulated", "INTEGER"},
	{"open_positions", "entry_indicators", "TEXT"},
	{"open_positions", "orig_qty", "REAL"},
	{"open_positions", "partials_taken", "INTEGER"},
	{"open_positions", "adds", "INTEGER"},
	{"open_positions", "entry_commission", "REAL"},
}

// ensureColumn adds column to table with the given SQL type/decl if it does
// not already exist. table/column/decl are always compile-time constants
// controlled by this package (see legacyColumns), never user input.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	ctx, cancel := dbCtx()
	defer cancel()

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
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
		ctx,
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

	schemaCtx, schemaCancel := dbCtx()
	_, err = db.ExecContext(schemaCtx, schema)
	schemaCancel()

	if err != nil {
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
	ctx, cancel := dbCtx()
	defer cancel()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO trades (ts, symbol, side, qty, price, value, status, reason, order_id, err,
		                      pnl, pnl_pct, win, indicators, entry_indicators, strategy_config, simulated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Time.Format(tsLayout), t.Symbol, string(t.Side), t.Qty, t.Price,
		t.Value, t.Status, t.Reason, t.OrderID, t.Err,
		nullFloat(t.PnL), nullFloat(t.PnLPct), nullBool(t.Win), t.Indicators, t.EntryIndicators,
		t.StrategyConfig, t.Simulated,
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
	ctx, cancel := dbCtx()
	defer cancel()

	_, err := s.db.ExecContext(ctx,
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

	ctx, cancel := dbCtx()
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, symbol, side, qty, price, value, status, reason, order_id, err,
		        pnl, pnl_pct, win, indicators, entry_indicators, strategy_config, simulated
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
			strategyConfig                  sql.NullString
			simulated                       sql.NullBool
		)

		if err := rows.Scan(&ts, &t.Symbol, &side, &t.Qty, &t.Price, &t.Value,
			&t.Status, &reason, &orderID, &errs,
			&pnl, &pnlPct, &win, &indicators, &entryIndicators, &strategyConfig, &simulated); err != nil {
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
		t.StrategyConfig = strategyConfig.String
		t.Simulated = simulated.Valid && simulated.Bool
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

// SavePinned replaces the persisted set of hand-picked symbols — the ones the
// screener may not prune when screener.keep_manual is set.
func (s *SQLite) SavePinned(symbols []string) error {
	b, err := json.Marshal(symbols)
	if err != nil {
		return err
	}

	return s.kvSet("watchlist_pinned", string(b))
}

// LoadPinned returns the persisted set of hand-picked symbols.
//
// An installation upgrading to keep_manual has no such key, and returning empty
// is the deliberate answer: the config watchlist is re-pinned at startup, and
// anything else in the persisted watchlist is as likely to be a screener
// discovery as an operator's pick. Guessing "pinned" would freeze a whole
// screened universe in place the first time keep_manual was switched on.
func (s *SQLite) LoadPinned() ([]string, error) {
	v, ok, err := s.kvGet("watchlist_pinned")
	if err != nil || !ok {
		return nil, err
	}

	var out []string

	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}

	return out, nil
}

// SaveNames replaces the persisted symbol -> company name cache. Names are
// effectively immutable, so this exists to avoid re-fetching every one of them
// from the search endpoint on each start.
func (s *SQLite) SaveNames(names map[string]string) error {
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}

	return s.kvSet("symbol_names", string(b))
}

// LoadNames returns the persisted symbol -> company name cache.
func (s *SQLite) LoadNames() (map[string]string, error) {
	v, ok, err := s.kvGet("symbol_names")
	if err != nil || !ok {
		return nil, err
	}

	var out map[string]string

	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}

	return out, nil
}

// SaveSectors replaces the persisted symbol -> auto-learned sector cache.
func (s *SQLite) SaveSectors(sectors map[string]string) error {
	b, err := json.Marshal(sectors)
	if err != nil {
		return err
	}

	return s.kvSet("auto_sectors", string(b))
}

// LoadSectors returns the persisted auto-learned sector cache.
func (s *SQLite) LoadSectors() (map[string]string, error) {
	v, ok, err := s.kvGet("auto_sectors")
	if err != nil || !ok {
		return nil, err
	}

	var out map[string]string

	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}

	return out, nil
}

// SaveAssetClasses replaces the persisted symbol -> auto-learned asset class
// cache.
func (s *SQLite) SaveAssetClasses(classes map[string]string) error {
	b, err := json.Marshal(classes)
	if err != nil {
		return err
	}

	return s.kvSet("auto_asset_classes", string(b))
}

// LoadAssetClasses returns the persisted auto-learned asset class cache.
func (s *SQLite) LoadAssetClasses() (map[string]string, error) {
	v, ok, err := s.kvGet("auto_asset_classes")
	if err != nil || !ok {
		return nil, err
	}

	var out map[string]string

	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}

	return out, nil
}

// SaveLossStreaks replaces the persisted consecutive-loss counters.
func (s *SQLite) SaveLossStreaks(streaks map[string]int) error {
	b, err := json.Marshal(streaks)
	if err != nil {
		return err
	}

	return s.kvSet("loss_streaks", string(b))
}

// LoadLossStreaks returns the persisted consecutive-loss counters.
func (s *SQLite) LoadLossStreaks() (map[string]int, error) {
	v, ok, err := s.kvGet("loss_streaks")
	if err != nil || !ok {
		return nil, err
	}

	var out map[string]int

	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}

	return out, nil
}

// SaveAutotuneApplies replaces the persisted map of symbol -> the time the
// scheduled or manual auto-optimizer last applied a strategy_overrides
// change to it.
func (s *SQLite) SaveAutotuneApplies(applies map[string]time.Time) error {
	b, err := json.Marshal(applies)
	if err != nil {
		return err
	}

	return s.kvSet("autotune_applies", string(b))
}

// LoadAutotuneApplies returns the persisted map.
func (s *SQLite) LoadAutotuneApplies() (map[string]time.Time, error) {
	v, ok, err := s.kvGet("autotune_applies")
	if err != nil || !ok {
		return nil, err
	}

	var out map[string]time.Time

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

// SaveProtectionState upserts the protections' halt/peak/day-equity state, so
// an active halt (and the drawdown peak it was measured against) survives a
// restart instead of silently clearing — see protect.State's doc comment.
func (s *SQLite) SaveProtectionState(st protect.State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}

	return s.kvSet("protection_state", string(b))
}

// LoadProtectionState returns the persisted protections state.
func (s *SQLite) LoadProtectionState() (protect.State, bool, error) {
	v, ok, err := s.kvGet("protection_state")
	if err != nil || !ok {
		return protect.State{}, false, err
	}

	var out protect.State

	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return protect.State{}, false, err
	}

	return out, true, nil
}

// SaveOpenPosition writes (or replaces) a symbol's open-position row.
func (s *SQLite) SaveOpenPosition(p engine.OpenPosition) error {
	ctx, cancel := dbCtx()
	defer cancel()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO open_positions (
		   symbol, qty, entry_price, entry_time, peak, entry_indicators,
		   orig_qty, partials_taken, adds, entry_commission)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(symbol) DO UPDATE SET
		   qty=excluded.qty, entry_price=excluded.entry_price,
		   entry_time=excluded.entry_time, peak=excluded.peak,
		   entry_indicators=excluded.entry_indicators,
		   orig_qty=excluded.orig_qty, partials_taken=excluded.partials_taken,
		   adds=excluded.adds, entry_commission=excluded.entry_commission`,
		p.Symbol,
		p.Qty,
		p.EntryPrice,
		p.EntryTime.Format(time.RFC3339Nano),
		p.Peak,
		p.EntryIndicators,
		p.OrigQty,
		p.PartialsTaken,
		p.Adds,
		p.EntryCommission,
	)

	return err
}

// DeleteOpenPosition removes a symbol's open-position row.
func (s *SQLite) DeleteOpenPosition(symbol string) error {
	ctx, cancel := dbCtx()
	defer cancel()

	_, err := s.db.ExecContext(
		ctx,
		`DELETE FROM open_positions WHERE symbol=?`,
		symbol,
	)

	return err
}

// LoadOpenPositions returns all persisted open positions.
func (s *SQLite) LoadOpenPositions() ([]engine.OpenPosition, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT symbol, qty, entry_price, entry_time, peak, entry_indicators,
		        orig_qty, partials_taken, adds, entry_commission
		 FROM open_positions`,
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
			origQty         sql.NullFloat64
			partials, adds  sql.NullInt64
			entryCommission sql.NullFloat64
		)

		if err := rows.Scan(
			&p.Symbol,
			&p.Qty,
			&p.EntryPrice,
			&ts,
			&p.Peak,
			&entryIndicators,
			&origQty,
			&partials,
			&adds,
			&entryCommission,
		); err != nil {
			return nil, err
		}

		p.EntryTime, _ = time.Parse(time.RFC3339Nano, ts)
		p.EntryIndicators = entryIndicators.String
		// Rows written before the scale-out columns existed have no original
		// quantity; the current quantity is the best available stand-in and is
		// exactly right for a position that has never been scaled out.
		p.OrigQty = origQty.Float64
		if p.OrigQty <= 0 {
			p.OrigQty = p.Qty
		}

		p.PartialsTaken = int(partials.Int64)
		p.Adds = int(adds.Int64)
		// NULL for a row written before this column existed: such a position
		// simply books its close gross, exactly as it did before, rather than
		// inventing a commission nobody recorded.
		p.EntryCommission = entryCommission.Float64
		out = append(out, p)
	}

	return out, rows.Err()
}

// TradeStats aggregates realized (closed) trades — rows where win IS NOT
// NULL — since the given cutoff, overall and per symbol.
//
//nolint:cyclop // two straight-line queries plus result shaping
func (s *SQLite) TradeStats(since time.Time) (engine.TradeStats, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	sinceStr := since.Format(tsLayout)

	var (
		stats           engine.TradeStats
		winPnL, lossPnL sql.NullFloat64
	)

	err := s.db.QueryRowContext(ctx,
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

	rows, err := s.db.QueryContext(ctx,
		`SELECT symbol, COUNT(*),
		        COALESCE(SUM(CASE WHEN win=1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN win=0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(pnl), 0),
		        COALESCE(AVG(pnl_pct), 0)
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
			&sym.ExpectancyPct,
		); err != nil {
			return engine.TradeStats{}, err
		}

		if sym.Trades > 0 {
			sym.WinRatePct = 100 * float64(sym.Wins) / float64(sym.Trades)
		}

		stats.BySymbol = append(stats.BySymbol, sym)
	}

	if err := rows.Err(); err != nil {
		return engine.TradeStats{}, err
	}

	breakdown, err := s.exitBreakdownBySymbol(ctx, sinceStr)
	if err != nil {
		return engine.TradeStats{}, err
	}

	for i := range stats.BySymbol {
		stats.BySymbol[i].ExitBreakdown = breakdown[stats.BySymbol[i].Symbol]
	}

	return stats, nil
}

// exitBreakdownBySymbol counts each symbol's realized closes by
// engine.ClassifyExitKind, for TradeStats.BySymbol's ExitBreakdown. A
// separate query rather than folding into the aggregate one above: the
// classification is a Go-side string match on the free-text reason column,
// not something a portable SQL CASE expression should try to duplicate (and
// re-duplicate every time the reason-string convention changes).
func (s *SQLite) exitBreakdownBySymbol(ctx context.Context, sinceStr string) (map[string]map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT symbol, reason FROM trades WHERE win IS NOT NULL AND ts >= ?`, sinceStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]map[string]int)

	for rows.Next() {
		var symbol, reason string

		if err := rows.Scan(&symbol, &reason); err != nil {
			return nil, err
		}

		if out[symbol] == nil {
			out[symbol] = make(map[string]int)
		}

		out[symbol][engine.ClassifyExitKind(reason)]++
	}

	return out, rows.Err()
}

// kvSet upserts a key/value pair.
func (s *SQLite) kvSet(key, value string) error {
	ctx, cancel := dbCtx()
	defer cancel()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// kvGet returns the value for key and whether it exists.
func (s *SQLite) kvGet(key string) (string, bool, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	var v string

	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, key).
		Scan(&v)

	if err == sql.ErrNoRows {
		return "", false, nil
	}

	if err != nil {
		return "", false, err
	}

	return v, true, nil
}
