package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// CM3-2: BOT_WATCHLIST was upper-cased but the file's watchlist was not, so
// `watchlist: [aapl]` reached the engine as a second symbol beside the
// screener's AAPL — pinned under the wrong key, with its own position record
// and pending-sell guard: two decision loops over one holding.
func TestTheFileWatchlistIsNormalised(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("watchlist: [aapl, \" sap.de \", AAPL]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"AAPL", "SAP.DE"}; !slices.Equal(cfg.Watchlist, want) {
		t.Errorf("watchlist = %q, want %q", cfg.Watchlist, want)
	}
}
