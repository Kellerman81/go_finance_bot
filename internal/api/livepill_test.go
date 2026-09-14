package api

import (
	"net/http"
	"strings"
	"testing"
)

// The connection pill said "reconnecting…" permanently, whatever the stream was
// doing, for two compounding reasons:
//
//  1. setLive changed only the pill's CSS class. The label is server-rendered,
//     so it kept the text it was born with.
//  2. The pill sat inside #pills, a fragment re-fetched every 5 seconds. Even
//     the class was thrown away on every poll.
//
// Both have to stay fixed for the pill to mean anything.

// The client must set the pill's text, not just its colour.
func TestLivePillTextIsUpdatedByTheClient(t *testing.T) {
	t.Parallel()

	src := clientSource(t)

	start := strings.Index(src, "function setLive(")
	if start < 0 {
		t.Fatal("setLive is gone; this test needs rewriting")
	}

	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of setLive")
	}

	body := src[start : start+end]

	if !strings.Contains(body, "textContent") {
		t.Error("setLive does not set textContent.\nThe label is rendered by the " +
			"server as \"reconnecting…\", so without this the pill reports that " +
			"forever no matter how healthy the stream is.")
	}

	if !strings.Contains(body, "className") {
		t.Error("setLive no longer sets the colour")
	}
}

// The pill must not live inside a polled fragment, or the poll overwrites the
// state the stream handler just set.
func TestLivePillIsOutsideThePolledFragment(t *testing.T) {
	t.Parallel()

	layout, ok := v2Assets["templates/layout.html"]
	if !ok {
		t.Fatal("layout.html missing from the embedded tree")
	}

	views, ok := v2Assets["templates/views.html"]
	if !ok {
		t.Fatal("views.html missing from the embedded tree")
	}

	if !strings.Contains(string(layout.raw), `id="live-pill"`) {
		t.Error("the live pill is not in the shell; it has to be somewhere the " +
			"5-second pills poll cannot replace it")
	}

	if strings.Contains(string(views.raw), `id="live-pill"`) {
		t.Error("the live pill is back inside the pills fragment.\nThat fragment " +
			"is re-fetched every 5 seconds, which replaces the element and " +
			"discards the connection state with it.")
	}
}

// Both labels must reach the browser, in the active language, without shipping
// the catalogs — the client has no translator.
func TestLivePillCarriesBothLabels(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	rec := fetch(t, s, "/v2/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{`data-up="live"`, `data-down="reconnecting`} {
		if !strings.Contains(body, want) {
			t.Errorf("the shell does not carry %s; setLive has nothing to switch to", want)
		}
	}
}

// And in German, since that is the whole point of the data attributes.
func TestLivePillLabelsAreTranslated(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	rec := fetch(t, s, "/v2/?lang=de")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "Verbindung wird aufgebaut") {
		t.Error("the German down-label is missing from the shell")
	}
}
