package main

import (
	"fmt"
	"net/http"
	"strconv"

	faultdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// The outcomes that are not protocol errors: the stand behaving, and the stand
// never answering at all.
const (
	outcomeSuccess = "success"
	outcomeTimeout = "timeout"
)

// everyMethod is the wildcard a rule uses to mean "whatever was called".
const everyMethod = "*"

// parseRuleForm turns the create form into a rule.
//
// Every field is validated here rather than in the database, so a mistyped
// entry comes back as a sentence on the form instead of a constraint violation
// the operator cannot act on.
func parseRuleForm(r *http.Request) (newRule, error) {
	out := newRule{
		Method:      methodOr(r.PostFormValue("method")),
		Service:     serviceOr(r.PostFormValue("service")),
		Probability: 1,
	}

	action, code, err := parseOutcome(r.PostFormValue("outcome"))
	if err != nil {
		return newRule{}, err
	}
	out.Action = action
	out.ErrorCode = code

	if raw := r.PostFormValue("sandbox_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return newRule{}, fmt.Errorf("unknown sandbox")
		}
		out.SandboxID = &id
	}

	if err := parseAction(r, action, &out); err != nil {
		return newRule{}, err
	}

	if raw := r.PostFormValue("probability"); raw != "" {
		percent, err := strconv.ParseFloat(raw, 64)
		if err != nil || percent < 0 || percent > 100 {
			return newRule{}, fmt.Errorf("how often must be between 0 and 100 percent")
		}
		out.Probability = percent / 100
	}

	if raw := r.PostFormValue("times_left"); raw != "" {
		times, err := strconv.Atoi(raw)
		if err != nil || times < 1 {
			return newRule{}, fmt.Errorf("number of uses must be 1 or more")
		}
		out.TimesLeft = &times
	}

	return out, nil
}

// parseOutcome turns the chosen result into the action that produces it.
//
// The form offers one list — success, timeout, then the errors the method can
// return — because that is how the outcome is thought about; which fault action
// implements it is this function's business, not the operator's.
func parseOutcome(outcome string) (faultdomain.Action, payerr.Code, error) {
	switch outcome {
	case outcomeSuccess:
		// Passthrough is a rule that fires and does nothing, which is how one
		// method is exempted from a wildcard rule that breaks the rest.
		return faultdomain.ActionPassthrough, 0, nil

	case outcomeTimeout:
		// A timeout is the connection closing with no answer, held open first
		// for however long the operator asked.
		return faultdomain.ActionDrop, 0, nil

	case "":
		return "", 0, fmt.Errorf("pick what the method should return")
	}

	code, err := strconv.Atoi(outcome)
	if err != nil {
		return "", 0, fmt.Errorf("pick what the method should return")
	}

	return faultdomain.ActionRPCError, payerr.Code(code), nil
}

// parseAction reads the seconds an outcome needs. A timeout asks for them
// outright; anything else may take them to answer slowly but correctly.
func parseAction(r *http.Request, action faultdomain.Action, out *newRule) error {
	raw := r.PostFormValue("delay_seconds")

	if action == faultdomain.ActionDrop && raw == "" {
		// A timeout with no wait closes the connection at once, which is a
		// refused connection rather than the stall being rehearsed.
		return fmt.Errorf("say how many seconds to wait before timing out")
	}

	if raw == "" {
		return nil
	}

	seconds, err := parseSeconds(raw)
	if err != nil {
		return err
	}
	out.DelaySeconds = seconds

	return nil
}

// parseSeconds reads a wait in seconds, which is how a stalled provider is
// described; the database keeps milliseconds.
func parseSeconds(raw string) (float64, error) {
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds < 0 {
		return 0, fmt.Errorf("seconds must be a positive number")
	}

	// A wait longer than an hour would hold a connection open past every
	// client timeout, which is a mistake rather than a scenario.
	if seconds > 3600 {
		return 0, fmt.Errorf("seconds must be 3600 or less")
	}

	return seconds, nil
}

// serviceOr defaults to the merchant side, which is the half a stand is
// pointed at first.
func serviceOr(service string) string {
	switch service {
	case serviceMerchant, servicePaymeMock:
		return service
	default:
		return serviceMerchant
	}
}

// methodOr defaults to every method, which is the common case: a stand is
// broken as a whole more often than one call at a time.
func methodOr(method string) string {
	if method == "" {
		return "*"
	}
	return method
}
