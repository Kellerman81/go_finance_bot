package storage

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
)

// ReadLearnedLabels reads the auto-learned sector and asset-class caches from
// the bot's database at path, READ-ONLY: no schema, no migration, no write —
// an offline tool must never alter a database it was pointed at, and it may
// well be pointed at a copy of production. For cmd/backtest, whose caps
// otherwise bucket symbols by the hand-written maps alone and so ran uncapped
// where live, relying on learned classes, is capped (CM2-4).
func ReadLearnedLabels(path string) (sectors, classes map[string]string, err error) {
	// mode=ro also refuses to CREATE a missing file, which a plain open does.
	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}

	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	defer db.Close()

	s := &SQLite{db: db}

	if sectors, err = s.LoadSectors(); err != nil {
		return nil, nil, fmt.Errorf("read learned sectors: %w", err)
	}

	if classes, err = s.LoadAssetClasses(); err != nil {
		return nil, nil, fmt.Errorf("read learned asset classes: %w", err)
	}

	return sectors, classes, nil
}
