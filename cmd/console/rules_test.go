package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	faultdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// postForm builds the request the rule form submits.
func postForm(values url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/rules", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestParseRuleFormReadsAnError(t *testing.T) {
	rule, err := parseRuleForm(postForm(url.Values{
		"method":  {"PerformTransaction"},
		"service": {"merchant"},
		"outcome": {"-31008"},
	}))
	require.NoError(t, err)

	assert.Equal(t, faultdomain.ActionRPCError, rule.Action)
	assert.Equal(t, payerr.Code(-31008), rule.ErrorCode)
	assert.Equal(t, "PerformTransaction", rule.Method)
	assert.Equal(t, float64(1), rule.Probability, "a rule fires every time unless told otherwise")
	assert.Nil(t, rule.SandboxID, "no sandbox means every sandbox")
	assert.Nil(t, rule.TimesLeft, "no count means forever")
}

// Success is a rule that fires and does nothing, which is how one method is
// exempted from a wildcard that breaks the rest.
func TestParseRuleFormReadsSuccess(t *testing.T) {
	rule, err := parseRuleForm(postForm(url.Values{"outcome": {outcomeSuccess}}))
	require.NoError(t, err)

	assert.Equal(t, faultdomain.ActionPassthrough, rule.Action)
	assert.Zero(t, rule.ErrorCode)
	assert.Equal(t, everyMethod, rule.Method, "no method means every method")
	assert.Equal(t, serviceMerchant, rule.Service)
}

func TestParseRuleFormReadsATimeout(t *testing.T) {
	rule, err := parseRuleForm(postForm(url.Values{
		"outcome":       {outcomeTimeout},
		"delay_seconds": {"12.5"},
	}))
	require.NoError(t, err)

	assert.Equal(t, faultdomain.ActionDrop, rule.Action)
	assert.InDelta(t, 12.5, rule.DelaySeconds, 0.0001)
}

// A timeout with no wait closes the connection at once, which rehearses a
// refused connection rather than a stall.
func TestParseRuleFormRequiresSecondsForATimeout(t *testing.T) {
	_, err := parseRuleForm(postForm(url.Values{"outcome": {outcomeTimeout}}))

	assert.ErrorContains(t, err, "say how many seconds")
}

func TestParseRuleFormReadsTheSandbox(t *testing.T) {
	rule, err := parseRuleForm(postForm(url.Values{
		"outcome":    {outcomeSuccess},
		"sandbox_id": {"42"},
	}))
	require.NoError(t, err)

	require.NotNil(t, rule.SandboxID)
	assert.Equal(t, int64(42), *rule.SandboxID)
}

func TestParseRuleFormRejectsAnUnreadableSandbox(t *testing.T) {
	_, err := parseRuleForm(postForm(url.Values{
		"outcome":    {outcomeSuccess},
		"sandbox_id": {"seven"},
	}))

	assert.ErrorContains(t, err, "unknown sandbox")
}

func TestParseRuleFormReadsProbabilityAsAPercentage(t *testing.T) {
	rule, err := parseRuleForm(postForm(url.Values{
		"outcome":     {outcomeSuccess},
		"probability": {"25"},
	}))
	require.NoError(t, err)

	assert.InDelta(t, 0.25, rule.Probability, 0.0001)
}

func TestParseRuleFormRejectsAnImpossibleProbability(t *testing.T) {
	for _, raw := range []string{"-1", "101", "often"} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseRuleForm(postForm(url.Values{
				"outcome":     {outcomeSuccess},
				"probability": {raw},
			}))

			assert.ErrorContains(t, err, "between 0 and 100")
		})
	}
}

func TestParseRuleFormReadsAUseCount(t *testing.T) {
	rule, err := parseRuleForm(postForm(url.Values{
		"outcome":    {outcomeSuccess},
		"times_left": {"3"},
	}))
	require.NoError(t, err)

	require.NotNil(t, rule.TimesLeft)
	assert.Equal(t, 3, *rule.TimesLeft)
}

func TestParseRuleFormRejectsAnImpossibleUseCount(t *testing.T) {
	for _, raw := range []string{"0", "-2", "twice"} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseRuleForm(postForm(url.Values{
				"outcome":    {outcomeSuccess},
				"times_left": {raw},
			}))

			assert.ErrorContains(t, err, "1 or more")
		})
	}
}

func TestParseRuleFormRejectsAnUnreadableDelay(t *testing.T) {
	_, err := parseRuleForm(postForm(url.Values{
		"outcome":       {outcomeSuccess},
		"delay_seconds": {"soon"},
	}))

	assert.ErrorContains(t, err, "seconds must be a positive number")
}

func TestParseOutcomeDemandsAChoice(t *testing.T) {
	for _, raw := range []string{"", "whatever"} {
		_, _, err := parseOutcome(raw)
		assert.ErrorContains(t, err, "pick what the method should return")
	}
}

func TestParseSeconds(t *testing.T) {
	seconds, err := parseSeconds("2.5")
	require.NoError(t, err)
	assert.InDelta(t, 2.5, seconds, 0.0001)

	_, err = parseSeconds("-1")
	assert.ErrorContains(t, err, "positive")

	// A wait past every client timeout is a mistake, not a scenario.
	_, err = parseSeconds("3601")
	assert.ErrorContains(t, err, "3600 or less")
}

