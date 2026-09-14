package config

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// Writing settings back to config.yaml without destroying it.
//
// Two approaches were tried and rejected, in this order.
//
// Unmarshal, mutate, marshal. Measured against a real 679-line config it
// produced 304 lines with ZERO of the 342 comment lines, and silently dropped
// the whole strategy_overrides block (SymbolStrategy is `yaml:"-"`, derived at
// load time, so a marshal cannot emit it).
//
// Parse to an AST, edit nodes, print the tree. That keeps the comments, but
// goccy's printer is not a faithful round-tripper: re-printing the same file
// with no edits at all turned 680 lines into 1119, inserting a blank line
// between every element. The file would have doubled on every save.
//
// So the AST is used only to LOCATE. Each changed scalar's byte span is computed
// from its token position and spliced into the original source; every byte
// outside those spans is carried through untouched — comments, spacing, key
// order, blank lines, commented-out blocks, anchors and all.

// patchMu serializes the read-modify-write cycle in Patch.
//
// Without it, two concurrent Patch calls (e.g. two settings PUTs from
// different browser tabs, or the v2 UI's per-section forms saved together)
// both read the same original bytes, each computes its own patched version
// against that stale snapshot, and whichever writeAtomic's rename wins
// silently discards the other's change — both calls return nil, so neither
// caller learns anything was lost. It resurfaces the next time something
// reads the file (a restart), reverting a change the operator believed had
// already saved. A single process-wide mutex is enough: config writes are
// rare and never on a hot path, so serializing them costs nothing that
// matters, and it removes the race regardless of which file(s) are
// involved. See CODE_REVIEW_FINDINGS.md CRIT-10.
var patchMu sync.Mutex

// Patch writes value into the YAML document at file under the top-level key
// section, changing only the scalars value describes.
//
// value is the section's own config struct, so keys and formatting come from its
// yaml tags rather than a hand-maintained mapping that could drift. Durations
// render as "6h0m0s", which the loader reads back.
//
// The write is atomic — temp file then rename — because this file holds live
// broker credentials and a torn write would be unrecoverable. The whole
// read-modify-write is additionally serialized (see patchMu) so a concurrent
// second call can't silently discard this one's change.
func Patch(file, section string, value any) error {
	patchMu.Lock()
	defer patchMu.Unlock()

	src, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	out, err := patchBytes(src, section, value)
	if err != nil {
		return err
	}

	return writeAtomic(file, out)
}

