package domain_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

var errStore = errors.New("rule store unavailable")

// fakeStore serves a fixed rule set and counts consumption.
type fakeStore struct {
	rules       []*domain.Rule
	consumed    []int64
	failActive  bool
	failConsume bool
}

func (f *fakeStore) Active(context.Context, int64) ([]*domain.Rule, error) {
	if f.failActive {
		return nil, errStore
	}
	return f.rules, nil
}

func (f *fakeStore) Consume(_ context.Context, ruleID int64) error {
	if f.failConsume {
		return errStore
	}
	f.consumed = append(f.consumed, ruleID)
	for _, r := range f.rules {
		if r.ID == ruleID && r.TimesLeft != nil {
			*r.TimesLeft--
		}
	}
	return nil
}

// seededRand returns its values in order, then repeats the last one, so a
// probability test is fully deterministic.
type seededRand struct {
	values []float64
	i      int
}

func (s *seededRand) Float64() float64 {
	if s.i >= len(s.values) {
		return s.values[len(s.values)-1]
	}
	v := s.values[s.i]
	s.i++
	return v
}

func alwaysFires() *seededRand { return &seededRand{values: []float64{0}} }

func TestEvaluateNoRulesLetsTheRequestThrough(t *testing.T) {
	e := domain.NewEngine(&fakeStore{}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.False(t, got.Faulted())
	assert.Nil(t, got.Rule)
	assert.Equal(t, domain.ActionPassthrough, got.Action())
}

func TestEvaluateReturnsTheMatchingRule(t *testing.T) {
	rule := enabledRule()
	rule.Action = domain.ActionDelay
	rule.DelayMillis = 120000 // the "answer in two minutes" case
	e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{rule}}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.True(t, got.Faulted())
	assert.Equal(t, domain.ActionDelay, got.Action())
	assert.Equal(t, 120000, got.DelayMillis)
}

// Lower priority values win, and equal priorities fall back to insertion order
// by ID, so evaluation does not depend on how the store sorted its rows.
func TestEvaluateHonoursPriorityOrder(t *testing.T) {
	low := enabledRule()
	low.ID, low.Priority, low.Action = 10, 200, domain.ActionDrop

	high := enabledRule()
	high.ID, high.Priority, high.Action = 20, 10, domain.ActionHTTPStatus

	e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{low, high}}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.Equal(t, domain.ActionHTTPStatus, got.Action())
}

func TestEvaluateBreaksPriorityTiesByID(t *testing.T) {
	second := enabledRule()
	second.ID, second.Action = 2, domain.ActionDrop

	first := enabledRule()
	first.ID, first.Action = 1, domain.ActionMalformed

	e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{second, first}}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.Equal(t, domain.ActionMalformed, got.Action())
}

func TestEvaluateSkipsNonMatchingRules(t *testing.T) {
	wrongMethod := enabledRule()
	wrongMethod.ID, wrongMethod.Priority = 1, 1
	wrongMethod.Method = "receipts.*"
	wrongMethod.Action = domain.ActionDrop

	matching := enabledRule()
	matching.ID, matching.Priority = 2, 2
	matching.Action = domain.ActionHTTPStatus

	e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{wrongMethod, matching}}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.Equal(t, domain.ActionHTTPStatus, got.Action())
}

func TestEvaluateSkipsExhaustedRules(t *testing.T) {
	spent := enabledRule()
	spent.ID, spent.Priority = 1, 1
	spent.TimesLeft = ptrInt(0)
	spent.Action = domain.ActionDrop

	fallback := enabledRule()
	fallback.ID, fallback.Priority = 2, 2
	fallback.Action = domain.ActionMalformed

	e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{spent, fallback}}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.Equal(t, domain.ActionMalformed, got.Action())
}

// A rule limited to one use fires once and then steps aside.
func TestLimitedRuleFiresExactlyItsAllowance(t *testing.T) {
	rule := enabledRule()
	rule.TimesLeft = ptrInt(1)
	rule.Action = domain.ActionRPCError
	store := &fakeStore{rules: []*domain.Rule{rule}}
	e := domain.NewEngine(store, alwaysFires())
	ctx := context.Background()

	first, err := e.Evaluate(ctx, baseRequest())
	require.NoError(t, err)
	assert.True(t, first.Faulted())

	second, err := e.Evaluate(ctx, baseRequest())
	require.NoError(t, err)
	assert.False(t, second.Faulted(), "a one-shot rule must not fire twice")

	assert.Equal(t, []int64{rule.ID}, store.consumed)
}

