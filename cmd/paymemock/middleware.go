package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"

	faultdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	trafficdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// serviceName is what this process calls itself in rules and in the log.
const serviceName = "paymemock"

// randomizer yields the values probability rules are decided on.
type randomizer struct{}

func (randomizer) Float64() float64 { return rand.Float64() }

// describeRequest turns a Subscribe API call into the facts a rule matches on.
//
// The account and amount are read from the JSON-RPC params, so a rule can be
// narrowed to one payer or one amount rather than breaking every call.
func describeRequest(r *http.Request, body []byte) faultdomain.Request {
	out := faultdomain.Request{Service: faultdomain.ServicePaymeMock}

	if sandbox, ok := sandboxctx.Get(r.Context()); ok {
		out.SandboxID = sandbox.ID
	}

	var envelope struct {
		Method string `json:"method"`
		Params struct {
			ID      string            `json:"id"`
			Amount  int64             `json:"amount"`
			Account map[string]string `json:"account"`
		} `json:"params"`
	}

	// A body that is not JSON-RPC still describes a request; it simply has no
	// method to narrow a rule by, so only service-wide rules can match it.
	if err := json.Unmarshal(body, &envelope); err != nil {
		return out
	}

	out.Method = envelope.Method
	out.PaymeID = envelope.Params.ID
	out.Amount = envelope.Params.Amount
	out.Account = envelope.Params.Account

	return out
}

// middlewareDeps are the ports the middleware stack needs.
type middlewareDeps struct {
	rules   faultdomain.RuleStore
	hits    ruleHitRecorder
	traffic trafficdomain.Recorder
	clock   clock.Clock
	// repeats answers a call the caller has already made with the answer it
	// got, for the methods that would otherwise do the work twice.
	repeats *httpx.IdempotencyMiddleware
}

// ruleHitRecorder counts a rule's applications for the console.
type ruleHitRecorder interface {
	Hit(ctx context.Context, ruleID int64) error
}

// withMiddleware puts the traffic log outside the fault layer, so what is
// recorded is what the caller actually received, faulted or not.
func withMiddleware(handler http.Handler, deps middlewareDeps, log *slog.Logger) http.Handler {
	faults := httpx.NewFaultMiddleware(
		faultdomain.NewEngine(deps.rules, randomizer{}),
		describeRequest,
		httpx.RealSleeper{},
	)

	// A rule that fired is counted here rather than in the engine, which only
	// touches rules with a limited number of uses; without this the console
	// could not show that an unlimited rule is doing anything.
	faults.OnDecision(func(ctx context.Context, d faultdomain.Decision) {
		if !d.Faulted() {
			return
		}
		if err := deps.hits.Hit(context.WithoutCancel(ctx), d.Rule.ID); err != nil {
			log.Error("fault rule hit not recorded", "rule", d.Rule.ID, "error", err)
		}
	})

	traffic := httpx.NewTrafficMiddleware(deps.traffic, serviceName, deps.clock, func(err error) {
		// A request that was answered must not be failed because its record
		// could not be written; the operator is told instead.
		log.Error("traffic record failed", "error", err)
	})

	// The replay sits inside the traffic log and inside the faults: a repeat is
	// still traffic an operator has to see, and a rule that was going to break
	// this call must still break it rather than be answered out of a store.
	handler = deps.repeats.Wrap(handler)

	return traffic.Wrap(faults.Wrap(handler))
}

// idempotentMethods are the calls a repeat of which must not do the work twice.
//
// They are the ones that create something: a payout asked for again is a second
// payout, and an integration that lost the first response cannot tell. The read
// methods are left alone — answering cards.check out of a store would hide the
// very state an operator changed between the two calls.
var idempotentMethods = []string{
	"transactions.create",
	"receipts.create",
	"receipts.pay",
	"cards.create",
}