// SymbolOverrideFields is the subset of StrategyConfig a caller has decided
// to set for one symbol's strategy_overrides entry — e.g. from a search's
// proposal. Every field is optional (a pointer, or the slice's own nilness)
// so PatchSymbolOverride writes only the fields actually present, exactly
// as a hand-written entry only ever names the fields someone decided on and
// inherits the rest (see PerSymbolStrategy/resolveSymbolStrategies).
// MinStrength/MinStrengthSell/TrendGate and every indicator-parameter field
// below are pointers specifically because their zero values (0, false) are
// legitimate values to SET, not just the absence of one — a plain omitempty
// bool/float/int would be unable to tell "set trend_gate to false" from
// "trend_gate wasn't decided at all".
//
// DetectorsSell/CombineSell are ALSO pointers (*[]string, *string), unlike
// Detectors/Combine — one asymmetry deliberately kept. A non-nil pointer to
// an empty slice/string is a real, distinct value here: "explicitly revert
// to inheriting the buy side" (see internal/strategy.New: an empty
// DetectorsSell/CombineSell means "same as buy"), which a search's Refine
// pass can and does choose. A plain []string/string with `omitempty` cannot
// express that — a nil slice or "" is indistinguishable from "field not
// set" and is dropped, silently keeping whatever sell-side override the
// symbol already had instead of clearing it. Detectors/Combine don't need
// this: the search always names a real buy-side detector set and combine
// mode, never an empty/inherited one, so there is no equivalent "explicit
// zero" to preserve for them. See CODE_REVIEW_FINDINGS.md's AT-4 finding.
//
// Detectors/DetectorsSell are tagged ",flow" so they render as
// `detectors: [ema_cross, rsi, macd]` on one line, matching the style every
// hand-written strategy_overrides entry already uses — goccy's default for
// a bare []string (struct field or map value alike) is one item per line
// spanning several lines, which the single-line leaf-replacement/insertion
// machinery in this file cannot place correctly.
//
// FastMA through DonchianPeriod mirror every indicator-parameter field a
// search (internal/backtest's guided search / Refine) can vary. Earlier,
// internal/autotune deliberately did not surface these — reasoning that
// since the search resamples all of them on every candidate regardless of
// which detectors are active, most of them differing from base was
// "sampling noise". That reasoning missed that relevant or not, they are
// still inputs to the very Result a proposal is judged and displayed by:
// dropping them meant the config actually written to strategy_overrides did
// not reproduce the config that was backtested and gated. See
// CODE_REVIEW_FINDINGS.md's AT-2 finding.
type SymbolOverrideFields struct {
	Detectors       []string  `yaml:"detectors,omitempty,flow"`
	Combine         string    `yaml:"combine,omitempty"`
	MinStrength     *float64  `yaml:"min_strength,omitempty"`
	DetectorsSell   *[]string `yaml:"detectors_sell,omitempty,flow"`
	CombineSell     *string   `yaml:"combine_sell,omitempty"`
	MinStrengthSell *float64  `yaml:"min_strength_sell,omitempty"`
	TrendGate       *bool     `yaml:"trend_gate,omitempty"`

	// Moving averages / RSI / MACD.
	FastMA        *int     `yaml:"fast_ma,omitempty"`
	SlowMA        *int     `yaml:"slow_ma,omitempty"`
	RSIPeriod     *int     `yaml:"rsi_period,omitempty"`
	RSIOverbought *float64 `yaml:"rsi_overbought,omitempty"`
	RSIOversold   *float64 `yaml:"rsi_oversold,omitempty"`
	MACDFast      *int     `yaml:"macd_fast,omitempty"`
	MACDSlow      *int     `yaml:"macd_slow,omitempty"`
	MACDSignal    *int     `yaml:"macd_signal,omitempty"`

	// Bollinger Bands.
	BBPeriod *int     `yaml:"bb_period,omitempty"`
	BBStdDev *float64 `yaml:"bb_stddev,omitempty"`
	// VWAP.
	VWAPPeriod *int `yaml:"vwap_period,omitempty"`
	// ATR breakout.
	ATRPeriod *int     `yaml:"atr_period,omitempty"`
	ATRMult   *float64 `yaml:"atr_mult,omitempty"`
	// Relative volume.
	RVOLPeriod    *int     `yaml:"rvol_period,omitempty"`
	RVOLThreshold *float64 `yaml:"rvol_threshold,omitempty"`
	// Volume profile.
	VPWindow  *int `yaml:"vp_window,omitempty"`
	VPBuckets *int `yaml:"vp_buckets,omitempty"`

	// ADX.
	ADXPeriod    *int     `yaml:"adx_period,omitempty"`
	ADXThreshold *float64 `yaml:"adx_threshold,omitempty"`
	// Stochastic oscillator.
	StochKPeriod    *int     `yaml:"stoch_k_period,omitempty"`
	StochDPeriod    *int     `yaml:"stoch_d_period,omitempty"`
	StochOverbought *float64 `yaml:"stoch_overbought,omitempty"`
	StochOversold   *float64 `yaml:"stoch_oversold,omitempty"`
	// Supertrend.
	SupertrendPeriod *int     `yaml:"supertrend_period,omitempty"`
	SupertrendMult   *float64 `yaml:"supertrend_mult,omitempty"`
	// Donchian channel.
	DonchianPeriod *int `yaml:"donchian_period,omitempty"`
}

// quotePathSegment wraps s in single quotes so it can be safely used as one
// segment of a goccy/go-yaml path string (e.g. "$.strategy_overrides." +
// quotePathSegment(symbol)) — the path language treats an unquoted "." as a
// nesting separator, not a literal character, so an exchange-suffixed
// ticker like "SAP.DE" or "BRK.A" resolves as two levels ($.strategy_
// overrides.SAP.DE) instead of the single mapping key it actually is,
// silently failing to find (or bootstrap, or verify) that symbol's own
// block — the exact shape of a live failure: "config has no
// strategy_overrides.SAP.DE: block to write detectors into" for every
// exchange-suffixed symbol on the watchlist, none of which are actually
// missing a block at all. Quoting is applied unconditionally, not just
// when a "." is present: a plain identifier like "AMZN" resolves
// identically quoted or not, so there is no "does this symbol need it"
// branch to get wrong for a ticker shape nobody anticipated.
func quotePathSegment(s string) string {
	return "'" + s + "'"
}