func TestUnlimitedRuleIsNotConsumed(t *testing.T) {
	rule := enabledRule()
	store := &fakeStore{rules: []*domain.Rule{rule}}
	e := domain.NewEngine(store, alwaysFires())

	_, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.Empty(t, store.consumed, "a rule with no limit has nothing to consume")
}

// A 30% rule fires when the roll lands under the threshold and steps aside
// when it does not. The randomizer is seeded, so this is deterministic.
func TestProbabilityDecidesWhetherARuleFires(t *testing.T) {
	tests := []struct {
		name        string
		probability float64
		roll        float64
		wantFaulted bool
	}{
		{"roll below the threshold fires", 0.3, 0.29, true},
		{"roll at the threshold does not fire", 0.3, 0.30, false},
		{"roll above the threshold does not fire", 0.3, 0.99, false},
		{"certainty always fires", 1, 0.99, true},
		{"impossibility never fires", 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := enabledRule()
			rule.Probability = tt.probability
			e := domain.NewEngine(
				&fakeStore{rules: []*domain.Rule{rule}},
				&seededRand{values: []float64{tt.roll}},
			)

			got, err := e.Evaluate(context.Background(), baseRequest())

			require.NoError(t, err)
			assert.Equal(t, tt.wantFaulted, got.Faulted())
		})
	}
}

func TestProbabilityMissLetsALowerPriorityRuleFire(t *testing.T) {
	flaky := enabledRule()
	flaky.ID, flaky.Priority = 1, 1
	flaky.Probability = 0.3
	flaky.Action = domain.ActionDrop

	certain := enabledRule()
	certain.ID, certain.Priority = 2, 2
	certain.Action = domain.ActionMalformed

	e := domain.NewEngine(
		&fakeStore{rules: []*domain.Rule{flaky, certain}},
		&seededRand{values: []float64{0.9}}, // the flaky rule loses its roll
	)

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.Equal(t, domain.ActionMalformed, got.Action())
}

// A passthrough rule matches but deliberately does nothing, which is how a
// single account is exempted from a broad fault.
func TestPassthroughRuleShieldsTheRequest(t *testing.T) {
	exempt := enabledRule()
	exempt.ID, exempt.Priority = 1, 1
	exempt.Action = domain.ActionPassthrough
	exempt.MatchAccount = map[string]string{"phone": "901234567"}

	broad := enabledRule()
	broad.ID, broad.Priority = 2, 2
	broad.Action = domain.ActionDrop

	e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{exempt, broad}}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())

	require.NoError(t, err)
	assert.False(t, got.Faulted(), "a passthrough match must not fault the request")
	assert.NotNil(t, got.Rule, "the shielding rule is still reported for the traffic log")
	assert.Equal(t, domain.ActionPassthrough, got.Action())
}

func TestEvaluatePropagatesStoreFailures(t *testing.T) {
	t.Run("listing rules", func(t *testing.T) {
		e := domain.NewEngine(&fakeStore{failActive: true}, alwaysFires())

		_, err := e.Evaluate(context.Background(), baseRequest())

		assert.ErrorIs(t, err, errStore)
	})

	t.Run("consuming a rule", func(t *testing.T) {
		rule := enabledRule()
		rule.TimesLeft = ptrInt(1)
		e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{rule}, failConsume: true}, alwaysFires())

		_, err := e.Evaluate(context.Background(), baseRequest())

		assert.ErrorIs(t, err, errStore)
	})
}

// The console's headline case: make one method answer with one chosen code.
func TestErrorRuleCarriesTheChosenCodeAndText(t *testing.T) {
	rule := enabledRule()
	rule.Method = "PerformTransaction"
	rule.Action = domain.ActionRPCError
	rule.ErrorCode = payerr.CodeCannotPerform

	e := domain.NewEngine(&fakeStore{rules: []*domain.Rule{rule}}, alwaysFires())

	got, err := e.Evaluate(context.Background(), baseRequest())
	require.NoError(t, err)
	require.True(t, got.Faulted())

	protoErr := got.Rule.ProtocolError()
	assert.Equal(t, payerr.CodeCannotPerform, protoErr.Code)
	assert.True(t, protoErr.Message.Complete())
}
