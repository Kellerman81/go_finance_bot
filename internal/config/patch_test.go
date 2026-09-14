package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

// A config file is mostly documentation. Measured on the real one: 679 lines, of
// which 342 are comments explaining why each number is what it is. An
// unmarshal/marshal round trip produced 304 lines with none of them, and dropped
// strategy_overrides entirely. These tests exist to keep that from happening
// through the settings screen.

// realistic mirrors the shape of a hand-written config: head comments, trailing
// comments, commented-out blocks, credentials, and keys the patcher never
// touches.
const realistic = `# go_finance_bot configuration.
server:
  auth_token: "s3cr3t-do-not-touch"

# ---------------------------------------------------------------------------
# Protective exits.
# ---------------------------------------------------------------------------
exits:
  enabled: true
  stop_loss_pct: 0.10         # sell if price falls 10% below entry
  trailing_stop_pct: 0.08
  take_profit_pct: 0.30
  # Volatility-adaptive stops. Read this before turning them on.
  atr_period: 14
  atr_stop_mult: 0            # NOT 2 on 1-minute bars
  atr_trail_mult: 0
  trailing_offset_pct: 0
  trailing_tight_pct: 0
  trailing_only_after_offset: false
  max_hold: 720h

# A commented-out block a marshal round-trip would delete outright.
# strategy_overrides:
#   NVDA:
#     min_strength: 0.40

isins:
  PRAJL.XC: US69608A1088
`

func patched(t *testing.T, src string, section string, value any) string {
	t.Helper()

	out, err := patchBytes([]byte(src), section, value)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	return string(out)
}

func ptrF(v float64) *float64    { return &v }
func ptrB(v bool) *bool          { return &v }
func ptrSS(v []string) *[]string { return &v }

// The value changes and nothing else does.
func TestPatchChangesOnlyTheValue(t *testing.T) {
	t.Parallel()

	ex := ExitsConfig{
		Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.08,
		TakeProfitPct: 0.30, ATRPeriod: 14, MaxHold: 720 * time.Hour,
	}

	out := patched(t, realistic, "exits", ex)

	if !strings.Contains(out, "stop_loss_pct: 0.05") {
		t.Errorf("the new value is missing:\n%s", out)
	}

	if strings.Contains(out, "stop_loss_pct: 0.10") {
		t.Error("the old value survived")
	}
}

// Every comment must survive — that is the whole reason this is not a marshal.
func TestPatchPreservesComments(t *testing.T) {
	t.Parallel()

	out := patched(t, realistic, "exits", ExitsConfig{StopLossPct: 0.05})

	for _, want := range []string{
		"# go_finance_bot configuration.",
		"# Protective exits.",
		"# Volatility-adaptive stops. Read this before turning them on.",
		"# NOT 2 on 1-minute bars",
		"sell if price falls 10% below entry", // trailing comment on the EDITED line
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comment lost: %q\n---\n%s", want, out)
		}
	}
}

// A commented-out block is configuration the operator is keeping for later.
// A marshal deletes it; a patch must not.
func TestPatchKeepsCommentedOutBlocks(t *testing.T) {
	t.Parallel()

	out := patched(t, realistic, "exits", ExitsConfig{StopLossPct: 0.05})

	for _, want := range []string{"# strategy_overrides:", "#     min_strength: 0.40"} {
		if !strings.Contains(out, want) {
			t.Errorf("commented-out config was deleted: %q", want)
		}
	}
}

// The file holds live broker credentials and keys the patcher has no business
// touching. Editing one section must leave every other byte alone.
func TestPatchLeavesOtherSectionsIntact(t *testing.T) {
	t.Parallel()

	out := patched(t, realistic, "exits", ExitsConfig{StopLossPct: 0.05})

	for _, want := range []string{
		`auth_token: "s3cr3t-do-not-touch"`,
		"PRAJL.XC: US69608A1088",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("content outside the edited section changed: %q missing", want)
		}
	}
}

// A key added by a later release is not in an older file. keep_manual is exactly
// that case, and a settings screen that silently failed to save it would be
// worse than one that never offered it.
func TestPatchInsertsAKeyTheFileLacks(t *testing.T) {
	t.Parallel()

	src := "screener:\n  enabled: false   # keep this comment\n  max_symbols: 25\n"

	out := patched(t, src, "screener", ScreenerConfig{
		Enabled: true, MaxSymbols: 5, KeepManual: true, Rank: "volume",
	})

	if !strings.Contains(out, "keep_manual: true") {
		t.Errorf("a key absent from the file was not inserted:\n%s", out)
	}

	if !strings.Contains(out, "keep this comment") {
		t.Error("inserting a key disturbed an existing comment")
	}
}

