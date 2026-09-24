package server

import "github.com/Hocsman/Relayer/internal/session"

// injectEvent hands one event to the run's event handling, as the run's event
// loop does with what its runtime emits.
func injectEvent(ctrl *Controller, event session.Event) {
	ctrl.mu.RLock()
	rt, sup := ctrl.runtime, ctrl.sup
	ctrl.mu.RUnlock()
	ctrl.handleEvent(rt, sup, event)
}
