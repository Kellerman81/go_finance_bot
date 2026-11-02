package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
	"github.com/gin-gonic/gin"
)

// Settings is every live-editable configuration block in one response, so the
// UI can render the whole surface from a single round-trip.
//
// The blocks the web interface previously exposed — strategy, exits, sizing,
// behaviour, limits — keep their own endpoints. These are the ones that had no
// way to be seen or changed without editing the file and restarting.
type Settings struct {
	Costs       config.CostsConfig     `json:"costs"`
	Exits       exitsWire              `json:"exits"`
	Entries     engine.EntryPolicy     `json:"entries"`
	Protections protectionsWire        `json:"protections"`
	Portfolio   config.PortfolioConfig `json:"portfolio"`
	Sectors     map[string]string      `json:"sectors"`
	// AssetClasses is the live symbol -> asset class map the allocation caps
	// bucket positions by, mostly auto-classified. Served read-write so the
	// UI can both show why a position counts against a class and let the
	// operator correct a misclassification.
	AssetClasses map[string]string `json:"asset_classes"`
	Screener     screenerWire      `json:"screener"`
	Autotune     autotuneWire      `json:"autotune"`
	TestMode     testModeWire      `json:"test_mode"`
	Notify       notifyWire        `json:"notify"`
	AI           aiWire            `json:"ai"`
	ML           config.MLConfig   `json:"ml"`

	// ScreenerFilters is every filter name the chain accepts, so the UI offers
	// the real set rather than a hard-coded copy that drifts from it.
	ScreenerFilters []string `json:"screener_filters"`

	// ScreenerSuggestions are the built-in starter candidate pools per asset
	// class, served so the UI can answer "what should I put in the universe?"
	// rather than leaving an operator staring at an empty list. They are a
	// starting point, not a recommendation: nothing in them bypasses a filter.
	ScreenerSuggestions map[string][]string `json:"screener_suggestions"`
}

// ----- duration wire format -----
//
// encoding/json renders a time.Duration as an integer count of nanoseconds and
// will not parse "30m" back. That is unusable in a form: nobody types
// 1800000000000. The duration fields are therefore tagged json:"-" in config and
// re-declared here as the string form Go itself prints and parses, which is also
// exactly what the YAML file contains.

// durStr renders a duration the way config.yaml writes it.
func durStr(d time.Duration) string { return d.String() }

// parseDur reads a duration string, treating empty as zero.
func parseDur(field, s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}

	if d < 0 {
		return 0, fmt.Errorf("%s cannot be negative", field)
	}

	return d, nil
}

// protectionsWire is ProtectionsConfig with its durations as strings.
type protectionsWire struct {
	config.ProtectionsConfig

	CooldownAfterLoss string `json:"cooldown_after_loss"`
	StopGuardLookback string `json:"stop_guard_lookback"`
	StopGuardLock     string `json:"stop_guard_lock"`
	LowProfitLookback string `json:"low_profit_lookback"`
	LowProfitLock     string `json:"low_profit_lock"`
}

// wrapProtections converts to the wire form.
func wrapProtections(c config.ProtectionsConfig) protectionsWire {
	return protectionsWire{
		ProtectionsConfig: c,
		CooldownAfterLoss: durStr(c.CooldownAfterLoss),
		StopGuardLookback: durStr(c.StopGuardLookback),
		StopGuardLock:     durStr(c.StopGuardLock),
		LowProfitLookback: durStr(c.LowProfitLookback),
		LowProfitLock:     durStr(c.LowProfitLock),
	}
}

// unwrap converts back, rejecting unparseable durations.
func (w protectionsWire) unwrap() (config.ProtectionsConfig, error) {
	out := w.ProtectionsConfig

	var err error

	out.CooldownAfterLoss, err = parseDur("cooldown_after_loss", w.CooldownAfterLoss)
	if err != nil {
		return out, err
	}

	out.StopGuardLookback, err = parseDur("stop_guard_lookback", w.StopGuardLookback)
	if err != nil {
		return out, err
	}

	out.StopGuardLock, err = parseDur("stop_guard_lock", w.StopGuardLock)
	if err != nil {
		return out, err
	}

	out.LowProfitLookback, err = parseDur("low_profit_lookback", w.LowProfitLookback)
	if err != nil {
		return out, err
	}

	out.LowProfitLock, err = parseDur("low_profit_lock", w.LowProfitLock)

	return out, err
}