// TestPatchWritesSymbolListFieldsAsFlowStyle reproduces a corruption a user
// hit live: AutotuneConfig.RunOnlyForSymbols/ExcludeSymbols are []string
// fields with no ",flow" yaml tag, so goccy's default block style (one item
// per line) got spliced onto the single "key: value" line the leaf-splice
// machinery expects, corrupting the file the moment either field was first
// populated — exactly SymbolOverrideFields.Detectors's own documented
// failure mode (see its doc comment in patch.go), re-introduced for a
// different struct. Both fields carry the same ",flow" tag now for the
// same reason: a bare []string marshals multi-line by default (struct
// field or map value alike), which this splicer cannot place correctly.
//
// Run against a real copy of data/config.example.yaml, whose autotune:
// block has exclude_symbols/run_only_for_symbols present only as COMMENTED
// placeholders — the exact shape that hits config.Patch's insert-a-new-key
// path (not merely replace-an-existing-line), which is where this
// corruption actually happened live.
func TestPatchWritesSymbolListFieldsAsFlowStyle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	base, err := os.ReadFile("../../data/config.example.yaml")
	if err != nil {
		t.Skipf("example config unavailable: %v", err)
	}

	if err := os.WriteFile(file, base, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := Load(file)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}

	want := before.Autotune
	want.ExcludeSymbols = []string{"GOOG", "AMZN"}
	want.RunOnlyForSymbols = []string{"AAPL"}

	if err := Patch(file, "autotune", want); err != nil {
		t.Fatalf("patch: %v", err)
	}

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), "exclude_symbols: [GOOG, AMZN]") {
		t.Errorf("exclude_symbols not written as a single-line flow list:\n%s", out)
	}

	if !strings.Contains(string(out), "run_only_for_symbols: [AAPL]") {
		t.Errorf("run_only_for_symbols not written as a single-line flow list:\n%s", out)
	}

	after, err := Load(file)
	if err != nil {
		t.Fatalf("the patched config no longer loads: %v", err)
	}

	if len(after.Autotune.ExcludeSymbols) != 2 || len(after.Autotune.RunOnlyForSymbols) != 1 {
		t.Errorf("after reload: ExcludeSymbols=%v RunOnlyForSymbols=%v, want [GOOG AMZN] / [AAPL]",
			after.Autotune.ExcludeSymbols, after.Autotune.RunOnlyForSymbols)
	}

	// The corruption a user actually hit live: re-patching an EXISTING
	// flow-style list to a DIFFERENT one (here, back down to empty) exercises
	// replaceEdit rather than insertEdits — ast.SequenceNode.GetToken()
	// returns only its opening "[" token, so treating that as the whole
	// leaf's span used to splice the new text one character in, leaving the
	// old list's remaining characters behind ("[AMZN]" -> "[]" produced
	// "[]AMZN]", not "[]" — caught by verify() before it ever reached disk,
	// but the save still failed outright). See replaceFlowSequenceEdit.
	shrunk := after.Autotune
	shrunk.ExcludeSymbols = nil
	shrunk.RunOnlyForSymbols = []string{"MSFT"}

	if err := Patch(file, "autotune", shrunk); err != nil {
		t.Fatalf("patch (shrinking an existing flow list): %v", err)
	}

	out, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), "exclude_symbols: []") {
		t.Errorf("exclude_symbols not shrunk to an empty flow list:\n%s", out)
	}

	if !strings.Contains(string(out), "run_only_for_symbols: [MSFT]") {
		t.Errorf("run_only_for_symbols not replaced with the new single-element list:\n%s", out)
	}

	// The exact corruption reproduced: the old list's trailing characters
	// spliced in right after the new value ("[]AMZN]" instead of "[]").
	// Confirmed within the autotune: block specifically, not a whole-file
	// search — config.example.yaml's own commented strategy_overrides
	// example legitimately mentions GOOG/AAPL elsewhere.
	if i := strings.Index(string(out), "autotune:"); i >= 0 {
		block := string(out)[i:]
		if j := strings.Index(block[len("autotune:"):], "\n\n"); j >= 0 {
			block = block[:len("autotune:")+j]
		}

		if strings.Contains(block, "]AMZN]") || strings.Contains(block, "]GOOG]") {
			t.Errorf("stray characters left behind from the old list values in the autotune: block:\n%s", block)
		}
	}

	final, err := Load(file)
	if err != nil {
		t.Fatalf("the shrunk config no longer loads: %v", err)
	}

	if len(final.Autotune.ExcludeSymbols) != 0 {
		t.Errorf("ExcludeSymbols after shrinking = %v, want empty", final.Autotune.ExcludeSymbols)
	}

	if len(final.Autotune.RunOnlyForSymbols) != 1 || final.Autotune.RunOnlyForSymbols[0] != "MSFT" {
		t.Errorf("RunOnlyForSymbols after replacing = %v, want [MSFT]", final.Autotune.RunOnlyForSymbols)
	}
}

