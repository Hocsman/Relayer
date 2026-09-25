package supervisetest

import (
	"sync"
	"time"
)

// Clock is a clock that moves only when a test moves it, for
// supervise.Options.Now: a test then says how long after an answer a repeat
// arrives instead of sleeping for it. The core calls Now from several goroutines at
// once, outside its own lock, so Now and Advance are safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock that reads start until it is advanced.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now is the time the clock reads.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock on by the given duration.
func (c *Clock) Advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}