// exitsWire is ExitsConfig with its max-hold duration as a string.
//
// It EMBEDS ExitsConfig and shadows only max_hold, the screenerWire pattern.
// It used to restate the fields one by one, and when bypass_trading_halt was
// added to ExitsConfig and to the form it never reached this list: the form
// always read "No", and every exits save turned the setting off in the live
// engine and in config.yaml — an operator who wanted stops to keep firing
// through a drawdown halt lost that on any exits edit (AP2-1). A field added to
// ExitsConfig now reaches the wire with nothing to forget.
type exitsWire struct {
	config.ExitsConfig

	MaxHold string `json:"max_hold"`
}

// wrapExits converts to the wire form.
func wrapExits(c config.ExitsConfig) exitsWire {
	return exitsWire{ExitsConfig: c, MaxHold: durStr(c.MaxHold)}
}

// unwrap converts back, rejecting an unparseable max_hold.
func (w exitsWire) unwrap() (config.ExitsConfig, error) {
	out := w.ExitsConfig

	var err error

	out.MaxHold, err = parseDur("max_hold", w.MaxHold)

	return out, err
}

// screenerWire is ScreenerConfig with its interval as a string.
type screenerWire struct {
	config.ScreenerConfig

	Interval string `json:"interval"`
}

// wrapScreener converts to the wire form.
func wrapScreener(c config.ScreenerConfig) screenerWire {
	return screenerWire{ScreenerConfig: c, Interval: durStr(c.Interval)}
}

// unwrap converts back, rejecting an unparseable interval.
func (w screenerWire) unwrap() (config.ScreenerConfig, error) {
	out := w.ScreenerConfig

	var err error

	out.Interval, err = parseDur("interval", w.Interval)

	return out, err
}

// autotuneWire is AutotuneConfig with its durations as strings. Both Interval
// and the nested on_loss.cooldown shadow the embedded struct's own fields,
// which is why the embedded config tags them json:"-".
type autotuneWire struct {
	config.AutotuneConfig

	Interval string             `json:"interval"`
	OnLoss   autotuneOnLossWire `json:"on_loss"`
}

// autotuneOnLossWire is AutotuneOnLossConfig with its cooldown as a string.
type autotuneOnLossWire struct {
	config.AutotuneOnLossConfig

	Cooldown string `json:"cooldown"`
}

// wrapAutotune converts to the wire form.
func wrapAutotune(c config.AutotuneConfig) autotuneWire {
	return autotuneWire{
		AutotuneConfig: c,
		Interval:       durStr(c.Interval),
		OnLoss: autotuneOnLossWire{
			AutotuneOnLossConfig: c.OnLoss,
			Cooldown:             durStr(c.OnLoss.Cooldown),
		},
	}
}

// unwrap converts back, rejecting an unparseable interval or cooldown.
func (w autotuneWire) unwrap() (config.AutotuneConfig, error) {
	out := w.AutotuneConfig

	var err error

	if out.Interval, err = parseDur("interval", w.Interval); err != nil {
		return out, err
	}

	// The embedded AutotuneConfig carried whatever OnLoss the client sent
	// only if it also sent the flat form; the shadowing field above is what
	// actually decodes, so rebuild the block from it.
	out.OnLoss = w.OnLoss.AutotuneOnLossConfig

	out.OnLoss.Cooldown, err = parseDur("on_loss.cooldown", w.OnLoss.Cooldown)

	return out, err
}

// testModeWire is TestModeConfig with its duration as a string.
type testModeWire struct {
	config.TestModeConfig

	Duration string `json:"duration"`
}

// wrapTestMode converts to the wire form.
func wrapTestMode(c config.TestModeConfig) testModeWire {
	return testModeWire{TestModeConfig: c, Duration: durStr(c.Duration)}
}

