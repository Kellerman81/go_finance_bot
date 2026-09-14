package backtest

import "context"

// countAfterCtx reports itself cancelled once its Err method has been
// called at least cancelAfter times — a deterministic stand-in for "the
// operator hit Ctrl+C partway through," since the search functions here
// check ctx.Err() exactly once per grid combination/candidate/fold, making
// the call count a precise proxy for how much of the search ran before the
// cancellation. A real context.WithCancel raced against a goroutine would
// make the exact cutoff point (and so the exact assertion on how many
// results came back) nondeterministic; this keeps the tests exact.
type countAfterCtx struct {
	context.Context

	calls       *int
	cancelAfter int
}

func (c countAfterCtx) Err() error {
	*c.calls++

	if *c.calls >= c.cancelAfter {
		return context.Canceled
	}

	return nil
}

// newCountAfterCtx returns a context that reports cancelled starting on its
// cancelAfter'th Err() call.
func newCountAfterCtx(cancelAfter int) context.Context {
	return countAfterCtx{context.Background(), new(int), cancelAfter}
}