// PatchSymbolOverride writes fields into symbol's strategy_overrides entry in
// file, creating the strategy_overrides: top-level block and/or symbol's own
// sub-block first if either is missing yet — the one case Patch itself
// deliberately refuses (planParent's "config has no %s: block" error; see
// TestPatchRefusesAMissingSection's doc comment on why that refusal exists
// for the general case, and PatchSymbolOverride's own tests for why a
// per-symbol override earns the exception a settings form's static sections
// don't need: a hand-written config may simply never have added a symbol's
// first override yet, and there is no settings form to have added a
// strategy_overrides: heading by hand in advance).
func PatchSymbolOverride(file, symbol string, fields SymbolOverrideFields) error {
	patchMu.Lock()
	defer patchMu.Unlock()

	src, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	symbol = existingOverrideKey(src, symbol)

	src, err = ensureSymbolOverrideBlock(src, symbol)
	if err != nil {
		return err
	}

	out, err := patchBytes(src, "strategy_overrides."+quotePathSegment(symbol), fields)
	if err != nil {
		return err
	}

	return writeAtomic(file, out)
}

// existingOverrideKey returns the key PatchSymbolOverride should write under:
// the strategy_overrides key already in src that names the same symbol
// case-insensitively, or symbol normalized to the canonical form
// (upper-cased, trimmed) when there is none.
//
// resolveSymbolStrategies upper-cases every key as it builds its map, so
// "aapl:" and "AAPL:" are the SAME symbol to the loader but two distinct
// keys in the file. Writing the caller's raw spelling (autotune passes the
// raw watchlist symbol) against a file that already spells it differently
// produced two blocks for one symbol, and load then resolved whichever the
// map iteration happened to visit last — measured at 171/29 across 200 loads
// of one file, so which config a symbol actually traded under changed from
// one restart to the next. See CODE_REVIEW_FINDINGS.md's EN-2 finding, and
// resolveSymbolStrategies, which now refuses such a file outright.
func existingOverrideKey(src []byte, symbol string) string {
	want := strings.ToUpper(strings.TrimSpace(symbol))

	var doc struct {
		Overrides map[string]map[string]any `yaml:"strategy_overrides"`
	}

	if err := yaml.Unmarshal(src, &doc); err == nil {
		for key := range doc.Overrides {
			if strings.ToUpper(strings.TrimSpace(key)) == want {
				return key
			}
		}
	}

	return want
}

// ensureSymbolOverrideBlock returns src with a top-level strategy_overrides:
// key and symbol's own sub-key both present — as bare, valueless (YAML
// null) keys if either was missing — so the ordinary field-splice path
// (patchBytes, via applyRound/insertEdits/emptyBlockEnd) has something to
// write fields into. That path already knows how to populate a freshly bare
// key's first fields (the exact "engine: with no scale_in:" case
// patchRounds' doc comment describes) — the only genuinely new step here is
// creating the bare key(s) in the first place, which planParent explicitly
// refuses to do (by design, for every other section).
//
// The inserted placeholder is always a bare key with nothing after the
// colon, never a flow-style empty mapping ("symbol: {}") — a flow mapping
// is a complete, self-closed value on its own line; block-style children
// cannot be appended beneath it (that's a YAML syntax error, not nesting).
// A bare key reads as null, and is exactly the idiom planParent already
// uses elsewhere in this file for a fresh intermediate block.
func ensureSymbolOverrideBlock(src []byte, symbol string) ([]byte, error) {
	doc, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	top, err := yaml.PathString("$.strategy_overrides")
	if err != nil {
		return nil, fmt.Errorf("strategy_overrides path: %w", err)
	}

	topNode, _ := top.FilterFile(doc)
	if topNode == nil {
		return append(append([]byte{}, src...), "\nstrategy_overrides:\n  "+symbol+":\n"...), nil
	}

	symPath, err := yaml.PathString("$.strategy_overrides." + quotePathSegment(symbol))
	if err != nil {
		return nil, fmt.Errorf("strategy_overrides.%s path: %w", symbol, err)
	}

	if symNode, _ := symPath.FilterFile(doc); symNode != nil {
		return src, nil // already there; nothing to bootstrap
	}

	lines := lineStarts(src)

	anchor, indent, ok := blockEnd(src, lines, doc, "$.strategy_overrides")
	if !ok {
		return nil, fmt.Errorf("config has a strategy_overrides: block this cannot anchor a new symbol into")
	}

	text := "\n" + strings.Repeat(" ", indent) + symbol + ":"
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:anchor]...)
	out = append(out, text...)
	out = append(out, src[anchor:]...)

	return out, nil
}