func TestServiceOrDefaultsToTheMerchantSide(t *testing.T) {
	assert.Equal(t, serviceMerchant, serviceOr(""))
	assert.Equal(t, serviceMerchant, serviceOr("nonsense"))
	assert.Equal(t, serviceMerchant, serviceOr(serviceMerchant))
	assert.Equal(t, servicePaymeMock, serviceOr(servicePaymeMock))
}

func TestMethodOrDefaultsToTheWildcard(t *testing.T) {
	assert.Equal(t, everyMethod, methodOr(""))
	assert.Equal(t, "CheckTransaction", methodOr("CheckTransaction"))
}

func TestRuleNameSaysWhatTheRuleDoes(t *testing.T) {
	tests := []struct {
		name string
		rule newRule
		want string
	}{
		{"error", newRule{Method: "PerformTransaction", Action: faultdomain.ActionRPCError, ErrorCode: -31008},
			"PerformTransaction returns -31008"},
		{"delay", newRule{Method: "CheckTransaction", Action: faultdomain.ActionDelay, DelaySeconds: 2.5},
			"CheckTransaction waits 2.5s"},
		{"status", newRule{Method: "CreateTransaction", Action: faultdomain.ActionHTTPStatus, HTTPStatus: 502},
			"CreateTransaction answers HTTP 502"},
		{"timeout", newRule{Method: "GetStatement", Action: faultdomain.ActionDrop},
			"GetStatement times out"},
		{"malformed", newRule{Method: "CheckTransaction", Action: faultdomain.ActionMalformed},
			"CheckTransaction returns broken JSON"},
		{"duplicate", newRule{Method: "CreateTransaction", Action: faultdomain.ActionDuplicate},
			"CreateTransaction is delivered twice"},
		{"passthrough", newRule{Method: "CheckTransaction", Action: faultdomain.ActionPassthrough},
			"CheckTransaction"},
		{"the wildcard reads as words", newRule{Method: everyMethod, Action: faultdomain.ActionDrop},
			"every method times out"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ruleName(tt.rule))
		})
	}
}

func TestDescribeAction(t *testing.T) {
	code := -31008
	status := 500
	twice := 2

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"error", describeAction("rpc_error", 0, &code, nil, 1, nil), "returns error -31008"},
		{"delay", describeAction("delay", 2500, nil, nil, 1, nil), "waits 2.5s"},
		{"status", describeAction("http_status", 0, nil, &status, 1, nil), "answers HTTP 500"},
		{"timeout", describeAction("drop", 0, nil, nil, 1, nil), "closes the connection"},
		{"malformed", describeAction("malformed", 0, nil, nil, 1, nil), "returns broken JSON"},
		{"duplicate", describeAction("duplicate", 0, nil, nil, 1, nil), "delivers the request twice"},
		{"passthrough", describeAction("passthrough", 0, nil, nil, 1, nil), "lets the request through"},
		{"a delay before another action is spelled out",
			describeAction("drop", 3000, nil, nil, 1, nil), "closes the connection, after 3s"},
		{"a rule that fires sometimes says so",
			describeAction("passthrough", 0, nil, nil, 0.25, nil), "lets the request through, 25% of the time"},
		{"a rule with a count says how many are left",
			describeAction("passthrough", 0, nil, nil, 1, &twice), "lets the request through, 2 use(s) left"},
		{"a missing code reads as zero rather than crashing",
			describeAction("rpc_error", 0, nil, nil, 1, nil), "returns error 0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

// The edit dialog opens on what the rule does now, so a stored action has to
// map back to the word the form offered.
func TestOutcomeOf(t *testing.T) {
	code := -31003

	assert.Equal(t, outcomeTimeout, outcomeOf("drop", nil))
	assert.Equal(t, "-31003", outcomeOf("rpc_error", &code))
	assert.Equal(t, outcomeSuccess, outcomeOf("rpc_error", nil), "an error rule with no code returns nothing special")
	assert.Equal(t, outcomeSuccess, outcomeOf("passthrough", nil))
}

func TestTrimZero(t *testing.T) {
	assert.Equal(t, "2", trimZero(2))
	assert.Equal(t, "2.5", trimZero(2.5))
	assert.Equal(t, "0", trimZero(0))
}

func TestDerefInt(t *testing.T) {
	seven := 7

	assert.Equal(t, 7, derefInt(&seven))
	assert.Zero(t, derefInt(nil))
}

// A rule carries its own copy of the message so editing the catalog later
// cannot change what a running scenario answers with.
func TestCatalogMessageCopiesTheDocumentedText(t *testing.T) {
	raw := catalogMessage(payerr.CodeCannotPerform)

	var message payerr.Message
	require.NoError(t, json.Unmarshal(raw, &message))
	assert.NotEmpty(t, message.EN)
}

func TestCatalogMessageForAnUnknownCodeIsEmpty(t *testing.T) {
	assert.JSONEq(t, `{}`, string(catalogMessage(payerr.Code(-1))))
}

func TestSlugFor(t *testing.T) {
	assert.Equal(t, "cannot_perform", slugFor(payerr.CodeCannotPerform))
	assert.Equal(t, "account_not_found", slugFor(payerr.CodeAccountMax))
	assert.Equal(t, "error_31099", slugFor(payerr.Code(-31099)), "an unnamed code is named after itself")
}

func TestScopeFor(t *testing.T) {
	assert.Equal(t, "merchant", scopeFor(payerr.CodeCannotPerform))
	assert.Equal(t, "merchant", scopeFor(payerr.CodeInvalidAmount))
	assert.Equal(t, "general", scopeFor(payerr.CodeUnauthorized))
}
