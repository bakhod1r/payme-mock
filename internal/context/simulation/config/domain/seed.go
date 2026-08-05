package domain

import (
	fault "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// The seeded profile names. Switching between them is how the console reshapes
// the stand without anyone editing rules by hand.
const (
	ProfileClean   = "clean"
	ProfileSlow    = "slow"
	ProfileFlaky   = "flaky"
	ProfileErrors  = "errors"
	ProfileTimeout = "timeout"
	ProfileStrict  = "strict"
	ProfileWalkIn  = "walkin"
)

// Seed is a profile together with the fault rules that define its behaviour.
type Seed struct {
	Profile *Profile
	Rules   []*fault.Rule
}

// SeedProfiles returns the built-in profiles in the order they are created.
func SeedProfiles() []Seed {
	return []Seed{
		cleanSeed(),
		slowSeed(),
		flakySeed(),
		errorsSeed(),
		timeoutSeed(),
		strictSeed(),
		walkInSeed(),
	}
}

// walkInSeed accepts a payer the merchant has never seen. It is the profile a
// card-charging register needs: the account it sends is generated per payment,
// so under any other profile every payment is refused as an unknown order.
func walkInSeed() Seed {
	settings := DefaultSettings()
	settings.AutoRegisterAccounts = true

	return Seed{
		Profile: builtin(ProfileWalkIn,
			"Like clean, but a payer the merchant has never seen is registered as the payment arrives.",
			settings),
	}
}

// cleanSeed is the baseline: everything works.
func cleanSeed() Seed {
	return Seed{
		Profile: builtin(ProfileClean, "Nothing injected; the stand behaves like a healthy provider.",
			DefaultSettings()),
	}
}

// slowSeed answers everything after two minutes, which is the case the console
// exists to reproduce: a provider that has not failed, only stalled.
func slowSeed() Seed {
	return Seed{
		Profile: builtin(ProfileSlow, "Every call answers after two minutes.", DefaultSettings()),
		Rules: []*fault.Rule{{
			Name:        "two minute delay",
			Enabled:     true,
			Priority:    100,
			Service:     fault.ServiceAny,
			Method:      "*",
			Action:      fault.ActionDelay,
			DelayMillis: 120_000,
			Probability: 1,
		}},
	}
}

// flakySeed fails roughly a third of calls, which is what shakes out retry and
// idempotency handling.
func flakySeed() Seed {
	return Seed{
		Profile: builtin(ProfileFlaky, "Roughly a third of calls fail at random.", DefaultSettings()),
		Rules: []*fault.Rule{
			{
				Name:        "intermittent transport failure",
				Enabled:     true,
				Priority:    100,
				Service:     fault.ServiceAny,
				Method:      "*",
				Action:      fault.ActionHTTPStatus,
				HTTPStatus:  500,
				Probability: 0.15,
			},
			{
				Name:        "intermittent duplicate delivery",
				Enabled:     true,
				Priority:    110,
				Service:     fault.ServiceAny,
				Method:      "*",
				Action:      fault.ActionDuplicate,
				Probability: 0.15,
			},
		},
	}
}

// errorsSeed gives each write method the protocol error it is most likely to
// meet in production.
func errorsSeed() Seed {
	return Seed{
		Profile: builtin(ProfileErrors, "Each method answers with its most likely protocol error.",
			DefaultSettings()),
		Rules: []*fault.Rule{
			errorRule("create refuses", 100, "CreateTransaction", payerr.CodeCannotPerform),
			errorRule("perform refuses", 110, "PerformTransaction", payerr.CodeCannotPerform),
			errorRule("cancel reports a completed order", 120, "CancelTransaction", payerr.CodeOrderCompleted),
			errorRule("check reports a missing transaction", 130, "CheckTransaction", payerr.CodeTransactionNotFnd),
			errorRule("check perform rejects the amount", 140, "CheckPerformTransaction", payerr.CodeInvalidAmount),
			errorRule("receipts refuse", 150, "receipts.*", payerr.CodeCannotPerform),
		},
	}
}

// timeoutSeed drops the connection outright, which is the failure a delay
// cannot reproduce: no response ever arrives.
func timeoutSeed() Seed {
	return Seed{
		Profile: builtin(ProfileTimeout, "Connections are closed without a response.", DefaultSettings()),
		Rules: []*fault.Rule{{
			Name:        "drop the connection",
			Enabled:     true,
			Priority:    100,
			Service:     fault.ServiceAny,
			Method:      "*",
			Action:      fault.ActionDrop,
			Probability: 1,
		}},
	}
}

// strictSeed shortens the timings so ordering and idempotency mistakes surface
// in seconds rather than hours, and duplicates every write.
func strictSeed() Seed {
	settings := DefaultSettings()
	settings.TransactionTimeoutMillis = 5_000
	settings.CardVerifyWaitMillis = 5_000
	settings.StepDelayMillis = 0

	return Seed{
		Profile: builtin(ProfileStrict,
			"Short deadlines and duplicated writes, to surface ordering and idempotency mistakes.",
			settings),
		Rules: []*fault.Rule{{
			Name:        "duplicate every write",
			Enabled:     true,
			Priority:    100,
			Service:     fault.ServiceAny,
			Method:      "*Transaction",
			Action:      fault.ActionDuplicate,
			Probability: 1,
		}},
	}
}

func builtin(name, description string, settings Settings) *Profile {
	return &Profile{Name: name, Description: description, Settings: settings, Builtin: true}
}

// errorRule builds a rule that answers one method with one documented code.
func errorRule(name string, priority int, method string, code payerr.Code) *fault.Rule {
	return &fault.Rule{
		Name:        name,
		Enabled:     true,
		Priority:    priority,
		Service:     fault.ServiceAny,
		Method:      method,
		Action:      fault.ActionRPCError,
		ErrorCode:   code,
		Probability: 1,
	}
}
