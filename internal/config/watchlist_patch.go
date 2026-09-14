package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Commenting a symbol out of the watchlist: block.
//
// An automated drop that only changed the running watchlist would not survive a
// restart: cmd/bot re-adds every symbol in watchlist: on start, so the bot would
// give up on an instrument and then take it straight back. The file has to
// change too.
//
// Commented, never deleted. A deletion is a decision the operator cannot see,
// review or reverse — the symbol simply stops existing and nothing says why.
// A commented line keeps the symbol, the reason and the date in the file, and
// un-dropping it is removing one "#".
//
// Line-based rather than an AST splice, unlike Patch. The whole point is to
// produce a COMMENT, and comments are not nodes: any structural rewrite would
// either drop them or have to reinvent them. Working on lines also means an
// entry this cannot confidently identify is left strictly alone.

// ErrWatchlistFlowStyle reports a watchlist written inline (watchlist: [A, B]).
// A flow sequence has no per-symbol line to comment out, and rewriting it into
// block style to make room would reformat a file the operator wrote by hand.
var ErrWatchlistFlowStyle = errors.New(
	"config: watchlist is written inline ([A, B]); " +
		"put one symbol per line to allow automatic drops")

// CommentOutWatchlistSymbol comments symbol's entry out of file's watchlist:
// block, appending reason so the file records why.
//
// It reports whether an active entry was found and changed. false with a nil
// error is the ordinary "nothing to do" case — a screener discovery was never
// in the file, and a symbol dropped earlier is already commented — so callers
// treat it as success, not as a failure to retry.
func CommentOutWatchlistSymbol(file, symbol, reason string) (bool, error) {
	patchMu.Lock()
	defer patchMu.Unlock()

	src, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}

	out, changed, err := commentOutWatchlistBytes(src, symbol, reason)
	if err != nil || !changed {
		return false, err
	}

	if err := writeAtomic(file, out); err != nil {
		return false, err
	}

	return true, nil
}

// commentOutWatchlistBytes is CommentOutWatchlistSymbol's pure half, so the
// line-walking can be tested without touching a filesystem.
func commentOutWatchlistBytes(src []byte, symbol, reason string) ([]byte, bool, error) {
	want := normalizeWatchlistEntry(symbol)
	if want == "" {
		return src, false, nil
	}

	// Preserve the file's own line ending: rewriting CRLF as LF would show up
	// as a whole-file diff for a one-line change.
	nl := "\n"
	if strings.Contains(string(src), "\r\n") {
		nl = "\r\n"
	}

	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")

	start, err := watchlistBlockStart(lines)
	if err != nil || start < 0 {
		return src, false, err
	}

	for i := start; i < len(lines); i++ {
		item, ok := watchlistItem(lines[i])
		if !ok {
			if endsWatchlistBlock(lines[i]) {
				break
			}

			continue // a blank line or a comment inside the block
		}

		if normalizeWatchlistEntry(item) != want {
			continue
		}

		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
		lines[i] = fmt.Sprintf("%s# - %s   # %s", indent, item, reason)

		return []byte(strings.Join(lines, nl)), true, nil
	}

	return src, false, nil
}

// watchlistBlockStart returns the index of the first line after a top-level
// "watchlist:" key, or -1 when the file has no watchlist block.
func watchlistBlockStart(lines []string) (int, error) {
	for i, line := range lines {
		// Top level only: an indented "watchlist:" belongs to some other
		// block, and a commented one is not the live list.
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
			strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "watchlist:")
		if !ok {
			continue
		}

		if strings.TrimSpace(rest) != "" {
			// watchlist: [A, B] — nothing per-symbol to comment.
			return -1, ErrWatchlistFlowStyle
		}

		return i + 1, nil
	}

	return -1, nil
}

// watchlistItem extracts an ACTIVE list entry's value from a line, stripping
// quotes and any trailing inline comment. ok is false for anything that is not
// a live "- SYMBOL" item, which includes an already-commented one.
func watchlistItem(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)

	rest, ok := strings.CutPrefix(trimmed, "- ")
	if !ok {
		// "-SYMBOL" with no space is not valid YAML for a scalar item, and
		// "#- SYMBOL" is already commented; neither is an active entry.
		return "", false
	}

	// Strip an inline comment, then quotes.
	if idx := strings.Index(rest, "#"); idx >= 0 {
		rest = rest[:idx]
	}

	rest = strings.TrimSpace(rest)
	rest = strings.Trim(rest, `"'`)

	if rest == "" {
		return "", false
	}

	return rest, true
}

// endsWatchlistBlock reports whether a line ends the watchlist sequence: any
// content that is neither a list item nor a comment nor blank.
func endsWatchlistBlock(line string) bool {
	trimmed := strings.TrimSpace(line)

	return trimmed != "" && !strings.HasPrefix(trimmed, "#")
}

// normalizeWatchlistEntry canonicalises a symbol for comparison the same way
// the loader does — upper-cased and trimmed — so "aapl" in the file matches
// "AAPL" from the engine.
func normalizeWatchlistEntry(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