// withoutEnvSourced drops any leaf whose path is currently sourced from an
// environment variable rather than config.yaml.
//
// The env var overrides that field again on every restart regardless of what
// the file says, so writing a different value there would be silently
// inert — and worse: persist always writes a whole section's current live
// value, not just the one field the operator actually edited, so without
// this a save of any OTHER field in the same section would drag an
// env-sourced secret into the file for the first time, defeating the
// documented purpose of supplying it via the environment instead of the
// file. See applyNotifyEnv/applyAIEnv and CODE_REVIEW_FINDINGS.md's
// "env-supplied secrets are written to config.yaml by the first settings
// save".
func withoutEnvSourced(section string, leaves []leaf) []leaf {
	out := make([]leaf, 0, len(leaves))

	for _, lf := range leaves {
		if EnvSourced(section, lf.path) {
			continue
		}

		out = append(out, lf)
	}

	return out
}

// patchRounds bounds the replace/insert cycle. A round can create a block the
// next round's leaves need (an `engine:` with no `scale_in:`), and the nesting
// here is two deep, so this is slack rather than a real limit.
const patchRounds = 4

// Line endings the patcher normalises between. Declared rather than inlined so
// the two directions cannot be written the wrong way round.
var (
	crlfBytes = []byte{'\r', '\n'}
	lfBytes   = []byte{'\n'}
)

// patchBytes is Patch on an in-memory document, which is what the tests drive.
//
// CRLF is normalised away first. goccy's token offsets do not count carriage
// returns, so on a Windows-written file every reported position is short by the
// number of line breaks before it — and a splice at that offset lands in the
// middle of an unrelated comment. Rather than carry that correction through
// every calculation, the edit happens in LF space and the file's original line
// ending is restored on the way out.
func patchBytes(src []byte, section string, value any) ([]byte, error) {
	leaves, err := leavesOf(value)
	if err != nil {
		return nil, err
	}

	leaves = withoutEnvSourced(section, leaves)

	crlf := bytes.Contains(src, crlfBytes)

	out := src
	if crlf {
		out = bytes.ReplaceAll(src, crlfBytes, lfBytes)
	}

	leaves = changedOnly(out, section, value, leaves)
	if len(leaves) == 0 {
		return src, nil // nothing to do; hand back the original bytes
	}

	for range patchRounds {
		next, remaining, rerr := applyRound(out, section, leaves)
		if rerr != nil {
			return nil, rerr
		}

		out, leaves = next, remaining

		if len(leaves) == 0 {
			break
		}
	}

	// Read the result back and check it means what was asked. This is the
	// backstop for every rendering asymmetry, known or not — goccy re-renders an
	// embedded newline in a double-quoted scalar as a space, which would quietly
	// rewrite ai.system_prompt.
	if err := verify(out, section, value); err != nil {
		return nil, err
	}

	if crlf {
		out = bytes.ReplaceAll(out, lfBytes, crlfBytes)
	}

	return out, nil
}

// changedOnly drops leaves the file already agrees with.
//
// Without this, saving a section rewrites every one of its keys in Go's
// formatting rather than the file's: 0.30 becomes 0.3, 0 becomes 0.0, and a
// commented-out ladder gains an explicit `partial_take_profit: []`. All
// semantically identical, all pure diff noise, and enough of it to make a save
// look like the destruction this whole file exists to avoid.
//
// The comparison runs both sides through the same marshal, so it is comparing
// meanings rather than spellings: a key the file omits reads back as the zero
// value and matches a zero-valued leaf, which is why an absent optional block
// stays absent.
func changedOnly(src []byte, section string, value any, want []leaf) []leaf {
	current := reflect.New(reflect.TypeOf(value))

	p, err := yaml.PathString("$." + section)
	if err != nil {
		return want
	}

	if err := p.Read(bytes.NewReader(src), current.Interface()); err != nil {
		return want // no readable section; let the caller's insert path handle it
	}

	have, err := leavesOf(current.Elem().Interface())
	if err != nil {
		return want
	}

	text := make(map[string]string, len(have))
	for _, lf := range have {
		text[lf.path] = lf.text
	}

	out := make([]leaf, 0, len(want))

	for _, lf := range want {
		if cur, ok := text[lf.path]; ok && cur == lf.text {
			continue
		}

		out = append(out, lf)
	}

	return out
}

