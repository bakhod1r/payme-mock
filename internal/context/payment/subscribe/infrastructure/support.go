package infrastructure

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// Tokens produces identifiers shaped like the provider's: 24-character hex ids
// and long base64 card tokens. Nothing downstream may be able to tell a mock
// identifier from a real one.
type Tokens struct{}

// NewTokens returns the identifier generator.
func NewTokens() Tokens { return Tokens{} }

// CardToken returns an opaque base64 card token.
func (Tokens) CardToken() string {
	buf := make([]byte, 48)
	// crypto/rand.Read fills the buffer or panics; it cannot report a short read.
	_, _ = rand.Read(buf)
	return base64.StdEncoding.EncodeToString(buf)
}

// ReceiptID returns a 24-character hex identifier.
func (Tokens) ReceiptID() string { return hexID() }

// TransactionID returns a 24-character hex identifier, which is what Payme
// sends the merchant as params.id.
func (Tokens) TransactionID() string { return hexID() }

func hexID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// SMSLog records a delivery instead of sending one. The stand is offline, so a
// message is something to be inspected in the console, not delivered.
type SMSLog struct {
	log *slog.Logger

	mu   sync.Mutex
	sent []SMS
}

// SMS is one recorded delivery.
type SMS struct {
	Phone   string
	Message string
	At      time.Time
}

// NewSMSLog returns a recorder writing to log.
func NewSMSLog(log *slog.Logger) *SMSLog {
	return &SMSLog{log: log}
}

// Send records the message.
func (s *SMSLog) Send(_ context.Context, phone, message string) error {
	s.mu.Lock()
	s.sent = append(s.sent, SMS{Phone: phone, Message: message, At: time.Now()})
	s.mu.Unlock()

	s.log.Info("sms recorded", "phone", phone, "message", message)
	return nil
}

// Sent returns the recorded deliveries, most recent last.
func (s *SMSLog) Sent() []SMS {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SMS(nil), s.sent...)
}

// Scheduler runs the receipt state walk in this process, after a delay.
//
// The walk is what makes payment take plausible time instead of settling the
// instant it is asked for. The delay is a profile setting, so a stand can be
// made slow or immediate without touching this.
type Scheduler struct {
	log *slog.Logger

	// advance is set after the service is built, because the service needs the
	// scheduler and the scheduler needs the service.
	advance atomic.Pointer[func(ctx context.Context, receiptID string) error]

	wg sync.WaitGroup
}

// NewScheduler returns a scheduler with no runner attached yet.
func NewScheduler(log *slog.Logger) *Scheduler {
	return &Scheduler{log: log}
}

// SetAdvance attaches the step the scheduler runs.
func (s *Scheduler) SetAdvance(fn func(ctx context.Context, receiptID string) error) {
	s.advance.Store(&fn)
}

// ScheduleAdvance queues one step of the walk.
//
// The sandbox travels with the step, since the repositories it will reach
// refuse to run unscoped; the request's cancellation does not, because the
// receipt must keep moving after the caller has been answered.
func (s *Scheduler) ScheduleAdvance(ctx context.Context, receiptID string, delayMillis int) error {
	fn := s.advance.Load()
	if fn == nil {
		// Nothing to run means the stand is still starting; the receipt stays
		// where it is rather than the caller being failed.
		s.log.Warn("receipt step dropped: no runner attached", "receipt", receiptID)
		return nil
	}

	stepCtx := context.WithoutCancel(ctx)
	if sandbox, ok := sandboxctx.Get(ctx); ok {
		stepCtx = sandboxctx.With(stepCtx, sandbox)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		if delayMillis > 0 {
			timer := time.NewTimer(time.Duration(delayMillis) * time.Millisecond)
			defer timer.Stop()
			<-timer.C
		}

		if err := (*fn)(stepCtx, receiptID); err != nil {
			s.log.Error("receipt step failed", "receipt", receiptID, "error", err)
		}
	}()

	return nil
}

// Wait blocks until every queued step has run, which shutdown uses so a
// receipt is not left half-way through its walk.
func (s *Scheduler) Wait() { s.wg.Wait() }
