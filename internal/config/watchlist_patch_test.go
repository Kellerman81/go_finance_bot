package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleWatchlist = `# leading comment
watchlist:
  - 2HP.F
  - AAPL
  # - OLDONE   # dropped earlier
  - SAP.DE
  - "QUOTED.DE"
  - MRVL   # an inline note

limits:
  max_order_value: 300
`

func TestCommentOutWatchlistSymbol(t *testing.T) {
	t.Parallel()

	out, changed, err := commentOutWatchlistBytes([]byte(sampleWatchlist), "AAPL", "8 losses")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want a successful edit", changed, err)
	}

	got := string(out)
	if !strings.Contains(got, "  # - AAPL   # 8 losses") {
		t.Errorf("AAPL was not commented with its reason:\n%s", got)
	}

	// Everything else must be untouched, byte for byte.
	for _, keep := range []string{
		"# leading comment", "  - 2HP.F", "  - SAP.DE", `  - "QUOTED.DE"`,
		"  # - OLDONE   # dropped earlier", "  max_order_value: 300",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("edit disturbed %q:\n%s", keep, got)
		}
	}

	// And the list is one entry shorter once parsed.
	if strings.Count(got, "\n  - ") != strings.Count(sampleWatchlist, "\n  - ")-1 {
		t.Errorf("wrong number of active entries left:\n%s", got)
	}
}

// The loader upper-cases symbols, so a file spelled differently must still
// match — otherwise a drop silently does nothing and the symbol returns on the
// next restart.
func TestCommentOutMatchesCaseInsensitively(t *testing.T) {
	t.Parallel()

	src := "watchlist:\n  - aapl\n"

	out, changed, err := commentOutWatchlistBytes([]byte(src), "AAPL", "why")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	if !strings.Contains(string(out), "# - aapl") {
		t.Errorf("did not comment the differently-cased entry: %q", out)
	}
}

// Quotes and inline comments must not defeat the match, and the entry is
// written back as it was found rather than being reformatted.
func TestCommentOutHandlesQuotesAndInlineComments(t *testing.T) {
	t.Parallel()

	for name, src := range map[string]string{
		"quoted": "watchlist:\n  - \"MRVL\"\n",
		"inline": "watchlist:\n  - MRVL   # a note\n",
		"tabbed": "watchlist:\n\t- MRVL\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, changed, err := commentOutWatchlistBytes([]byte(src), "MRVL", "why")
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v for %q", changed, err, src)
			}

			if !strings.Contains(string(out), "# - MRVL") {
				t.Errorf("unexpected result for %q: %q", src, out)
			}
		})
	}
}

// Nothing to do is not an error: a screener discovery was never in the file,
// and a symbol dropped earlier is already commented.
func TestCommentOutIsANoOpWhenNotFound(t *testing.T) {
	t.Parallel()

	for name, symbol := range map[string]string{
		"absent":           "NOSUCH",
		"already dropped":  "OLDONE",
		"empty symbol":     "",
		"outside the list": "max_order_value",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, changed, err := commentOutWatchlistBytes([]byte(sampleWatchlist), symbol, "why")
			if err != nil {
				t.Fatalf("err=%v", err)
			}

			if changed {
				t.Errorf("reported a change for %q", symbol)
			}

			if string(out) != sampleWatchlist {
				t.Error("the file was modified anyway")
			}
		})
	}
}

// A symbol named in another block must never be mistaken for a watchlist entry.
func TestCommentOutStopsAtTheEndOfTheBlock(t *testing.T) {
	t.Parallel()

	src := "watchlist:\n  - AAPL\n\nscreener:\n  universe:\n    - MRVL\n"

	out, changed, err := commentOutWatchlistBytes([]byte(src), "MRVL", "why")
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	if changed || strings.Contains(string(out), "# - MRVL") {
		t.Errorf("commented an entry belonging to another block:\n%s", out)
	}
}

// An inline list has no per-symbol line to comment, and rewriting it into block
// style would reformat a file the operator wrote by hand. Say so rather than
// guessing.
func TestCommentOutRefusesAFlowSequence(t *testing.T) {
	t.Parallel()

	_, changed, err := commentOutWatchlistBytes([]byte("watchlist: [AAPL, MRVL]\n"), "AAPL", "why")
	if !errors.Is(err, ErrWatchlistFlowStyle) {
		t.Fatalf("err=%v, want ErrWatchlistFlowStyle", err)
	}

	if changed {
		t.Error("reported a change for a flow sequence")
	}
}

// CRLF files must not come back as a whole-file diff for a one-line change.
func TestCommentOutPreservesLineEndings(t *testing.T) {
	t.Parallel()

	src := "watchlist:\r\n  - AAPL\r\n  - MRVL\r\n"

	out, changed, err := commentOutWatchlistBytes([]byte(src), "AAPL", "why")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	if strings.Contains(strings.ReplaceAll(string(out), "\r\n", ""), "\n") {
		t.Errorf("line endings were not preserved: %q", out)
	}
}

// The result must still parse, and still parse as a watchlist one symbol
// shorter — the whole point is a config the bot can read on its next start.
func TestCommentedConfigStillLoads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	src, err := os.ReadFile("../../data/config.example.yaml")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}

	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	before, err := Load(path)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}

	if len(before.Watchlist) == 0 {
		t.Skip("the example config has no watchlist to drop from")
	}

	victim := before.Watchlist[0]

	changed, err := CommentOutWatchlistSymbol(path, victim, "8 consecutive losses")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	after, err := Load(path)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}

	if len(after.Watchlist) != len(before.Watchlist)-1 {
		t.Fatalf("watchlist went from %d to %d, want one fewer",
			len(before.Watchlist), len(after.Watchlist))
	}

	for _, sym := range after.Watchlist {
		if strings.EqualFold(sym, victim) {
			t.Fatalf("%s is still active in the watchlist", victim)
		}
	}

	// The symbol and the reason stay in the file so the drop is reversible.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), victim) ||
		!strings.Contains(string(raw), "8 consecutive losses") {
		t.Error("the commented entry lost the symbol or its reason")
	}
}
