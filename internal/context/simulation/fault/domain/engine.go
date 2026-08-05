package domain

import (
	"context"
	"sort"
)

// Randomizer yields values in [0, 1) for probability decisions. Tests supply a
// deterministic one so a "30% flaky" rule is reproducible.
type Randomizer interface {
	Float64() float64
}

// RuleStore provides the rules in force for a request and records their use.
type RuleStore interface {
	// Active returns the enabled rules of the active configuration profile.
	Active(ctx context.Context, sandboxID int64) ([]*Rule, error)
	// Consume records one application of a rule, decrementing its remaining
	// uses and disabling it once spent.
	Consume(ctx context.Context, ruleID int64) error
}

// Decision is what the engine concluded for a request.
type Decision struct {
	// Rule is the rule that fired, or nil when the request proceeds normally.
	Rule *Rule
	// DelayMillis is how long to wait before answering. It applies to every
	// action, so an error can be made slow as well as wrong.
	DelayMillis int
}

// Faulted reports whether the request should be answered by the fault layer
// rather than by the real handler.
func (d Decision) Faulted() bool {
	return d.Rule != nil && d.Rule.Action != ActionPassthrough
}

// Action returns the decided action, or passthrough when no rule fired.
func (d Decision) Action() Action {
	if d.Rule == nil {
		return ActionPassthrough
	}
	return d.Rule.Action
}

// Engine evaluates rules against requests.
type Engine struct {
	store RuleStore
	rand  Randomizer
}

// NewEngine wires the engine to its rule source and randomizer.
func NewEngine(store RuleStore, rand Randomizer) *Engine {
	return &Engine{store: store, rand: rand}
}

// Evaluate finds the first matching rule by priority and decides whether it
// fires. Rules that are exhausted or lose their probability roll are skipped,
// letting a lower-priority rule take over.
func (e *Engine) Evaluate(ctx context.Context, req Request) (Decision, error) {
	rules, err := e.store.Active(ctx, req.SandboxID)
	if err != nil {
		return Decision{}, err
	}

	sortByPriority(rules)

	for _, rule := range rules {
		if !rule.Matches(req) || rule.Exhausted() {
			continue
		}
		if rule.Probability < 1 && e.rand.Float64() >= rule.Probability {
			continue
		}

		if rule.TimesLeft != nil {
			if err := e.store.Consume(ctx, rule.ID); err != nil {
				return Decision{}, err
			}
		}

		return Decision{Rule: rule, DelayMillis: rule.DelayMillis}, nil
	}

	return Decision{}, nil
}

// sortByPriority orders rules lowest priority value first, breaking ties by ID
// so evaluation is deterministic regardless of how the store returned them.
func sortByPriority(rules []*Rule) {
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority < rules[j].Priority
		}
		return rules[i].ID < rules[j].ID
	})
}