// TestPatchExcludesAnEnvSourcedSecret reproduces the "env-supplied secrets
// are written to config.yaml by the first settings save" finding: a real
// deployment can deliberately keep a token out of the file by supplying it
// via BOT_NOTIFY_TELEGRAM_TOKEN instead (see applyNotifyEnv). Persisting a
// settings save always writes a whole section's current live value, not just
// the one field actually edited — so saving an unrelated field (here,
// min_notional) used to drag the env-sourced token into the file for the
// first time, defeating the whole point of using the environment for it.
//
// Not run in parallel: it uses t.Setenv, which Go's testing package forbids
// alongside t.Parallel.
func TestPatchExcludesAnEnvSourcedSecret(t *testing.T) {
	t.Setenv("BOT_NOTIFY_TELEGRAM_TOKEN", "env-secret-token")

	var cfg Config

	applyNotifyEnv(&cfg) // populates envSecretPaths["notify"]["telegram_token"]
	t.Cleanup(func() { resetEnvSecretPaths("notify") })

	src := "notify:\n  enabled: true\n  min_notional: 10\n"

	out := patched(t, src, "notify", NotifyConfig{
		Enabled:       true,
		MinNotional:   25,                 // the field actually being changed
		TelegramToken: "env-secret-token", // carried in memory, sourced from the env
	})

	if strings.Contains(out, "env-secret-token") {
		t.Errorf("the env-sourced secret was written to the file:\n%s", out)
	}

	if !strings.Contains(out, "min_notional: 25") {
		t.Errorf("the legitimately-edited field was not saved:\n%s", out)
	}
}

// The same secret must still be excluded on the insert path (a key the file
// does not have yet within an existing section), not just the replace path
// (a key already present).
func TestPatchExcludesAnEnvSourcedSecretOnTheInsertPath(t *testing.T) {
	t.Setenv("BOT_AI_API_KEY", "env-secret-key")

	var cfg Config

	applyAIEnv(&cfg)
	t.Cleanup(func() { resetEnvSecretPaths("ai") })

	// api_key is absent from the file, same as model — both would be new
	// keys to insert. Only model should actually be inserted.
	out := patched(t, "ai:\n  enabled: false\n", "ai", AIConfig{
		Enabled: true, Provider: "ollama", Model: "m", BaseURL: "http://x",
		APIKey: "env-secret-key",
	})

	if strings.Contains(out, "env-secret-key") {
		t.Errorf("the env-sourced secret was inserted as a new key:\n%s", out)
	}

	if !strings.Contains(out, "model: m") {
		t.Errorf("the legitimately new field was not inserted:\n%s", out)
	}
}

// A CHANGED duration must be written as a duration, not as nanosecond integers
// — a config nobody can read is barely better than one that lost its comments.
func TestPatchWritesReadableDurations(t *testing.T) {
	t.Parallel()

	out := patched(t, realistic, "exits", ExitsConfig{MaxHold: 48 * time.Hour})

	if !strings.Contains(out, "max_hold: 48h0m0s") {
		t.Errorf("max_hold is not human-readable:\n%s", out)
	}

	if strings.Contains(out, "172800000000000") {
		t.Error("a duration was written as nanoseconds")
	}
}

// A duration the file already expresses differently but means identically must
// be left alone. "720h" and "720h0m0s" are the same 30 days, and rewriting one
// into the other is churn in a diff that should have been empty.
func TestPatchLeavesAnEquivalentDurationAlone(t *testing.T) {
	t.Parallel()

	out := patched(t, realistic, "exits", ExitsConfig{MaxHold: 720 * time.Hour})

	if !strings.Contains(out, "max_hold: 720h") || strings.Contains(out, "720h0m0s") {
		t.Errorf("an unchanged duration was reformatted:\n%s", out)
	}
}

// The same for numbers: 0.30 and 0.3 are one value, and 0 is not worth
// rewriting as 0.0. Saving a section must not churn every line in it.
func TestPatchDoesNotReformatUnchangedNumbers(t *testing.T) {
	t.Parallel()

	src := "exits:\n  take_profit_pct: 0.30\n  atr_stop_mult: 0\n  stop_loss_pct: 0.10\n"

	out := patched(t, src, "exits", ExitsConfig{
		TakeProfitPct: 0.30, ATRStopMult: 0, StopLossPct: 0.05,
	})

	if !strings.Contains(out, "take_profit_pct: 0.30") {
		t.Errorf("0.30 was rewritten as 0.3:\n%s", out)
	}

	if !strings.Contains(out, "stop_loss_pct: 0.05") {
		t.Errorf("the value that actually changed was not written:\n%s", out)
	}
}

// And the result must load again. A patch that produces a file the loader
// rejects would take the bot down at its next restart.
func TestPatchedConfigStillLoads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	base, err := os.ReadFile("../../data/config.example.yaml")
	if err != nil {
		t.Skipf("example config unavailable: %v", err)
	}

	if err := os.WriteFile(file, base, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before, err := Load(file)
	if err != nil {
		t.Fatalf("load before: %v", err)
	}

	want := before.Exits
	want.StopLossPct = 0.055

	if err := Patch(file, "exits", want); err != nil {
		t.Fatalf("patch: %v", err)
	}

	after, err := Load(file)
	if err != nil {
		t.Fatalf("the patched config no longer loads: %v", err)
	}

	if after.Exits.StopLossPct != 0.055 {
		t.Errorf("stop_loss_pct = %v after reload, want 0.055", after.Exits.StopLossPct)
	}
}

