package main

import (
	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
)

// These entry points inject a run's events the way its event loop does, and
// keep the signatures the bridge's tests were written against before the
// state machine moved into internal/supervise.

func (a *App) handleAdapterEvent(event adapters.Event) {
	a.mu.RLock()
	run := a.active
	a.mu.RUnlock()
	if run != nil {
		a.handleAdapterEventForRun(run, event)
	}
}

func (a *App) handleAdapterEventWithdrawn(event adapters.Event) {
	a.mu.RLock()
	run := a.active
	a.mu.RUnlock()
	if run != nil {
		run.sup.Handle(session.AdapterEventWithdrawn{Event: event})
	}
}

func (a *App) handleAdapterEventForRun(run *runGeneration, event adapters.Event) {
	run.sup.Handle(session.AdapterEvent{Event: event})
}