// unwrap converts back, rejecting an unparseable duration.
func (w testModeWire) unwrap() (config.TestModeConfig, error) {
	out := w.TestModeConfig

	var err error

	out.Duration, err = parseDur("duration", w.Duration)

	return out, err
}

// notifyWire is NotifyConfig with its timeout as a string.
type notifyWire struct {
	config.NotifyConfig

	Timeout string `json:"timeout"`
}

// wrapNotify converts to the wire form.
func wrapNotify(c config.NotifyConfig) notifyWire {
	return notifyWire{NotifyConfig: c, Timeout: durStr(c.Timeout)}
}

// unwrap converts back, rejecting an unparseable timeout.
func (w notifyWire) unwrap() (config.NotifyConfig, error) {
	out := w.NotifyConfig

	var err error

	out.Timeout, err = parseDur("timeout", w.Timeout)

	return out, err
}

// aiWire is AIConfig with its durations as strings.
type aiWire struct {
	config.AIConfig

	Timeout         string `json:"timeout"`
	RefreshInterval string `json:"refresh_interval"`
	MaxAge          string `json:"max_age"`
}

// wrapAI converts to the wire form.
func wrapAI(c config.AIConfig) aiWire {
	return aiWire{
		AIConfig:        c,
		Timeout:         durStr(c.Timeout),
		RefreshInterval: durStr(c.RefreshInterval),
		MaxAge:          durStr(c.MaxAge),
	}
}

// unwrap converts back, rejecting unparseable durations.
func (w aiWire) unwrap() (config.AIConfig, error) {
	out := w.AIConfig

	var err error

	if out.Timeout, err = parseDur("timeout", w.Timeout); err != nil {
		return out, err
	}

	if out.RefreshInterval, err = parseDur("refresh_interval", w.RefreshInterval); err != nil {
		return out, err
	}

	out.MaxAge, err = parseDur("max_age", w.MaxAge)

	return out, err
}

// getSettings serves GET /api/settings: every live-editable block.
//
// Secrets are redacted. The API is reachable by anything that can talk to the
// port, and an unauthenticated GET that hands back your Pushover token, Telegram
// bot token and LLM API key would turn a read-only endpoint into a credential
// leak. A redacted field submitted back unchanged keeps its stored value.
func (s *Server) getSettings(c *gin.Context) {
	port, sectors, assetClasses := s.eng.PortfolioConfig()

	c.JSON(http.StatusOK, Settings{
		Costs:               s.eng.CostsConfig(),
		Exits:               wrapExits(s.eng.ExitsConfig()),
		Entries:             s.eng.EntryPolicy(),
		Protections:         wrapProtections(s.eng.ProtectionsConfig()),
		Portfolio:           port,
		Sectors:             sectors,
		AssetClasses:        assetClasses,
		Screener:            wrapScreener(s.eng.ScreenerConfig()),
		Autotune:            wrapAutotune(s.eng.AutotuneConfig()),
		TestMode:            wrapTestMode(s.eng.TestModeConfig()),
		Notify:              wrapNotify(redactNotify(s.eng.NotifyConfig())),
		AI:                  wrapAI(redactAI(s.aiConfig())),
		ML:                  s.mlConfig(),
		ScreenerFilters:     screener.FilterNames(),
		ScreenerSuggestions: screener.Suggestions(),
	})
}

// redactedMarker is what a stored secret reads as over the wire. Submitting it
// back unchanged means "keep what you have".
const redactedMarker = "********"

// redactNotify blanks the credentials in an alert configuration.
func redactNotify(n config.NotifyConfig) config.NotifyConfig {
	n.Webhook = redact(n.Webhook)
	n.DiscordWebhook = redact(n.DiscordWebhook)
	n.TelegramToken = redact(n.TelegramToken)
	n.PushoverToken = redact(n.PushoverToken)
	n.PushoverUser = redact(n.PushoverUser)
	n.AppriseServices = redact(n.AppriseServices)

	return n
}

// redactAI blanks the model provider's API key.
func redactAI(a config.AIConfig) config.AIConfig {
	a.APIKey = redact(a.APIKey)

	return a
}

// redact replaces a non-empty secret with the marker.
func redact(v string) string {
	if v == "" {
		return ""
	}

	return redactedMarker
}