// TestConcurrentPatchesDoNotLoseAnUpdate reproduces CRIT-10: two Patch calls
// racing on the same file both read the same original bytes, and without
// serialization each computes its own patched version from that stale
// snapshot — whichever writeAtomic's rename lands last wins outright, and
// the other call's change is silently gone even though it returned nil.
// Proven empirically over many repetitions rather than by inspection, since
// the race depends on scheduling.
func TestConcurrentPatchesDoNotLoseAnUpdate(t *testing.T) {
	t.Parallel()

	base, err := os.ReadFile("../../data/config.example.yaml")
	if err != nil {
		t.Skipf("example config unavailable: %v", err)
	}

	for range 20 { // a single run is not guaranteed to hit the race window
		dir := t.TempDir()
		file := filepath.Join(dir, "config.yaml")

		if err := os.WriteFile(file, base, 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}

		before, err := Load(file)
		if err != nil {
			t.Fatalf("load before: %v", err)
		}

		wantExits := before.Exits
		wantExits.StopLossPct = 0.077

		wantLimits := before.Limits
		wantLimits.MaxOrderValue = 4242

		done := make(chan error, 2)

		go func() { done <- Patch(file, "exits", wantExits) }()
		go func() { done <- Patch(file, "limits", wantLimits) }()

		for range 2 {
			if err := <-done; err != nil {
				t.Fatalf("patch: %v", err)
			}
		}

		after, err := Load(file)
		if err != nil {
			t.Fatalf("load after: %v", err)
		}

		if after.Exits.StopLossPct != 0.077 {
			t.Fatalf("exits change lost: stop_loss_pct = %v, want 0.077", after.Exits.StopLossPct)
		}

		if after.Limits.MaxOrderValue != 4242 {
			t.Fatalf("limits change lost: max_order_value = %v, want 4242", after.Limits.MaxOrderValue)
		}
	}
}

// The write is atomic, so a crash cannot leave a truncated file holding live
// credentials. Proven by the absence of leftovers, not by inspection.
func TestPatchLeavesNoTempFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(file, []byte(realistic), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Patch(file, "exits", ExitsConfig{StopLossPct: 0.05}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		t.Errorf("directory holds %v, want only config.yaml", names)
	}
}

// A save through the settings API must not silently tighten the file's
// permissions. os.CreateTemp always creates its file 0600 regardless of what
// the real file already had, and a rename does not carry a file's mode with
// it — without preserving the original mode explicitly, every single save
// would reset a deliberately group-readable/writable config (e.g. on a
// shared volume another process or user needs access to) down to
// owner-only, invisibly, on the very first settings change.
func TestPatchPreservesExistingFilePermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(file, []byte(realistic), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Patch(file, "exits", ExitsConfig{StopLossPct: 0.05}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	fi, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("mode after patch = %o, want the original 0640 preserved", got)
	}
}

// writeAtomic itself still defaults a brand new file to owner-only — there is
// no existing mode to preserve, and this file holds live API keys.
func TestWriteAtomicDefaultsToOwnerOnlyForANewFile(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "config.yaml")

	if err := writeAtomic(file, []byte("a: 1\n")); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}

	fi, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode for a new file = %o, want 0600", got)
	}
}

// A section the file does not have at all is reported, not invented. Adding a
// whole block to a hand-written file is beyond what a settings form should do.
func TestPatchRefusesAMissingSection(t *testing.T) {
	t.Parallel()

	_, err := patchBytes([]byte("exits:\n  enabled: true\n"), "portfolio",
		PortfolioConfig{Enabled: true})
	if err == nil {
		t.Error("patching a section the file lacks silently succeeded")
	}
}

