// Package storage persists trades and bot state. The SQLite implementation is
// pure-Go (modernc.org/sqlite, no cgo) so the database file can be inspected
// with any SQLite tool. With persistence disabled the engine uses its own
// no-op store (engine.nopStore); cmd/bot simply passes no store.
package storage

import "github.com/Kellerman81/go_finance_bot/internal/engine"

// The contract is engine.Store, owned by the engine so it does not import
// this package. This package used to restate it as its own Store interface
// with a Nop implementation; both had drifted (no protection-state methods),
// so Nop no longer satisfied engine.Store and nothing used either (SG2-5).
// The check below is what keeps SQLite and the contract from drifting.
var _ engine.Store = (*SQLite)(nil)