// unredact restores a stored secret when the submitted value is the marker, so
// saving a form that never displayed the secret cannot erase it.
func unredact(submitted, stored string) string {
	if submitted == redactedMarker {
		return stored
	}

	return submitted
}

// presentJSONKeys reads c's already-buffered request body (via
// ShouldBindBodyWithJSON, which callers must use instead of ShouldBindJSON
// so the body can be read a second time here) and reports which top-level
// JSON keys the client actually sent.
//
// A plain string field can't tell "the client never mentioned this field"
// apart from "the client submitted an empty string" — both unmarshal to ""
// — so an ordinary partial update like PUT /api/settings/notify
// {"enabled":false} used to wipe every secret field it didn't mention,
// permanently and with no copy to restore from (the client only ever saw
// the redacted marker). See CODE_REVIEW_FINDINGS.md's settings-redaction
// finding.
func presentJSONKeys(c *gin.Context) (map[string]bool, error) {
	var raw map[string]json.RawMessage
	if err := c.ShouldBindBodyWithJSON(&raw); err != nil {
		return nil, err
	}

	present := make(map[string]bool, len(raw))
	for k := range raw {
		present[k] = true
	}

	return present, nil
}

// bindMerged binds the JSON body ONTO dst, which the caller has filled with the
// settings currently in force, so a key the body does not mention keeps its
// live value instead of being reset to zero.
//
// Binding into an empty struct is what let a form wipe every setting it does
// not declare: the strategy form has no htf_* fields, so saving it — unchanged —
// switched the higher-timeframe gate off, live and in config.yaml (AP2-2), and
// the screener form dropped by_asset_class/universe_by_asset_class from the
// running engine and wrote probe.bars as 0 (AP2-3). A map the body DOES mention
// is replaced rather than merged into — json would otherwise keep keys the
// body removed, and deleting one would silently not happen.
//
// dst is deep-copied first. The live config getters return shallow copies
// whose slices, maps and pointers are the engine's own, and encoding/json
// decodes a JSON array into an existing backing array in place: a REJECTED
// save would otherwise already have rewritten the running engine's scale-out
// ladder or detector list, with no lock held while it did.
func bindMerged(c *gin.Context, dst any) error {
	var raw map[string]json.RawMessage

	if err := c.ShouldBindBodyWithJSON(&raw); err != nil {
		return err
	}

	v := reflect.ValueOf(dst).Elem()
	v.Set(deepCopy(v))

	clearPresentMaps(v, raw)

	return c.ShouldBindBodyWithJSON(dst)
}

// deepCopy returns a copy of v that shares no slice, map or pointer target
// with it. Unexported struct fields are copied by value.
func deepCopy(v reflect.Value) reflect.Value {
	//nolint:exhaustive // only the reference-holding kinds need copying; the rest are values
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}

		n := reflect.New(v.Elem().Type())
		n.Elem().Set(deepCopy(v.Elem()))

		return n

	case reflect.Slice:
		return copySlice(v)

	case reflect.Map:
		return copyMap(v)

	case reflect.Struct:
		return copyStruct(v)

	default:
		return v
	}
}

// copySlice deep-copies a slice for deepCopy.
func copySlice(v reflect.Value) reflect.Value {
	if v.IsNil() {
		return v
	}

	n := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
	for i := range v.Len() {
		n.Index(i).Set(deepCopy(v.Index(i)))
	}

	return n
}

// copyMap deep-copies a map's values for deepCopy (keys are values already).
func copyMap(v reflect.Value) reflect.Value {
	if v.IsNil() {
		return v
	}

	n := reflect.MakeMapWithSize(v.Type(), v.Len())

	for it := v.MapRange(); it.Next(); {
		n.SetMapIndex(it.Key(), deepCopy(it.Value()))
	}

	return n
}

// copyStruct deep-copies a struct's exported fields for deepCopy.
func copyStruct(v reflect.Value) reflect.Value {
	n := reflect.New(v.Type()).Elem()
	n.Set(v)

	for i := range v.NumField() {
		if n.Field(i).CanSet() {
			n.Field(i).Set(deepCopy(v.Field(i)))
		}
	}

	return n
}