// applyRound rewrites every leaf whose key already exists and creates the blocks
// the rest need, returning the leaves still to be placed.
func applyRound(src []byte, section string, leaves []leaf) ([]byte, []leaf, error) {
	doc, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("parse config: %w", err)
	}

	lines := lineStarts(src)

	var (
		edits   []edit
		missing []leaf
	)

	for _, lf := range leaves {
		p, perr := yaml.PathString("$." + section + "." + lf.path)
		if perr != nil {
			return nil, nil, fmt.Errorf("path %s: %w", lf.path, perr)
		}

		node, ferr := p.FilterFile(doc)
		if ferr != nil || node == nil {
			missing = append(missing, lf)

			continue
		}

		e, eerr := replaceEdit(src, lines, node, lf.text)
		if eerr != nil {
			return nil, nil, eerr
		}

		edits = append(edits, e)
	}

	ins, still, err := insertEdits(src, lines, doc, section, missing)
	if err != nil {
		return nil, nil, err
	}

	return applyEdits(src, append(edits, ins...)), still, nil
}

// edit is a byte span of the source to replace with text.
type edit struct {
	start, end int
	text       string
}

// applyEdits splices every edit into src, right to left so earlier offsets stay
// valid while later ones are rewritten.
func applyEdits(src []byte, edits []edit) []byte {
	if len(edits) == 0 {
		return src
	}

	slices.SortFunc(edits, func(a, b edit) int { return cmp.Compare(b.start, a.start) })

	out := src

	for _, e := range edits {
		tail := append([]byte(e.text), out[e.end:]...)

		out = append(out[:e.start:e.start], tail...)
	}

	return out
}

// lineStarts returns the byte offset at which each line begins.
func lineStarts(src []byte) []int {
	starts := []int{0}

	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}

	return starts
}

// replaceEdit computes the span of a scalar's raw text in the source — or,
// for a flow-style sequence (a ",flow"-tagged []string field, e.g.
// SymbolOverrideFields.Detectors or AutotuneConfig.ExcludeSymbols), the
// span of the WHOLE bracketed list.
//
// A flow sequence needs its own path: ast.SequenceNode.GetToken() returns
// only its Start token (the "[" itself), whose own raw text is a single
// character — replacing just that "[" with a full new list corrupts the
// file, splicing the new text in ahead of the OLD list's remaining
// characters instead of replacing them (`exclude_symbols: [AMZN]` -> `[]`
// produced `[]AMZN]`, not `[]`). See replaceFlowSequenceEdit.
func replaceEdit(src []byte, lines []int, node ast.Node, text string) (edit, error) {
	if seq, ok := node.(*ast.SequenceNode); ok && seq.IsFlowStyle {
		return replaceFlowSequenceEdit(src, lines, seq, text)
	}

	tok := node.GetToken()
	if tok == nil || tok.Position == nil {
		return edit{}, fmt.Errorf("no position for %s", node.GetPath())
	}

	raw := rawText(tok)
	if raw == "" {
		return edit{}, fmt.Errorf("empty token for %s", node.GetPath())
	}

	line := tok.Position.Line - 1
	if line < 0 || line >= len(lines) {
		return edit{}, fmt.Errorf("line %d out of range for %s", tok.Position.Line, node.GetPath())
	}

	start := lines[line] + tok.Position.Column - 1

	// Column is the parser's idea of where the value starts; confirm it against
	// the bytes before splicing, and fall back to finding the raw text on the
	// line. Writing at a wrong offset would corrupt the file silently.
	if start < 0 || start+len(raw) > len(src) || string(src[start:start+len(raw)]) != raw {
		found, ok := findOnLine(src, lines, line, raw)
		if !ok {
			return edit{}, fmt.Errorf("cannot locate %s in the source", node.GetPath())
		}

		start = found
	}

	return edit{start: start, end: start + len(raw), text: text}, nil
}