// TestLastChildAnchorsPastANestedChildsFullContent reproduces a corruption
// bug found while designing the scheduled per-symbol auto-optimizer:
// strategy_overrides: is the first section in this codebase whose children
// are themselves multi-line nested mappings (one symbol's own
// detectors/combine/min_strength), and the first place a brand-new sibling
// key ever needs inserting into such a section (today it's hand-edited
// only). blockEnd's lastChild helper used to return the LAST CHILD'S OWN KEY
// LINE as the insertion anchor — correct for a scalar-only mapping (every
// existing caller/test, e.g. screener:'s max_symbols: 25, where the key's
// line and the value's full extent are the same line), wrong here: splicing
// a new sibling at "AMZN:"'s own line lands it BETWEEN that key and its own
// nested fields, corrupting the file — "AMZN:" reads back as null (AMZN's
// override silently disappears) and its former field lines are re-parented
// under the newly-inserted symbol instead.
//
// This directly exercises blockEnd/lastChild (unexported, same-package) on
// a real strategy_overrides-shaped fixture, mirroring live data/config.yaml,
// simulates exactly the insertion insertEdits performs at the returned
// anchor, then re-parses the RESULT and checks AMZN's own fields survived
// intact and the new symbol was actually added — a black-box check of the
// anchor's correctness rather than asserting a specific line number.
func TestLastChildAnchorsPastANestedChildsFullContent(t *testing.T) {
	t.Parallel()

	src := []byte(`strategy_overrides:
  # GOOG: hand-derived reasoning that must survive untouched.
  GOOG:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20
  # AMZN: same.
  AMZN:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20
`)

	doc, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	lines := lineStarts(src)

	anchor, indent, ok := blockEnd(src, lines, doc, "$.strategy_overrides")
	if !ok {
		t.Fatal("blockEnd reported the strategy_overrides mapping as not found")
	}

	// Simulate exactly what insertEdits does when adding a new sibling key.
	inserted := string(src[:anchor]) + "\n" + strings.Repeat(" ", indent) + "MSFT:" +
		string(src[anchor:])

	var doc2 struct {
		StrategyOverrides map[string]struct {
			Detectors   []string `yaml:"detectors"`
			Combine     string   `yaml:"combine"`
			MinStrength float64  `yaml:"min_strength"`
		} `yaml:"strategy_overrides"`
	}

	if err := yaml.Unmarshal([]byte(inserted), &doc2); err != nil {
		t.Fatalf("re-parse after simulated insertion: %v\n%s", err, inserted)
	}

	amzn, ok := doc2.StrategyOverrides["AMZN"]
	if !ok || amzn.MinStrength != 0.20 || amzn.Combine != "unanimous" || len(amzn.Detectors) != 3 {
		t.Errorf(
			"AMZN's own fields were lost or corrupted by the sibling insertion "+
				"(got %+v):\n%s",
			amzn, inserted,
		)
	}

	if _, ok := doc2.StrategyOverrides["MSFT"]; !ok {
		t.Errorf("MSFT was not actually inserted as a sibling:\n%s", inserted)
	}
}

// The one value in the config that can legitimately contain newlines is the LLM
// system prompt. goccy re-renders an embedded newline in a double-quoted scalar
// as a space, so writing one naively would silently rewrite the operator's
// prompt. The verification step must catch that and refuse rather than corrupt.
func TestPatchRefusesRatherThanCorruptAMultiLineValue(t *testing.T) {
	t.Parallel()

	src := "ai:\n  enabled: false\n  system_prompt: \"\"\n"

	ai := AIConfig{SystemPrompt: "line one\nline two"}

	out, err := patchBytes([]byte(src), "ai", ai)
	if err != nil {
		return // refused, which is the safe outcome
	}

	// If it did write, the value must survive intact — anything else is silent
	// corruption of a field the operator wrote by hand.
	var back struct {
		AI AIConfig `yaml:"ai"`
	}

	if uerr := yaml.Unmarshal(out, &back); uerr != nil {
		t.Fatalf("re-read: %v", uerr)
	}

	if back.AI.SystemPrompt != ai.SystemPrompt {
		t.Errorf("the prompt was silently rewritten:\n set:  %q\n read: %q\n%s",
			ai.SystemPrompt, back.AI.SystemPrompt, out)
	}
}

// symbolOverridesDoc unmarshals just enough of a config file to inspect
// strategy_overrides: in PatchSymbolOverride's tests.
type symbolOverridesDoc struct {
	StrategyOverrides map[string]struct {
		Detectors   []string `yaml:"detectors"`
		Combine     string   `yaml:"combine"`
		MinStrength float64  `yaml:"min_strength"`
	} `yaml:"strategy_overrides"`
}

// TestPatchSymbolOverrideBootstrapsAMissingTopLevelBlock covers the first
// bootstrap case: a config with no strategy_overrides: key anywhere.
func TestPatchSymbolOverrideBootstrapsAMissingTopLevelBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(file, []byte("exits:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := PatchSymbolOverride(file, "MSFT", SymbolOverrideFields{
		Detectors: []string{"ema_cross", "rsi", "macd"}, Combine: "unanimous", MinStrength: ptrF(0.20),
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	var doc symbolOverridesDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}

	msft, ok := doc.StrategyOverrides["MSFT"]
	if !ok || msft.Combine != "unanimous" || msft.MinStrength != 0.20 || len(msft.Detectors) != 3 {
		t.Errorf("MSFT override = %+v (present=%v), want the fields just set:\n%s", msft, ok, out)
	}

	if !strings.Contains(string(out), "enabled: true") {
		t.Errorf("the unrelated exits: section was disturbed:\n%s", out)
	}
}

