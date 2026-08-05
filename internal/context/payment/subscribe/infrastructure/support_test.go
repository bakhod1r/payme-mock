package infrastructure_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// discard is a logger for tests that care about behaviour, not output.
func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Nothing downstream may be able to tell a mock identifier from a real one, so
// the shapes are asserted rather than assumed.
func TestTokensLookLikeTheProvidersOwn(t *testing.T) {
	tokens := infrastructure.NewTokens()

	for _, id := range []string{tokens.ReceiptID(), tokens.TransactionID()} {
		assert.Len(t, id, 24, "the provider's identifiers are 24 hex characters")
		_, err := hex.DecodeString(id)
		assert.NoError(t, err)
	}

	token := tokens.CardToken()
	raw, err := base64.StdEncoding.DecodeString(token)
	require.NoError(t, err)
	assert.Len(t, raw, 48)
}

func TestTokensAreDistinct(t *testing.T) {
	tokens := infrastructure.NewTokens()

	seen := make(map[string]bool, 300)
	for range 100 {
		for _, v := range []string{tokens.CardToken(), tokens.ReceiptID(), tokens.TransactionID()} {
			assert.False(t, seen[v], "a reused identifier would collide across receipts")
			seen[v] = true
		}
	}
}

// The stand is offline, so a message is something to inspect in the console
// rather than something delivered.
func TestSMSLogRecordsInsteadOfSending(t *testing.T) {
	sms := infrastructure.NewSMSLog(discard())

	require.NoError(t, sms.Send(context.Background(), "998901234567", "code 666666"))
	require.NoError(t, sms.Send(context.Background(), "998907654321", "receipt"))

	sent := sms.Sent()
	require.Len(t, sent, 2)
	assert.Equal(t, "998901234567", sent[0].Phone)
	assert.Equal(t, "code 666666", sent[0].Message)
	assert.False(t, sent[0].At.IsZero())
	assert.Equal(t, "receipt", sent[1].Message)

	// The returned slice is a copy, so a caller cannot rewrite the record.
	sent[0].Phone = "changed"
	assert.Equal(t, "998901234567", sms.Sent()[0].Phone)
}

// ---------- scheduler ----------

// recorder captures the steps the scheduler ran.
type recorder struct {
	mu      sync.Mutex
	calls   []string
	sandbox []sandboxctx.Sandbox
	err     error
	done    chan struct{}
}

func newRecorder(expected int) *recorder {
	return &recorder{done: make(chan struct{}, expected)}
}

func (r *recorder) advance(ctx context.Context, receiptID string) error {
	r.mu.Lock()
	r.calls = append(r.calls, receiptID)
	if s, ok := sandboxctx.Get(ctx); ok {
		r.sandbox = append(r.sandbox, s)
	}
	err := r.err
	r.mu.Unlock()

	r.done <- struct{}{}
	return err
}

func (r *recorder) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// The step must keep running after the caller has been answered, and it must
// still know which stand it belongs to, since the repositories it reaches
// refuse to run unscoped.
func TestSchedulerRunsTheStepWithItsSandboxAfterTheRequestEnds(t *testing.T) {
	scheduler := infrastructure.NewScheduler(discard())
	steps := newRecorder(1)
	scheduler.SetAdvance(steps.advance)

	ctx, cancel := context.WithCancel(sandboxctx.With(context.Background(),
		sandboxctx.Sandbox{ID: 7, Slug: "qa"}))
	require.NoError(t, scheduler.ScheduleAdvance(ctx, receiptID, 0))
	cancel() // the caller has been answered and its context is gone

	scheduler.Wait()

	assert.Equal(t, []string{receiptID}, steps.ran())
	require.Len(t, steps.sandbox, 1)
	assert.Equal(t, int64(7), steps.sandbox[0].ID)
	assert.Equal(t, "qa", steps.sandbox[0].Slug)
}

// The delay is what makes payment take plausible time instead of settling the
// instant it is asked for.
func TestSchedulerWaitsTheConfiguredDelay(t *testing.T) {
	scheduler := infrastructure.NewScheduler(discard())
	steps := newRecorder(1)
	scheduler.SetAdvance(steps.advance)

	start := time.Now()
	require.NoError(t, scheduler.ScheduleAdvance(context.Background(), receiptID, 40))
	scheduler.Wait()

	assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)
	assert.Equal(t, []string{receiptID}, steps.ran())
}

// A failed step is reported to the operator; it cannot fail the caller, who
// was answered long before.
func TestSchedulerLogsAFailedStep(t *testing.T) {
	scheduler := infrastructure.NewScheduler(discard())
	steps := newRecorder(1)
	steps.err = errors.New("database gone")
	scheduler.SetAdvance(steps.advance)

	require.NoError(t, scheduler.ScheduleAdvance(context.Background(), receiptID, 0))
	scheduler.Wait()

	assert.Equal(t, []string{receiptID}, steps.ran())
}

// Before a runner is attached the stand is still starting; the receipt stays
// where it is rather than the caller being failed.
func TestSchedulerDropsAStepWithNoRunnerAttached(t *testing.T) {
	scheduler := infrastructure.NewScheduler(discard())

	assert.NoError(t, scheduler.ScheduleAdvance(context.Background(), receiptID, 0))

	scheduler.Wait()
}