// clearPresentMaps zeroes every map field of the struct v whose json key is in
// present, descending into embedded structs (whose fields json promotes).
func clearPresentMaps(v reflect.Value, present map[string]json.RawMessage) {
	t := v.Type()

	for i := range t.NumField() {
		f := t.Field(i)

		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			clearPresentMaps(v.Field(i), present)

			continue
		}

		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}

		if _, ok := present[name]; ok && f.Type.Kind() == reflect.Map && v.Field(i).CanSet() {
			v.Field(i).Set(reflect.Zero(f.Type))
		}
	}
}

// unredactOmitted is unredact, but first checks whether the client's request
// mentioned this field at all: if not, the stored value is kept untouched
// regardless of what the (zero-valued) bound struct field says. See
// presentJSONKeys.
func unredactOmitted(present bool, submitted, stored string) string {
	if !present {
		return stored
	}

	return unredact(submitted, stored)
}

// putSettings serves PUT /api/settings/:section: applies one block live.
//
// Sections are applied individually rather than as one document so a malformed
// or rejected block cannot partially apply its neighbours.
//
//nolint:cyclop // one case per configuration section; a flat dispatch
func (s *Server) putSettings(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	section := strings.ToLower(strings.TrimSpace(c.Param("section")))

	var (
		note string
		err  error
	)

	switch section {
	case "costs":
		err = s.applyCosts(c)
	case "exits":
		err = s.applyExits(c)
	case "entries":
		err = s.applyEntries(c)
	case "protections":
		err = s.applyProtections(c)
	case "portfolio":
		err = s.applyPortfolio(c)
	case "screener":
		note, err = s.applyScreener(c)
	case "autotune":
		note, err = s.applyAutotune(c)
	case "test_mode":
		err = s.applyTestMode(c)
	case "notify":
		err = s.applyNotify(c)
	case "ai":
		err = s.applyAI(c)
	case "ml":
		err = s.applyML(c)
	default:
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("unknown settings section %q", section),
			"sections": []string{
				"costs", "exits", "entries", "protections", "portfolio",
				"screener", "autotune", "test_mode", "notify", "ai", "ml",
			},
		})

		return
	}

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if perr := s.persistSection(section); perr != nil {
		note = joinNote(note, perr.Error())
	}

	c.JSON(http.StatusOK, gin.H{"applied": section, "note": note})
}

// joinNote appends a second remark to a note, either of which may be empty.
func joinNote(note, extra string) string {
	switch {
	case note == "":
		return extra
	case extra == "":
		return note
	default:
		return note + " — " + extra
	}
}

// applyCosts validates and applies the commission and friction model.
func (s *Server) applyCosts(c *gin.Context) error {
	var in config.CostsConfig

	if err := c.ShouldBindJSON(&in); err != nil {
		return err
	}

	if err := in.Validate(); err != nil {
		return err
	}

	s.eng.SetCostsConfig(in)

	return nil
}

// applyExits validates and applies the protective exit rules.
func (s *Server) applyExits(c *gin.Context) error {
	w := wrapExits(s.eng.ExitsConfig())

	if err := bindMerged(c, &w); err != nil {
		return err
	}

	in, err := w.unwrap()
	if err != nil {
		return err
	}

	// The same check config.Load runs: a ladder that sells more than the whole
	// position is a typo nothing else would catch.
	if err := in.Validate(); err != nil {
		return err
	}

	s.eng.SetExitsConfig(in)

	return nil
}

// applyEntries validates and applies the entry rules (scale-in ladder,
// pyramiding, and whether to evaluate symbols that cannot be bought).
func (s *Server) applyEntries(c *gin.Context) error {
	var in engine.EntryPolicy

	if err := c.ShouldBindJSON(&in); err != nil {
		return err
	}

	// Averaging down is only survivable with the cap and the size limit, so the
	// API enforces them exactly as the config loader does.
	if err := in.ScaleIn.Validate(); err != nil {
		return err
	}

	s.eng.SetEntryPolicy(in)

	return nil
}

