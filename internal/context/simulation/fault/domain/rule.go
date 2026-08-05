// Package domain models fault injection: the rules that make a method answer
// slowly, fail with a chosen code, or drop the connection. This is what lets
// the console reshape the stand's behaviour without a restart.
package domain

import (
	"errors"
	"path"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// Service names the process a rule applies to.
type Service string

// The services a rule can target. ServiceAny matches all of them.
const (
	ServiceMerchant  Service = "merchant"
	ServicePaymeMock Service = "paymemock"
	ServiceGateway   Service = "gateway"
	ServiceAny       Service = "*"
)

// Action is what a matching rule does to the request.
type Action string

// The available actions.
const (
	// ActionDelay holds the response back before answering normally.
	ActionDelay Action = "delay"
	// ActionRPCError answers with a chosen protocol error.
	ActionRPCError Action = "rpc_error"
	// ActionHTTPStatus answers with a status other than 200, which the
	// protocol treats as a transport failure.
	ActionHTTPStatus Action = "http_status"
	// ActionMalformed answers with a body that is not valid JSON.
	ActionMalformed Action = "malformed"
	// ActionDrop closes the connection without answering, simulating a timeout.
	ActionDrop Action = "drop"
	// ActionDuplicate sends the request twice, exercising idempotency.
	ActionDuplicate Action = "duplicate"
	// ActionPassthrough does nothing; useful as a high-priority exemption.
	ActionPassthrough Action = "passthrough"
)

// Valid reports whether a is a known action.
func (a Action) Valid() bool {
	switch a {
	case ActionDelay, ActionRPCError, ActionHTTPStatus,
		ActionMalformed, ActionDrop, ActionDuplicate, ActionPassthrough:
		return true
	default:
		return false
	}
}

// Rule is one fault injection rule. Rules are ordered by Priority, lowest
// first; the first match wins.
type Rule struct {
	ID       int64
	Name     string
	Enabled  bool
	Priority int

	ConfigID  int64
	SandboxID *int64

	// Matching
	Service      Service
	Method       string // glob, for example "receipts.*" or "*"
	MatchAccount map[string]string
	MatchPaymeID string
	AmountMin    *int64
	AmountMax    *int64

	// Behaviour
	Action       Action
	DelayMillis  int
	ErrorCode    payerr.Code
	ErrorMessage payerr.Message
	ErrorData    string
	HTTPStatus   int

	Probability float64
	TimesLeft   *int
	HitCount    int64
	Note        string
}

// Request describes the call a rule is being matched against.
type Request struct {
	Service   Service
	Method    string
	SandboxID int64
	Account   map[string]string
	PaymeID   string
	Amount    int64
}

// Matches reports whether the rule applies to a request, ignoring probability
// and remaining uses, which are decided when the rule is applied.
func (r *Rule) Matches(req Request) bool {
	if !r.Enabled {
		return false
	}
	if r.SandboxID != nil && *r.SandboxID != req.SandboxID {
		return false
	}
	if r.Service != ServiceAny && r.Service != req.Service {
		return false
	}
	if !matchGlob(r.Method, req.Method) {
		return false
	}
	if r.MatchPaymeID != "" && !matchGlob(r.MatchPaymeID, req.PaymeID) {
		return false
	}
	if r.AmountMin != nil && req.Amount < *r.AmountMin {
		return false
	}
	if r.AmountMax != nil && req.Amount > *r.AmountMax {
		return false
	}
	return r.matchesAccount(req.Account)
}

// matchesAccount requires every configured account field to be present and to
// match its pattern. A rule with no account constraints matches anything.
func (r *Rule) matchesAccount(account map[string]string) bool {
	for field, pattern := range r.MatchAccount {
		value, ok := account[field]
		if !ok || !matchGlob(pattern, value) {
			return false
		}
	}
	return true
}

// Exhausted reports whether a limited-use rule has been spent.
func (r *Rule) Exhausted() bool {
	return r.TimesLeft != nil && *r.TimesLeft <= 0
}

// ProtocolError renders the rule's configured error. Rules may override the
// catalog text; when they do not, the documented message is used.
func (r *Rule) ProtocolError() *payerr.ProtocolError {
	if r.ErrorMessage.Complete() {
		return payerr.New(r.ErrorCode, r.ErrorMessage, r.ErrorData)
	}
	if known, ok := payerr.ByCode(r.ErrorCode); ok {
		out := known
		if r.ErrorData != "" {
			out = out.WithData(r.ErrorData)
		}
		return out
	}
	return payerr.New(r.ErrorCode, payerr.ErrTransport.Message, r.ErrorData)
}

// matchGlob reports whether value satisfies a pattern. An empty pattern and
// "*" both match everything; otherwise shell-style globbing applies, so
// "receipts.*" matches every Subscribe API receipt method.
func matchGlob(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	ok, err := path.Match(pattern, value)
	if err != nil {
		// A malformed pattern must not silently match every request; a rule
		// nobody can parse is a rule that does nothing.
		return false
	}
	return ok
}

// ErrNotFound is what the store returns when no rule matches an identifier.
var ErrNotFound = errors.New("fault rule not found")