// replaceFlowSequenceEdit computes the span of an entire flow-style
// sequence, from its opening "[" to its closing "]" inclusive — Start and
// End's own Column positions each locate one bracket character exactly
// (verified against goccy's actual token output, not just its doc
// comments), the same way a scalar token's Column locates its value.
//
// This deliberately ignores each element's own token and any internal
// spacing/formatting (`[A,B]` vs `[A, B]`): the whole bracketed span is one
// leaf (see flatten's SequenceNode case), so on a change every element is
// rewritten together in this codebase's own "A, B" style — there is no
// existing single-element-of-a-list edit to preserve here, unlike the
// per-key formatting Patch otherwise goes to such lengths not to disturb.
func replaceFlowSequenceEdit(src []byte, lines []int, seq *ast.SequenceNode, text string) (edit, error) {
	if seq.Start == nil || seq.Start.Position == nil || seq.End == nil || seq.End.Position == nil {
		return edit{}, fmt.Errorf("no position for %s", seq.GetPath())
	}

	sLine := seq.Start.Position.Line - 1
	if sLine < 0 || sLine >= len(lines) {
		return edit{}, fmt.Errorf("line %d out of range for %s", seq.Start.Position.Line, seq.GetPath())
	}

	start := lines[sLine] + seq.Start.Position.Column - 1

	eLine := seq.End.Position.Line - 1
	if eLine < 0 || eLine >= len(lines) {
		return edit{}, fmt.Errorf("line %d out of range for %s", seq.End.Position.Line, seq.GetPath())
	}

	// Column (1-based) locates "]" itself, so the byte just past it — the
	// exclusive end this edit needs — is at lines[eLine] + Column (no -1).
	end := lines[eLine] + seq.End.Position.Column

	// Confirm both bracket bytes before splicing — the same defensive check
	// the scalar path applies, since writing at a wrong offset would corrupt
	// the file silently, and a flow sequence's own positions have already
	// proven surprising once (GetToken() alone).
	if start < 0 || end > len(src) || end <= start || src[start] != '[' || src[end-1] != ']' {
		return edit{}, fmt.Errorf("cannot locate %s in the source", seq.GetPath())
	}

	return edit{start: start, end: end, text: text}, nil
}

// rawText recovers a token's text exactly as it appears in the file, quotes
// included. Origin carries the surrounding whitespace and any trailing newline,
// which are not part of the value.
func rawText(tok *token.Token) string {
	raw := strings.TrimLeft(tok.Origin, " \t")
	if i := strings.IndexAny(raw, "\r\n"); i >= 0 {
		raw = raw[:i]
	}

	return strings.TrimRight(raw, " \t")
}

// findOnLine locates raw within one line and returns its absolute offset.
func findOnLine(src []byte, lines []int, line int, raw string) (int, bool) {
	start := lines[line]

	end := len(src)
	if line+1 < len(lines) {
		end = lines[line+1]
	}

	i := bytes.Index(src[start:end], []byte(raw))
	if i < 0 {
		return 0, false
	}

	return start + i, true
}

// insertEdits places leaves whose keys are not in the file yet.
//
// Two cases. A config written from an older release is missing a key added since
// — keep_manual is exactly that. A hand-trimmed one can be missing a whole
// nested block, an `engine:` with no `scale_in:`, and its children cannot be
// placed until that block exists; those come back as still-pending and land on
// the next round.
func insertEdits(
	src []byte,
	lines []int,
	doc *ast.File,
	section string,
	leaves []leaf,
) ([]edit, []leaf, error) {
	var (
		edits []edit
		still []leaf
	)

	made := make(map[string]bool)

	for _, lf := range leaves {
		parent, key := splitParent(section, lf.path)

		anchor, indent, ok := blockEnd(src, lines, doc, parent)
		if ok {
			edits = append(edits, edit{
				start: anchor, end: anchor,
				text: "\n" + strings.Repeat(" ", indent) + key + ": " + lf.text,
			})

			continue
		}

		e, err := planParent(src, lines, doc, section, lf, made)
		if err != nil {
			return nil, nil, err
		}

		still = append(still, lf)

		if e != nil {
			edits = append(edits, *e)
		}
	}

	return edits, still, nil
}

// planParent returns the edit that creates the missing block a leaf needs, or
// nil when this run already planned it.
func planParent(
	src []byte,
	lines []int,
	doc *ast.File,
	section string,
	lf leaf,
	made map[string]bool,
) (*edit, error) {
	i := strings.LastIndex(lf.path, ".")
	if i < 0 {
		return nil, fmt.Errorf("config has no %s: block to write %s into", section, lf.path)
	}

	block := lf.path[:i]
	if made[block] {
		return nil, nil //nolint:nilnil // "already planned", not a value
	}

	made[block] = true

	grandparent, name := splitParent(section, block)

	anchor, indent, ok := blockEnd(src, lines, doc, grandparent)
	if !ok {
		return nil, fmt.Errorf("config has no %s block to write %s into", grandparent, lf.path)
	}

	return &edit{
		start: anchor, end: anchor,
		text: "\n" + strings.Repeat(" ", indent) + name + ":",
	}, nil
}

// splitParent splits a leaf path into its parent YAML path and final key.
func splitParent(section, path string) (parent, key string) {
	parent, key = "$."+section, path

	i := strings.LastIndex(path, ".")
	if i >= 0 {
		parent += "." + path[:i]

		key = path[i+1:]
	}

	return parent, key
}