// TestPatchSymbolOverrideBootstrapsANewSymbolUnderAnExistingBlock covers the
// second bootstrap case, and is the direct regression test for the
// lastChild corruption: strategy_overrides: already has GOOG and AMZN
// (comments and all, mirroring live data/config.yaml); adding MSFT must
// leave both untouched.
func TestPatchSymbolOverrideBootstrapsANewSymbolUnderAnExistingBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	seed := `strategy_overrides:
  # GOOG: hand-derived reasoning that must survive untouched.
  GOOG:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20
  # AMZN: same.
  AMZN:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20
`
	if err := os.WriteFile(file, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := PatchSymbolOverride(file, "MSFT", SymbolOverrideFields{
		Detectors: []string{"ema_cross", "adx"}, Combine: "weighted", MinStrength: ptrF(0.30),
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	var doc symbolOverridesDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}

	goog := doc.StrategyOverrides["GOOG"]
	if goog.Combine != "unanimous" || goog.MinStrength != 0.20 || len(goog.Detectors) != 3 {
		t.Errorf("GOOG was disturbed by adding MSFT as a sibling: %+v\n%s", goog, out)
	}

	amzn := doc.StrategyOverrides["AMZN"]
	if amzn.Combine != "unanimous" || amzn.MinStrength != 0.20 || len(amzn.Detectors) != 3 {
		t.Errorf("AMZN was disturbed by adding MSFT as a sibling: %+v\n%s", amzn, out)
	}

	msft := doc.StrategyOverrides["MSFT"]
	if msft.Combine != "weighted" || msft.MinStrength != 0.30 || len(msft.Detectors) != 2 {
		t.Errorf("MSFT = %+v, want the fields just set:\n%s", msft, out)
	}

	if !strings.Contains(string(out), "hand-derived reasoning that must survive untouched") ||
		!strings.Contains(string(out), "# AMZN: same.") {
		t.Errorf("comments on the existing symbols were lost:\n%s", out)
	}
}

// TestPatchSymbolOverrideUpdatesAnExistingSymbolInPlace covers a symbol that
// already has its own block: no bootstrap should be needed, and the
// ordinary field-splice path (unchanged) should update only the fields
// given, preserving the symbol's own comment.
func TestPatchSymbolOverrideUpdatesAnExistingSymbolInPlace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	seed := `strategy_overrides:
  # GOOG: hand-derived reasoning.
  GOOG:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20
`
	if err := os.WriteFile(file, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := PatchSymbolOverride(file, "GOOG", SymbolOverrideFields{MinStrength: ptrF(0.25)})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	var doc symbolOverridesDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}

	goog := doc.StrategyOverrides["GOOG"]
	if goog.MinStrength != 0.25 {
		t.Errorf("min_strength = %v, want 0.25 (the updated value):\n%s", goog.MinStrength, out)
	}

	if goog.Combine != "unanimous" || len(goog.Detectors) != 3 {
		t.Errorf("fields not named in the update were disturbed: %+v\n%s", goog, out)
	}

	if !strings.Contains(string(out), "# GOOG: hand-derived reasoning.") {
		t.Errorf("GOOG's own comment was lost by an in-place update:\n%s", out)
	}
}

// TestPatchSymbolOverrideHandlesDottedSymbolNames reproduces a live
// autotune failure: every exchange-suffixed ticker (SAP.DE, LXS.DE, EVK.DE,
// ... — the majority of a typical European watchlist) failed with "config
// has no strategy_overrides.SAP.DE: block to write detectors into", even
// though the block genuinely didn't exist yet and PatchSymbolOverride's
// whole purpose is to create it. The cause: every internal
// yaml.PathString("$.strategy_overrides." + symbol) call treats an
// unquoted "." as a path-nesting separator, not a literal character, so
// "SAP.DE" resolved as two levels ($.strategy_overrides.SAP.DE, i.e.
// key "SAP" then key "DE") instead of the single mapping key it actually
// is — the lookup silently found nothing, so ensureSymbolOverrideBlock
// could never confirm the block already existed (or anchor off it),
// and every subsequent field lookup failed the same way.
//
// Covers all three PatchSymbolOverride paths for a dotted symbol: bootstrap
// from nothing, add as a new sibling under an existing block, and update an
// existing dotted entry in place.
func TestPatchSymbolOverrideHandlesDottedSymbolNames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	// Starts with no strategy_overrides: block at all — the bootstrap path.
	if err := os.WriteFile(file, []byte("exits:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := PatchSymbolOverride(file, "SAP.DE", SymbolOverrideFields{
		Detectors: []string{"ema_cross", "rsi", "macd"}, Combine: "unanimous", MinStrength: ptrF(0.20),
	})
	if err != nil {
		t.Fatalf("bootstrap patch for a dotted symbol: %v", err)
	}

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	var doc symbolOverridesDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}

	sap, ok := doc.StrategyOverrides["SAP.DE"]
	if !ok || sap.Combine != "unanimous" || sap.MinStrength != 0.20 || len(sap.Detectors) != 3 {
		t.Fatalf("SAP.DE override = %+v (present=%v), want the fields just set:\n%s", sap, ok, out)
	}

	// A SECOND dotted symbol, added as a new sibling under the block SAP.DE
	// itself just created — exercises ensureSymbolOverrideBlock's
	// already-exists check (also dot-broken) and the sibling-insert anchor.
	err = PatchSymbolOverride(file, "LXS.DE", SymbolOverrideFields{
		Detectors: []string{"trend"}, Combine: "weighted", MinStrength: ptrF(0.30),
	})
	if err != nil {
		t.Fatalf("sibling patch for a second dotted symbol: %v", err)
	}

	out, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse after second symbol: %v\n%s", err, out)
	}

	if sap, ok := doc.StrategyOverrides["SAP.DE"]; !ok || len(sap.Detectors) != 3 {
		t.Errorf("SAP.DE disturbed by adding a sibling: %+v (present=%v)\n%s", sap, ok, out)
	}

	lxs, ok := doc.StrategyOverrides["LXS.DE"]
	if !ok || lxs.Combine != "weighted" || lxs.MinStrength != 0.30 {
		t.Errorf("LXS.DE override = %+v (present=%v), want the fields just set:\n%s", lxs, ok, out)
	}

	// Updating SAP.DE's own already-existing dotted entry in place —
	// exercises the replace-an-existing-leaf path for a dotted symbol.
	err = PatchSymbolOverride(file, "SAP.DE", SymbolOverrideFields{MinStrength: ptrF(0.45)})
	if err != nil {
		t.Fatalf("in-place update for a dotted symbol: %v", err)
	}

	out, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse after update: %v\n%s", err, out)
	}

	if sap := doc.StrategyOverrides["SAP.DE"]; sap.MinStrength != 0.45 || sap.Combine != "unanimous" {
		t.Errorf("SAP.DE after in-place update = %+v, want min_strength 0.45 with combine unchanged:\n%s", sap, out)
	}
}

