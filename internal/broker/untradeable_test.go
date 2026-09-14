package broker

import "testing"

// The live rejection that prompted this, verbatim from the Trading 212 API.
const t212Invisible = `{"type":"/api-errors/instrument-invisible",` +
	`"title":"Error while placing the order","status":400,` +
	`"detail":"Instrument can not be traded.","traceId":"278a6d141eb8da9"}`

// A permanent instrument-level refusal must be recognisable, so the engine can
// stop re-sending an order the venue will never accept.
func TestInstrumentInvisibleIsPermanent(t *testing.T) {
	t.Parallel()

	if !instrumentUnavailable(400, []byte(t212Invisible)) {
		t.Error("the live instrument-invisible rejection was not classified as permanent")
	}
}

// Everything that clears by itself must NOT be, or one bad moment would disable
// a symbol for the rest of the process.
func TestTransientRefusalsAreNotPermanent(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"insufficient funds": {400, `{"code":"InsufficientFreeForStocks"}`},
		"below minimum":      {400, `{"code":"MinValueShares","detail":"Order value too small"}`},
		"not owned":          {400, `{"code":"SellingEquityNotOwned"}`},
		"rate limited":       {429, `{"detail":"too many requests"}`},
		"server error":       {500, `{"detail":"Instrument can not be traded."}`},
		"empty":              {400, ``},
	} {
		if instrumentUnavailable(tc.status, []byte(tc.body)) {
			t.Errorf("%s was classified as a permanent instrument refusal.\n"+
				"That would suspend entries for the symbol over a condition "+
				"that clears by itself.", name)
		}
	}
}
