package api

import (
	"errors"
	"fmt"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	mlpkg "github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/gin-gonic/gin"
)

// Writing an applied settings change back to config.yaml.
//
// Persistence deliberately reads the value back from the ENGINE rather than
// reusing the submitted form: the engine is what is actually running, and it
// normalises what it was given (defaults filled in, durations resolved). Writing
// the submission instead would let the file and the process disagree in ways
// nobody would notice until a restart.

// sectionTarget maps a settings form to the YAML block it belongs in.
//
// The two are mostly the same word, with one group of exceptions: the entries,
// sizing and behaviour forms are three views onto one `engine:` block. Nothing
// in the form names says so, which is exactly why it is written down here rather
// than inferred.
func sectionTarget(name string) string {
	switch name {
	case "entries", "sizing", "behavior":
		return "engine"
	default:
		return name
	}
}

// liveSection returns the running configuration for a settings form, and
// whether that form has anything to persist.
func (s *Server) liveSection(name string) (any, bool) {
	if v, ok := s.liveEngineSection(name); ok {
		return v, true
	}

	switch name {
	case "screener":
		return s.eng.ScreenerConfig(), true
	case "autotune":
		return s.eng.AutotuneConfig(), true
	case "test_mode":
		return s.eng.TestModeConfig(), true
	case "notify":
		return s.eng.NotifyConfig(), true
	case "ai":
		return s.aiConfig(), true
	case "ml":
		return mlpkg.Default().Config(), true
	case "strategy":
		return s.eng.StrategyConfig(), true
	case "limits":
		return s.rm.Limits(), true
	case "portfolio":
		// The sector and asset-class maps are edited separately.
		cfg, _, _ := s.eng.PortfolioConfig()

		return cfg, true
	}

	return nil, false
}

// liveEngineSection covers the forms whose values the engine owns directly.
func (s *Server) liveEngineSection(name string) (any, bool) {
	switch name {
	case "exits":
		return s.eng.ExitsConfig(), true
	case "costs":
		return s.eng.CostsConfig(), true
	case "protections":
		return s.eng.ProtectionsConfig(), true
	case "entries", "sizing", "behavior":
		return s.eng.EngineConfig(), true
	}

	return nil, false
}

// persistSection writes a form's live configuration back to config.yaml.
//
// It runs after the change has been applied and validated, so the file can only
// receive settings the engine already accepted. A failure does NOT undo the live
// change — the bot is running the new values either way — so the caller reports
// "applied, but not saved" rather than pretending one of the two did not happen.
func (s *Server) persistSection(name string) error {
	if s.configPath == "" {
		return nil
	}

	value, ok := s.liveSection(name)
	if !ok {
		return nil
	}

	var errs []error

	if err := s.persist(sectionTarget(name), value); err != nil {
		errs = append(errs, fmt.Errorf("applied, but not saved to config.yaml: %w", err))
	}

	// The portfolio form edits the hand-written sector and asset-class maps
	// too, and nothing wrote them: a correction applied live, the form said
	// "saved", and a restart reverted it (AP3-3). The hand layer only, never
	// the merged view — see Engine.PortfolioOverrides (SG2-2).
	if name == "portfolio" {
		sectors, classes := s.eng.PortfolioOverrides()

		// Their own blocks, written whatever became of the portfolio: one.
		for _, key := range []string{"sectors", "asset_classes"} {
			m := sectors
			if key == "asset_classes" {
				m = classes
			}

			if err := config.PatchLabelMap(s.configPath, key, m); err != nil {
				errs = append(errs,
					fmt.Errorf("applied, but %s not saved to config.yaml: %w", key, err))
			}
		}
	}

	return errors.Join(errs...)
}

// noteSave persists a form's section and, on failure, attaches the reason to
// the response as a header AND returns it, so the caller can also fold it
// into its own JSON body.
//
// The header exists because these handlers each return their own shaped
// JSON, and reshaping every response so a rare warning has somewhere to
// live would change the JSON API for every existing caller; the v2 form
// layer reads the header and shows it beside the Save button. But a header
// alone is invisible to any caller that only reads the body — which is
// exactly how a persist failure on the legacy sizing/behavior endpoints used
// to go unnoticed until the setting silently reverted on the next restart
// (see CODE_REVIEW_FINDINGS.md's "Patch permanently cannot save any section
// using ordinary YAML idioms"). Callers that have room for one more field
// should include the returned note in their body; it is "" on success.
func (s *Server) noteSave(c *gin.Context, name string) string {
	err := s.persistSection(name)
	if err == nil {
		return ""
	}

	c.Header(saveWarningHeader, err.Error())

	return err.Error()
}

// saveWarningHeader carries "applied, but not saved" back to the UI.
const saveWarningHeader = "X-Config-Save-Warning"