// applyProtections validates and applies the protection thresholds.
func (s *Server) applyProtections(c *gin.Context) error {
	var w protectionsWire

	if err := c.ShouldBindJSON(&w); err != nil {
		return err
	}

	in, err := w.unwrap()
	if err != nil {
		return err
	}

	// The same rules config.Load runs — this used to check two of the three
	// limits and none of the guards (PR2-4/PR2-5).
	if err := in.Validate(); err != nil {
		return err
	}

	s.eng.SetProtectionsConfig(in)

	return nil
}

// applyPortfolio validates and applies the concentration caps.
func (s *Server) applyPortfolio(c *gin.Context) error {
	var in struct {
		config.PortfolioConfig

		Sectors      map[string]string `json:"sectors"`
		AssetClasses map[string]string `json:"asset_classes"`
	}

	if err := c.ShouldBindJSON(&in); err != nil {
		return err
	}

	if err := in.PortfolioConfig.Validate(); err != nil {
		return err
	}

	// The cross-section rule the loader applies, before anything is applied
	// (AP3-5): see config.ValidatePositionSlots.
	minPos, _, _ := s.eng.Sizing()
	if err := config.ValidatePositionSlots(in.PortfolioConfig, minPos); err != nil {
		return err
	}

	// Absent means "leave the map alone"; an explicit {} clears it. Both maps
	// are read from one call so a request that sends neither cannot pick up a
	// half-updated pair from a concurrent write.
	// The hand-written layer, not the merged view: PortfolioConfig also
	// returns what the bot learned, and handing that back here made every
	// learned entry a hand-written one (SG2-2).
	sectors, assetClasses := s.eng.PortfolioOverrides()

	if in.Sectors != nil {
		sectors = in.Sectors
	}

	if in.AssetClasses != nil {
		assetClasses = normalizeAssetClasses(in.AssetClasses)
	}

	s.eng.SetPortfolioConfig(in.PortfolioConfig, sectors, assetClasses)

	return nil
}

// normalizeAssetClasses maps hand-entered instrument types onto the canonical
// class names the caps are keyed by, so an operator correcting a symbol can
// type "ETF" (or Saxo's "Etc") and have it land in the bucket the cap names.
func normalizeAssetClasses(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))

	for sym, class := range in {
		if c := market.NormalizeAssetClass(class); c != "" {
			out[strings.ToUpper(strings.TrimSpace(sym))] = c
		}
	}

	return out
}

// applyScreener applies the universe filters, reporting that a changed interval
// needs a restart rather than silently ignoring it.
func (s *Server) applyScreener(c *gin.Context) (string, error) {
	w := wrapScreener(s.eng.ScreenerConfig())

	if err := bindMerged(c, &w); err != nil {
		return "", err
	}

	in, err := w.unwrap()
	if err != nil {
		return "", err
	}

	if err := in.Validate(); err != nil {
		return "", err
	}

	before := s.eng.ScreenerConfig()
	s.eng.SetScreenerConfig(in)

	if in.Enabled && in.Interval != before.Interval {
		return "filters applied; the changed interval takes effect after a restart", nil
	}

	return "", nil
}

// applyAutotune applies the scheduled auto-optimizer settings, reporting
// that a changed interval — or enabling the job for the first time — needs
// a restart rather than silently ignoring it. See
// Engine.SetAutotuneConfig's doc comment for exactly which fields take
// effect live vs. need that restart.
func (s *Server) applyAutotune(c *gin.Context) (string, error) {
	var w autotuneWire

	if err := c.ShouldBindJSON(&w); err != nil {
		return "", err
	}

	in, err := w.unwrap()
	if err != nil {
		return "", err
	}

	if err := in.Validate(); err != nil {
		return "", err
	}

	before := s.eng.AutotuneConfig()
	s.eng.SetAutotuneConfig(in)

	if in.Enabled && (!before.Enabled || in.Interval != before.Interval) {
		return "saved; a newly enabled job or a changed interval takes effect after a restart", nil
	}

	return "", nil
}

