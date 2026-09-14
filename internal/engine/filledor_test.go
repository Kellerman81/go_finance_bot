package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// TestFilledOrDoesNotClaimFilledOnAConfirmedZero reproduces the "filledOr("")
// maps an empty broker status to 'filled', compounding CRIT-1/2's misleading
// audit trail" finding: a confirmed rejection/cancellation (settledQty's
// "confirmed zero") with no status string from the broker must not read as
// "filled" in the trade log — that combination (Status "filled", Qty 0) is
// nonsensical and exactly the misleading-audit-trail risk CRIT-1/2 already
// closed one layer up.
func TestFilledOrDoesNotClaimFilledOnAConfirmedZero(t *testing.T) {
	if got := filledOr("", 0); got == "filled" {
		t.Errorf(`filledOr("", 0) = %q, must not be "filled" — a confirmed zero fill is not a fill`, got)
	}

	if got := filledOr("", 0); got == "" {
		t.Error(`filledOr("", 0) returned an empty status, want a real (if generic) fallback`)
	}
}

// TestFilledOrStillDefaultsToFilledForARealFill confirms the fix does not
// overcorrect: an empty status alongside a genuinely positive fill quantity
// must still default to "filled" — some brokers confirm a fill (Terminal or
// a real reported FilledQty) without ever populating a status string.
func TestFilledOrStillDefaultsToFilledForARealFill(t *testing.T) {
	if got := filledOr("", 5); got != "filled" {
		t.Errorf(`filledOr("", 5) = %q, want "filled"`, got)
	}
}

// TestFilledOrPrefersARealStatusOverTheFallback confirms a non-empty status
// is never overridden, regardless of qty.
func TestFilledOrPrefersARealStatusOverTheFallback(t *testing.T) {
	if got := filledOr("partially_filled", 0); got != "partially_filled" {
		t.Errorf(`filledOr("partially_filled", 0) = %q, want the real status preserved`, got)
	}
}

// silentRejectBroker confirms every order as a definitive zero fill
// (Terminal, no status string) — a broker that rejects/cancels an order
// without populating a status field, the scenario filledOr must handle
// honestly. Distinct from the package's own rejectingBroker (pendingsell_test.go),
// which always sets Status: "rejected" and so never exercises the empty-
// status fallback this test is about.
type silentRejectBroker struct{ broker.Broker }

func (silentRejectBroker) Name() string { return "silent-reject" }

func (silentRejectBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	return broker.OrderResult{
		ID: "rej-1", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		Terminal: true, FilledQty: 0, Status: "", // confirmed zero, no status string
	}, nil
}

func (silentRejectBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 1000}, nil
}

func (silentRejectBroker) GetPositions(context.Context) ([]broker.Position, error) { return nil, nil }

// TestManualOrderConfirmedZeroFillIsNotRecordedAsFilled is the end-to-end
// reproduction: ManualOrder's own confirmed-zero branch (side == Sell &&
// settled <= 0) must produce a trade record whose Status does not falsely
// claim "filled" when the broker leaves Status blank.
func TestManualOrderConfirmedZeroFillIsNotRecordedAsFilled(t *testing.T) {
	e := ledgerEngine(t)
	e.brk = silentRejectBroker{}

	rec, err := e.ManualOrder(context.Background(), "X", broker.Sell, 2)
	if err != nil {
		t.Fatalf("ManualOrder: %v", err)
	}

	if rec.Status == "filled" {
		t.Errorf("recorded Status = %q with Qty %v — a confirmed zero fill must not read as filled",
			rec.Status, rec.Qty)
	}
}