// blockEnd returns the offset just past a mapping's last line and the
// indentation its children use — where a new key belongs.
func blockEnd(
	src []byte,
	lines []int,
	doc *ast.File,
	parent string,
) (anchor, indent int, ok bool) {
	p, err := yaml.PathString(parent)
	if err != nil {
		return 0, 0, false
	}

	node, err := p.FilterFile(doc)
	if err != nil || node == nil {
		return 0, 0, false
	}

	m, ok := node.(*ast.MappingNode)
	if !ok || len(m.Values) == 0 {
		return emptyBlockEnd(src, lines, node)
	}

	last, indent := lastChild(m)
	if last == 0 {
		return 0, 0, false
	}

	return lineEnd(src, lines, last-1), indent, true
}

// lastChild returns the line past which a mapping's content truly ends, and
// the column its keys start at — where a sibling belongs and how far to
// indent it.
//
// A child whose own value is itself a nested mapping or sequence (e.g. one
// symbol's block under strategy_overrides:, itself several fields deep) can
// span many lines below its key's own line. Using only the key's line — as
// this function used to — anchors the new sibling right after that key and
// BEFORE its own nested content, which corrupts the file: the existing
// child's key now has nothing after its colon (reads back as null, silently
// losing that entry), and its former field lines are re-parented under the
// newly-inserted sibling instead. Every mapping this function was exercised
// against before strategy_overrides existed had scalar-only children (e.g.
// screener:'s max_symbols: 25), where "the key's line" and "the value's
// full extent" happen to be the same line, so this never surfaced. See
// CODE_REVIEW_FINDINGS.md's strategy_overrides-sibling-insertion finding.
func lastChild(m *ast.MappingNode) (line, indent int) {
	for _, v := range m.Values {
		tok := v.Key.GetToken()
		if tok == nil || tok.Position == nil {
			continue
		}

		if tok.Position.Line > line {
			line, indent = tok.Position.Line, tok.Position.Column-1
		}

		if end := deepestLine(v.Value); end > line {
			line = end
		}
	}

	return line, indent
}

// deepestLine returns the largest line number any token in node's subtree
// occupies — a mapping or sequence value's true bottom line, regardless of
// how deeply it nests. Counts embedded newlines in a token's own Origin too,
// so a multi-line scalar (a block literal/folded string) is accounted for
// even though it is itself a leaf, not a container node.
//
// Trailing newlines in Origin are excluded from that count first — a live
// corruption traced to exactly this: the LAST field in a mapping, followed
// by a blank line before the next sibling key (a purely cosmetic spacing
// choice — see live strategy_overrides: GOOG/AMZN/AAPL blocks), carries
// Origin like " 0.20\n\n  " for an ordinary single-line "0.20" value — the
// two newlines are the end of its own line plus the blank line, not extra
// lines the VALUE itself spans. Counting them anyway inflated that field's
// reported line by 2, which happened to land exactly on the FOLLOWING
// sibling's own key line — anchoring a new field's insertion there
// spliced it into the wrong symbol's block entirely (mismatched
// indentation relative to that symbol's own existing fields, breaking the
// YAML). A genuine multi-line block scalar's embedded newlines survive this
// trim unaffected, since they sit BETWEEN actual content, not after it.
func deepestLine(node ast.Node) int {
	if node == nil {
		return 0
	}

	line := 0
	if tok := node.GetToken(); tok != nil && tok.Position != nil {
		trimmed := strings.TrimRight(tok.Origin, " \t\r\n")
		line = tok.Position.Line + strings.Count(trimmed, "\n")
	}

	switch n := node.(type) {
	case *ast.MappingNode:
		for _, v := range n.Values {
			if l := deepestLine(v.Value); l > line {
				line = l
			}
		}
	case *ast.SequenceNode:
		for _, item := range n.Values {
			if l := deepestLine(item); l > line {
				line = l
			}
		}
	}

	return line
}

// emptyBlockEnd handles a block with no children yet — one this run just
// created. Its own line is the anchor and its children indent two past it.
func emptyBlockEnd(src []byte, lines []int, node ast.Node) (anchor, indent int, ok bool) {
	tok := node.GetToken()
	if tok == nil || tok.Position == nil {
		return 0, 0, false
	}

	return lineEnd(src, lines, tok.Position.Line-1), tok.Position.Column + 1, true
}

// lineEnd returns the offset of the end of a line, excluding its newline.
func lineEnd(src []byte, lines []int, line int) int {
	if line < 0 || line >= len(lines) {
		return len(src)
	}

	end := len(src)
	if line+1 < len(lines) {
		end = lines[line+1] - 1
	}

	if end > 0 && end <= len(src) && src[end-1] == '\r' {
		end--
	}

	return end
}