// applyTestMode saves the configured default (enabled/duration) used at the
// next process start. It does not itself start or stop a currently active
// training window — see POST /api/testmode/start and /stop for that.
func (s *Server) applyTestMode(c *gin.Context) error {
	var w testModeWire

	if err := c.ShouldBindJSON(&w); err != nil {
		return err
	}

	in, err := w.unwrap()
	if err != nil {
		return err
	}

	if err := in.Validate(); err != nil {
		return err
	}

	s.eng.SetTestModeConfig(in)

	return nil
}

// applyNotify applies the alert settings, restoring any secret the client sent
// back redacted.
func (s *Server) applyNotify(c *gin.Context) error {
	var w notifyWire

	if err := c.ShouldBindBodyWithJSON(&w); err != nil {
		return err
	}

	present, err := presentJSONKeys(c)
	if err != nil {
		return err
	}

	in, err := w.unwrap()
	if err != nil {
		return err
	}

	cur := s.eng.NotifyConfig()

	in.Webhook = unredactOmitted(present["webhook"], in.Webhook, cur.Webhook)
	in.DiscordWebhook = unredactOmitted(present["discord_webhook"], in.DiscordWebhook, cur.DiscordWebhook)
	in.TelegramToken = unredactOmitted(present["telegram_token"], in.TelegramToken, cur.TelegramToken)
	in.PushoverToken = unredactOmitted(present["pushover_token"], in.PushoverToken, cur.PushoverToken)
	in.PushoverUser = unredactOmitted(present["pushover_user"], in.PushoverUser, cur.PushoverUser)
	in.AppriseServices = unredactOmitted(present["apprise_services"], in.AppriseServices, cur.AppriseServices)

	if err := in.Validate(); err != nil {
		return err
	}

	s.eng.SetNotifyConfig(in)

	return nil
}

// applyAI applies the LLM advisor settings and rebuilds the strategy.
//
// The rebuild is required, not cosmetic: the "ai" detector captures the advisor
// when the strategy is constructed, so without it enabling the advisor would
// appear to do nothing until a restart.
func (s *Server) applyAI(c *gin.Context) error {
	var w aiWire

	if err := c.ShouldBindBodyWithJSON(&w); err != nil {
		return err
	}

	present, err := presentJSONKeys(c)
	if err != nil {
		return err
	}

	in, err := w.unwrap()
	if err != nil {
		return err
	}

	in.APIKey = unredactOmitted(present["api_key"], in.APIKey, s.aiConfig().APIKey)

	if err := in.Validate(); err != nil {
		return err
	}

	if err := ai.Configure(in); err != nil {
		return err
	}

	s.eng.SetStrategyConfig(s.eng.StrategyConfig())

	return nil
}

// applyML applies the learned-detector settings and rebuilds the strategy.
//
// Reconfiguring discards every trained model, which is correct: a change to the
// horizon or the label threshold makes the existing models answers to a
// different question.
func (s *Server) applyML(c *gin.Context) error {
	var in config.MLConfig

	if err := c.ShouldBindJSON(&in); err != nil {
		return err
	}

	if err := in.Validate(); err != nil {
		return err
	}

	prev := ml.Default().Config()
	params := ml.ParamsFor(s.eng.StrategyConfig())

	ml.Configure(in, params)
	s.eng.RebuildStrategies() // overrides too: they bind the predictor at build (ML3-4)

	// A train_bars past ~2,830 (default periods) needs more history than the engine keeps: the
	// whole live strategy would vote HOLD forever, every detector of every
	// symbol, while a backtest kept trading (ML3-3). Rolled back, so the
	// rejected save is wholly rejected.
	if need := s.eng.StrategyWarmup(); need > engine.MaxHistoryBars {
		ml.Configure(prev, params)
		s.eng.RebuildStrategies()

		return fmt.Errorf("ml: the strategy would need %d bars of warmup, but the engine keeps %d — "+
			"lower train_bars", need, engine.MaxHistoryBars)
	}

	return nil
}

// aiConfig returns the advisor's live settings, or the disabled zero value.
func (*Server) aiConfig() config.AIConfig { return ai.Config() }

// mlConfig returns the predictor's live settings, or the disabled zero value.
func (*Server) mlConfig() config.MLConfig { return ml.Default().Config() }
