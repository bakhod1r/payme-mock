// Package clock abstracts time so that timeout, hold-expiry and OTP-validity
// logic can be tested by advancing a fake clock instead of sleeping.
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock is the port every bounded context depends on instead of time.Now.
type Clock interface {
	// Now returns the current wall time.
	Now() time.Time
	// NowMillis returns the current time as a Payme protocol timestamp:
	// milliseconds since the Unix epoch.
	NowMillis() int64
	// Sleep blocks for d, or until the context is done. It belongs on the
	// clock rather than being a bare time.Sleep so a stall can be rehearsed in
	// a test without the test taking as long as the stall.
	Sleep(ctx context.Context, d time.Duration)
}

// System is the production Clock backed by the operating system.
type System struct{}

// New returns the system clock.
func New() System { return System{} }

// Now returns the current wall time.
func (System) Now() time.Time { return time.Now() }

// NowMillis returns the current time in milliseconds since the Unix epoch.
func (s System) NowMillis() int64 { return ToMillis(s.Now()) }

// Sleep blocks for d. A caller that gives up first ends the wait: a client
// that has hung up is not owed the rest of a simulated stall.
func (System) Sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Fake is a Clock whose time only moves when the test moves it.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake clock started at the given instant.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now returns the fake clock's current instant.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NowMillis returns the fake clock's current instant in milliseconds.
func (f *Fake) NowMillis() int64 { return ToMillis(f.Now()) }

// Sleep moves the fake clock forward instead of blocking, so a test can
// rehearse a ten second stall without waiting ten seconds.
func (f *Fake) Sleep(_ context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	f.Advance(d)
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the fake clock to an absolute instant.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

// ToMillis converts a time to a Payme protocol timestamp.
func ToMillis(t time.Time) int64 { return t.UnixNano() / int64(time.Millisecond) }

// FromMillis converts a Payme protocol timestamp back to a time.
func FromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