// verify re-reads section from out and reports any difference from want.
func verify(out []byte, section string, want any) error {
	fresh := reflect.New(reflect.TypeOf(want))

	p, err := yaml.PathString("$." + section)
	if err != nil {
		return fmt.Errorf("verify path: %w", err)
	}

	if err := p.Read(bytes.NewReader(out), fresh.Interface()); err != nil {
		return fmt.Errorf("verify re-read %s: %w", section, err)
	}

	// Compared leaf-by-leaf (rather than as one marshalled document) so an
	// env-sourced leaf — deliberately excluded from the write by
	// withoutEnvSourced, and so expected to read back as whatever the file
	// already said, not as want's in-memory (environment) value — can be
	// skipped without also skipping every other field's check. Leaves come
	// from the same marshal-then-flatten path leavesOf uses, which is what
	// already erases a nil-vs-empty-slice difference the file cannot express
	// and nobody means.
	wantLeaves, err := leavesOf(want)
	if err != nil {
		return fmt.Errorf("verify marshal: %w", err)
	}

	gotLeaves, err := leavesOf(fresh.Elem().Interface())
	if err != nil {
		return fmt.Errorf("verify marshal: %w", err)
	}

	gotText := make(map[string]string, len(gotLeaves))
	for _, lf := range gotLeaves {
		gotText[lf.path] = lf.text
	}

	for _, lf := range wantLeaves {
		if EnvSourced(section, lf.path) {
			continue
		}

		if got := gotText[lf.path]; got != lf.text {
			return fmt.Errorf(
				"refusing to write %s: %s would not read back as it was set (set %q, reads %q)",
				section, lf.path, lf.text, got,
			)
		}
	}

	return nil
}

// leaf is one scalar to write, as a dotted path relative to the section and the
// YAML text of its value.
type leaf struct {
	path string
	text string
}

// leavesOf marshals value and flattens it to scalar leaves.
//
// Marshalling first is what keeps this honest: the keys, the nesting and the
// rendering of every type come from the struct's own yaml tags, so a field
// renamed in the struct cannot silently start writing to the wrong key.
func leavesOf(value any) ([]leaf, error) {
	body, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal section: %w", err)
	}

	doc, err := parser.ParseBytes(body, 0)
	if err != nil {
		return nil, fmt.Errorf("parse section: %w", err)
	}

	if len(doc.Docs) == 0 {
		return nil, nil
	}

	var out []leaf

	flatten(doc.Docs[0].Body, "", &out)

	return out, nil
}

// flatten walks a mapping, emitting one leaf per scalar. Sequences and empty
// mappings are emitted whole: a list has no stable per-element identity, so
// replacing it wholesale is the only meaning "set this list" can have.
func flatten(node ast.Node, prefix string, out *[]leaf) {
	m, ok := node.(*ast.MappingNode)
	if !ok {
		if node != nil && prefix != "" {
			*out = append(*out, leaf{path: prefix, text: node.String()})
		}

		return
	}

	for _, v := range m.Values {
		key := v.Key.GetToken().Value

		path := key
		if prefix != "" {
			path = prefix + "." + key
		}

		if _, nested := v.Value.(*ast.MappingNode); nested {
			flatten(v.Value, path, out)

			continue
		}

		*out = append(*out, leaf{path: path, text: v.Value.String()})
	}
}

// writeAtomic replaces file's contents via a temp file and a rename, so a crash
// mid-write cannot leave a truncated config — this one holds live API keys.
//
// The replacement is chmod'd to match the EXISTING file's permissions before
// the rename, not a hardcoded value — os.CreateTemp always creates its file
// 0600 regardless of what file already had, and a rename does not carry a
// file's mode with it, so without this every save through the API silently
// tightened the file's permissions to owner-only. That is surprising and
// destructive for a deployment that deliberately made the file group-
// readable/writable (e.g. a shared volume another process or user needs
// access to) — this preserves whatever the operator already set. 0600 is
// still the fallback for a config that does not exist yet, since this file
// holds live API keys and a brand new one should start locked down.
func writeAtomic(file string, data []byte) error {
	dir := filepath.Dir(file)

	mode := os.FileMode(0o600)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}

	name := tmp.Name()

	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write temp: %w", err)
	}

	// Flush before the rename, or a crash can leave the new name pointing at an
	// empty file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("sync temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	if err := os.Chmod(name, mode); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}

	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}

	return nil
}