// TestPatchSymbolOverrideDoesNotSpliceIntoTheFollowingSiblingsBlock
// reproduces a live production corruption: a mapping's LAST field followed
// by a purely cosmetic blank line before the next sibling key — exactly
// GOOG/AMZN/AAPL's own shape in the real config.yaml — carries a value
// token whose Origin includes that blank line's newlines (e.g. " 0.20\n\n  "
// for an ordinary single-line "0.20"). deepestLine used to count ALL of
// Origin's newlines as if the value itself spanned that many extra lines,
// inflating AMZN's reported block-end by 2 lines — which happened to land
// exactly on the FOLLOWING sibling's (AAPL's) own key line. Anchoring a new
// field there spliced it into AAPL's block instead of AMZN's, with AMZN's
// indentation rather than AAPL's own — invalid YAML next to AAPL's real,
// differently-indented fields (autotune-bootstrapped blocks land at a wider
// indent than a hand-written one — see ensureSymbolOverrideBlock's own
// noted quirk), caught by verify() before it reached disk.
func TestPatchSymbolOverrideDoesNotSpliceIntoTheFollowingSiblingsBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	// Byte-for-byte the real file's shape: AMZN's last field directly
	// followed by a blank line, then AAPL — an autotune-bootstrapped block
	// (10-space child indent, distinct from AMZN's 4-space) with its own
	// pre-existing fields that must stay completely undisturbed.
	seed := `strategy_overrides:
  GOOG:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20
  AMZN:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20

  AAPL:
          trend_gate: false
          min_strength: 0.1
          combine: weighted
          detectors: [rsi, macd, bollinger]
  SNDK:
          min_strength: 0.3
          combine: majority
          detectors: [ema_cross, rsi, macd, vwap]
          trend_gate: false
`
	if err := os.WriteFile(file, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A plausible autotune proposal for AMZN: shrinks detectors and adds
	// two brand-new fields (trend_gate, detectors_sell) not previously in
	// AMZN's block — exactly the insert-new-field path that anchors off
	// deepestLine.
	err := PatchSymbolOverride(file, "AMZN", SymbolOverrideFields{
		Detectors: []string{"rsi", "macd"}, Combine: "weighted", MinStrength: ptrF(0.15),
		DetectorsSell: ptrSS([]string{"bollinger"}), TrendGate: ptrB(false),
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	var doc symbolOverridesDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}

	amzn := doc.StrategyOverrides["AMZN"]
	if amzn.Combine != "weighted" || amzn.MinStrength != 0.15 || len(amzn.Detectors) != 2 {
		t.Errorf("AMZN not updated correctly = %+v:\n%s", amzn, out)
	}

	// AAPL's own pre-existing fields must be completely untouched — no new
	// fields spliced in ahead of them, no reindented lines.
	if !strings.Contains(string(out), "  AAPL:\n          trend_gate: false\n          min_strength: 0.1\n"+
		"          combine: weighted\n          detectors: [rsi, macd, bollinger]\n") {
		t.Errorf("AAPL's block was disturbed by AMZN's own update:\n%s", out)
	}

	if !strings.Contains(string(out), "  SNDK:") {
		t.Errorf("SNDK went missing entirely:\n%s", out)
	}

	if _, err := Load(file); err != nil {
		t.Errorf("the patched config no longer loads: %v\n%s", err, out)
	}
}

// TestPatchSymbolOverrideRoundTripsThroughLoad mirrors
// TestPatchedConfigStillLoads: a scheduler-written override must resolve
// through the same config.Load path a hand-written one does.
func TestPatchSymbolOverrideRoundTripsThroughLoad(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	base, err := os.ReadFile("../../data/config.example.yaml")
	if err != nil {
		t.Skipf("example config unavailable: %v", err)
	}

	if err := os.WriteFile(file, base, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Load(file); err != nil {
		t.Fatalf("load before: %v", err)
	}

	err = PatchSymbolOverride(file, "MSFT", SymbolOverrideFields{
		Detectors: []string{"ema_cross", "rsi", "macd"}, Combine: "unanimous", MinStrength: ptrF(0.20),
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	after, err := Load(file)
	if err != nil {
		t.Fatalf("the patched config no longer loads: %v", err)
	}

	if !after.HasOverride("MSFT") {
		t.Fatal("MSFT has no resolved override after PatchSymbolOverride + Load")
	}

	msft := after.PerSymbolStrategy("MSFT")
	if msft.Combine != "unanimous" || msft.MinStrength != 0.20 || len(msft.Detectors) != 3 {
		t.Errorf("PerSymbolStrategy(\"MSFT\") = %+v, want the fields just written", msft)
	}

	// Inherited, not just the fields named — the whole point of
	// strategy_overrides' layer-onto-a-copy-of-base semantics.
	if msft.RSIPeriod != after.Strategy.RSIPeriod {
		t.Errorf("RSIPeriod = %v, want inherited from the shared base (%v)",
			msft.RSIPeriod, after.Strategy.RSIPeriod)
	}
}

// TestPatchSymbolOverrideCanClearSellSideThroughAFileWrite is the
// AT-4 finding's end-to-end regression: a search that decides a symbol's
// sell side should revert to "same as buy" (internal/backtest/refine.go's
// sellSide axis offers this as an explicit candidate) must actually write
// that, not silently keep the symbol's existing detectors_sell/
// combine_sell because a zero-valued []string/string was indistinguishable
// from "field not set" and got dropped by omitempty.
func TestPatchSymbolOverrideCanClearSellSideThroughAFileWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")

	seed := `strategy_overrides:
  MSFT:
    detectors: [ema_cross]
    combine: majority
    detectors_sell: [rsi, macd]
    combine_sell: weighted
`
	if err := os.WriteFile(file, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	emptyDS := []string{}
	emptyCS := ""

	err := PatchSymbolOverride(file, "MSFT", SymbolOverrideFields{
		Detectors: []string{"ema_cross"}, Combine: "majority",
		DetectorsSell: &emptyDS, CombineSell: &emptyCS,
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	after, err := Load(file)
	if err != nil {
		t.Fatalf("the patched config no longer loads: %v", err)
	}

	msft := after.PerSymbolStrategy("MSFT")
	if len(msft.DetectorsSell) != 0 {
		t.Errorf("DetectorsSell = %v, want cleared (same-as-buy) after the explicit revert", msft.DetectorsSell)
	}

	if msft.CombineSell != "" {
		t.Errorf("CombineSell = %q, want cleared (same-as-buy) after the explicit revert", msft.CombineSell)
	}
}

// TestPatchSymbolOverrideReusesADifferentlyCasedBlock reproduces the EN-2
// finding: autotune passes the raw watchlist symbol, while
// resolveSymbolStrategies upper-cases every key as it builds its map. Writing
// "aapl" against a file that already has "AAPL:" produced two YAML keys that
// collapse to one symbol, and load then resolved whichever map iteration
// visited last — measured at 171/29 across 200 loads, so which config a symbol
// traded under changed between restarts.
func TestPatchSymbolOverrideReusesADifferentlyCasedBlock(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "config.yaml")

	seed := `strategy:
  fast_ma: 12
  slow_ma: 26
  min_strength: 0.10
strategy_overrides:
  AAPL:
    min_strength: 0.90
`
	if err := os.WriteFile(file, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PatchSymbolOverride(file, "aapl", SymbolOverrideFields{
		Detectors: []string{"rsi"}, Combine: "majority", MinStrength: ptrF(0.10),
	}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	out, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(out), "\n  aapl:") {
		t.Errorf("a second, lower-cased block was created:\n%s", out)
	}

	// Deterministic across repeated loads, because there is only one block.
	for range 50 {
		cfg, err := Load(file)
		if err != nil {
			t.Fatalf("load: %v", err)
		}

		if got := cfg.PerSymbolStrategy("AAPL").MinStrength; got != 0.10 {
			t.Fatalf("min_strength = %v, want the 0.10 just written", got)
		}
	}
}

// TestDuplicateSymbolOverrideKeysAreRejected covers a file that already has the
// ambiguity (hand-written, or written by an older build): resolving it by map
// iteration order silently picked one, so refuse instead — an operator is the
// only one who can say which block they meant.
func TestDuplicateSymbolOverrideKeysAreRejected(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "config.yaml")

	seed := `strategy:
  fast_ma: 12
  slow_ma: 26
strategy_overrides:
  AAPL:
    min_strength: 0.90
  aapl:
    min_strength: 0.10
`
	if err := os.WriteFile(file, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(file)
	if err == nil {
		t.Fatal("a config with two entries for one symbol loaded without complaint")
	}

	if !strings.Contains(err.Error(), "two entries for AAPL") {
		t.Errorf("error = %v, want it to name the ambiguous symbol and both spellings", err)
	}
}
