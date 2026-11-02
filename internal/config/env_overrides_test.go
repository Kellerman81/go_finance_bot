package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// configFile writes body to a temp config.yaml and returns its path.
func configFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// TestEnvProfileDoesNotOverrideTheFile is the regression for CF2-2.
// BOT_PROFILE was applied in applyEnv, AFTER the file, so it overwrote what
// the file set explicitly: stop_loss_pct 0.02 loaded as the aggressive
// profile's 0.08. A profile seeds; the file wins.
//
// Not parallel: it sets the environment.
func TestEnvProfileDoesNotOverrideTheFile(t *testing.T) {
	t.Setenv("BOT_PROFILE", "aggressive")

	cfg, err := writeConfig(t, "exits:\n  stop_loss_pct: 0.02\n")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Exits.StopLossPct != 0.02 {
		t.Errorf("stop_loss_pct = %v under BOT_PROFILE=aggressive, want the file's 0.02", cfg.Exits.StopLossPct)
	}

	if cfg.Profile != "aggressive" {
		t.Errorf("Profile = %q, want aggressive", cfg.Profile)
	}
}

// TestMisspeltEnvProfileIsAnError is the regression for CF2-3: an unknown
// BOT_PROFILE was dropped before Validate could see it, so "agressive"
// loaded as the defaults with nothing said.
func TestMisspeltEnvProfileIsAnError(t *testing.T) {
	t.Setenv("BOT_PROFILE", "agressive")

	if _, err := Load(""); err == nil {
		t.Error("BOT_PROFILE=agressive loaded without an error")
	}
}

// TestEnvKillSwitchAcceptsOffAndRejectsNonsense is the regression for CF2-4:
// an env boolean strconv could not parse was ignored, so
// BOT_TRADING_ENABLED=off over a file with trading_enabled: true left trading
// ON — the kill switch failing open.
func TestEnvKillSwitchAcceptsOffAndRejectsNonsense(t *testing.T) {
	path := configFile(t, "engine:\n  trading_enabled: true\n")

	t.Setenv("BOT_TRADING_ENABLED", "off")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with BOT_TRADING_ENABLED=off: %v", err)
	}

	if cfg.Engine.TradingEnabled {
		t.Error("BOT_TRADING_ENABLED=off left trading enabled")
	}

	t.Setenv("BOT_TRADING_ENABLED", "maybe")

	if _, err := Load(path); err == nil {
		t.Error("BOT_TRADING_ENABLED=maybe loaded without an error")
	}
}

// CF3-1: only the env SECRETS were marked env-sourced, so a settings save of
// the engine section wrote BOT_TRADING_ENABLED's value into config.yaml as if
// the operator had written it — the kill switch silently moved from env to
// file — and an AI save wrote a temporary BOT_AI_BASE_URL over the file's.
func TestASaveDoesNotBakeEnvOverridesIntoTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "engine:\n  trading_enabled: false\n  min_positions: 4\n" +
		"ai:\n  base_url: \"http://ollama:11434\"\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BOT_TRADING_ENABLED", "true")
	t.Setenv("BOT_AI_BASE_URL", "http://ollama-temp:11434")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Engine.MinPositions = 5
	if err := Patch(path, "engine", cfg.Engine); err != nil {
		t.Fatal(err)
	}

	if err := Patch(path, "ai", cfg.AI); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if s := string(out); strings.Contains(s, "trading_enabled: true") || strings.Contains(s, "ollama-temp") {
		t.Errorf("a save wrote env overrides into the file:\n%s", s)
	}
}

// CF3-2: a save compared the live section with the file read into a ZERO
// struct, so every value the profile set and the file did not name looked
// changed, and the first exits save under profile: aggressive wrote the
// aggressive stop and trail into the file. Switching to conservative then
// moved every other section and left exits aggressive. A save must write only
// what differs from what the file already loads to.
func TestASaveDoesNotBakeTheProfileIntoTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("profile: aggressive\nexits:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := Patch(path, "exits", cfg.Exits); err != nil {
		t.Fatal(err)
	}

	out, _ := os.ReadFile(path)
	if strings.Contains(string(out), "stop_loss_pct") {
		t.Fatalf("an unchanged exits save wrote the profile's values into the file:\n%s", out)
	}

	switched := strings.Replace(string(out), "profile: aggressive", "profile: conservative", 1)
	if err := os.WriteFile(path, []byte(switched), 0o600); err != nil {
		t.Fatal(err)
	}

	after, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	want, _ := ApplyProfile(Default(), "conservative")
	if after.Exits.StopLossPct != want.Exits.StopLossPct {
		t.Errorf("after switching to conservative, stop_loss_pct = %v, want the profile's %v",
			after.Exits.StopLossPct, want.Exits.StopLossPct)
	}
}

// CF3-5: "1.5h" is a valid duration, but the patcher's dotted leaf path read
// roi.1.5h as roi → 1 → 5h, so that rung could never be saved and every later
// exits save failed "applied, but not saved". It is refused at load with the
// dot-free spelling named.
func TestADottedROIKeyIsRefused(t *testing.T) {
	t.Parallel()

	x := Default().Exits
	x.ROI = map[string]float64{"1.5h": 0.02}

	if err := x.Validate(); err == nil || !strings.Contains(err.Error(), "90m") {
		t.Errorf("Validate = %v, want the dotted key refused with the 90m spelling", err)
	}
}

// CF3-6: a section built with a merge key loads fine, but every save of it
// failed in verify with "cannot find anchor by alias name b" — the patcher's
// internals, not the file's problem. The refusal must name the alias.
func TestASectionWithAMergeKeyIsRefusedClearly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "base: &b\n  enabled: true\n  stop_loss_pct: 0.05\nexits:\n  <<: *b\n  take_profit_pct: 0.1\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	x := Default().Exits
	x.StopLossPct = 0.04

	err := Patch(path, "exits", x)
	if err == nil || !strings.Contains(err.Error(), "merge key") {
		t.Errorf("Patch = %v, want a refusal that names the alias/merge key", err)
	}

	if got, _ := os.ReadFile(path); string(got) != body {
		t.Errorf("the file changed:\n%s", got)
	}
}

// AI3-4: max_age shorter than refresh_interval was accepted, and every
// verdict then expired before the next arrived — HOLD for 5 minutes of every
// 15 with a 15m refresh and a 10m max_age, with nothing saying why.
func TestAnAIMaxAgeShorterThanTheRefreshIsRefused(t *testing.T) {
	t.Parallel()

	a := Default().AI
	a.Enabled = true
	a.RefreshInterval = 15 * time.Minute
	a.MaxAge = 10 * time.Minute

	if err := a.Validate(); err == nil {
		t.Error("max_age 10m under refresh_interval 15m was accepted")
	}

	// A refresh_interval of 0 runs as the 15m default: the same gap.
	a.RefreshInterval = 0
	if err := a.Validate(); err == nil {
		t.Error("max_age 10m under the default 15m refresh was accepted")
	}
}
