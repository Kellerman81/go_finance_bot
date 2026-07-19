package engine

import (
	"sync/atomic"
	"time"
)

// metrics holds lock-free engine counters for observability. It is embedded in
// the Engine by value; the Engine is only ever used via pointer, so the atomics
// are never copied.
type metrics struct {
	ticks        atomic.Int64 // live ticks ingested
	barsDone     atomic.Int64 // completed bars appended to history
	evalCycles   atomic.Int64 // evaluate() cycles run
	ordersSent   atomic.Int64 // orders submitted to the broker
	lastEvalNs   atomic.Int64 // duration of the last evaluate() cycle (ns)
	lastEvalUnix atomic.Int64 // unix-nanos when the last evaluate finished
	openOrders   atomic.Int64 // working orders seen at the last reconcile
}

// onTick counts an ingested tick and, when it completed a bar, the bar.
func (m *metrics) onTick(barCompleted bool) {
	m.ticks.Add(1)

	if barCompleted {
		m.barsDone.Add(1)
	}
}

// onEval records a completed evaluate() cycle and its duration.
func (m *metrics) onEval(d time.Duration) {
	m.evalCycles.Add(1)
	m.lastEvalNs.Store(int64(d))
	m.lastEvalUnix.Store(time.Now().UnixNano())
}

// onOrder counts an order submission.
func (m *metrics) onOrder() { m.ordersSent.Add(1) }

// setOpenOrders records the working-order count from the last reconcile.
func (m *metrics) setOpenOrders(n int) { m.openOrders.Store(int64(n)) }

// Metrics is a point-in-time snapshot of engine counters, for the API/UI.
type Metrics struct {
	TicksIngested   int64     `json:"ticks_ingested"`
	BarsCompleted   int64     `json:"bars_completed"`
	EvalCycles      int64     `json:"eval_cycles"`
	OrdersSubmitted int64     `json:"orders_submitted"`
	OpenOrders      int64     `json:"open_orders"`
	LastEvalMillis  float64   `json:"last_eval_ms"`
	LastEvalTime    time.Time `json:"last_eval_time"`
}

// Metrics returns a snapshot of the engine's runtime counters.
func (e *Engine) Metrics() Metrics {
	var lastEval time.Time

	if ns := e.met.lastEvalUnix.Load(); ns > 0 {
		lastEval = time.Unix(0, ns)
	}

	return Metrics{
		TicksIngested:   e.met.ticks.Load(),
		BarsCompleted:   e.met.barsDone.Load(),
		EvalCycles:      e.met.evalCycles.Load(),
		OrdersSubmitted: e.met.ordersSent.Load(),
		OpenOrders:      e.met.openOrders.Load(),
		LastEvalMillis:  float64(e.met.lastEvalNs.Load()) / 1e6,
		LastEvalTime:    lastEval,
	}
}
