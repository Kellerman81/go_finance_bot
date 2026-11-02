package broker

import "testing"

// A broker names instruments its own way, and the bot must see its own names
// back. Trading 212 lists PRAJL.XC as "PR1Jd"; the reverse map that recovers it
// is filled only when a symbol is resolved, so a restart used to leave a
// long-held position reporting under the broker's name and matching nothing.
func TestSymbolForRecoversTheBotsName(t *testing.T) {
	t.Parallel()

	tr := &Trading212{
		resolved: map[string]string{},
		reverse:  map[string]string{},
	}

	// Before resolution the heuristic is all there is, and for a ticker with no
	// underscore it cannot recover anything.
	if got := tr.symbolFor("PR1Jd"); got == "PRAJL.XC" {
		t.Fatal("the test's premise is wrong: the heuristic already recovers this")
	}

	tr.remember("PRAJL.XC", "PR1Jd")

	if got := tr.symbolFor("PR1Jd"); got != "PRAJL.XC" {
		t.Errorf("symbolFor(PR1Jd) = %q, want PRAJL.XC once resolved", got)
	}
}

// The heuristic covers the common shape, which is why US tickers looked fine
// and this went unnoticed.
func TestSymbolFromTickerHandlesTheUnderscoreForm(t *testing.T) {
	t.Parallel()

	for ticker, want := range map[string]string{
		"MRVL_US_EQ": "MRVL",
		"NVDA_US_EQ": "NVDA",
		// No underscore: nothing to strip, so the bot's own name is unrecoverable
		// without the resolution table.
		"PR1Jd": "PR1Jd",
	} {
		if got := symbolFromTicker(ticker); got != want {
			t.Errorf("symbolFromTicker(%q) = %q, want %q", ticker, got, want)
		}
	}
}

// TestSuffixCountryMapsEveryVenue guards same-name listing disambiguation
// on Trading 212: a Yahoo suffix must map to the ISIN country of its venue,
// so "SAP.DE" picks the German listing and not the US ADR. A suffix that
// fell through to "" would let the first listing win — often the wrong
// currency and the wrong exchange hours.
func TestSuffixCountryMapsEveryVenue(t *testing.T) {
	t.Parallel()

	for suffix, want := range map[string]string{
		"DE": "DE", "F": "DE", "BE": "DE", "PA": "FR", "AS": "NL", "BR": "BE", "L": "GB", "IL": "GB",
		"MI": "IT", "MC": "ES", "SW": "CH", "VX": "CH", "ST": "SE", "HE": "FI", "OL": "NO",
		"CO": "DK", "LS": "PT", "VI": "AT", "IR": "IE", "": "US", "XC": "",
	} {
		if got := suffixCountry(suffix); got != want {
			t.Errorf("suffixCountry(%q) = %q, want %q", suffix, got, want)
		}
	}
}
