package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// The dropdown mirrors the documentation: both halves of the protocol, each
// method under the API it belongs to.
func TestBuildMethodGroupsCoversBothAPIs(t *testing.T) {
	groups := buildMethodGroups([]errorRow{{Code: -31008, Message: "Cannot perform"}})

	require.Len(t, groups, 2)
	assert.Equal(t, "Merchant API", groups[0].Label)
	assert.Equal(t, "Subscribe API", groups[1].Label)
	assert.Len(t, groups[0].Methods, len(merchantMethods))
	assert.Len(t, groups[1].Methods, len(subscribeMethods))
}

func TestMethodGroupsCarryTheServiceEachMethodBelongsTo(t *testing.T) {
	groups := buildMethodGroups(nil)

	for _, method := range groups[0].Methods {
		assert.Equal(t, serviceMerchant, method.Service, method.Name)
	}
	for _, method := range groups[1].Methods {
		assert.Equal(t, servicePaymeMock, method.Service, method.Name)
	}
}

// Success and timeout are not errors, and they open every list because between
// them they cover most of what an integration rehearses.
func TestEveryMethodOffersSuccessAndTimeoutFirst(t *testing.T) {
	for _, group := range buildMethodGroups(nil) {
		for _, method := range group.Methods {
			require.GreaterOrEqual(t, len(method.Outcomes), 2, method.Name)
			assert.Equal(t, outcomeSuccess, method.Outcomes[0].Value)
			assert.Equal(t, outcomeTimeout, method.Outcomes[1].Value)
		}
	}
}

// Offering an error a method cannot produce would set up a scenario the real
// provider never answers with.
func TestAMethodOffersOnlyTheErrorsItCanReturn(t *testing.T) {
	groups := buildMethodGroups(nil)

	var check methodOption
	for _, method := range groups[0].Methods {
		if method.Name == "CheckPerformTransaction" {
			check = method
		}
	}
	require.NotEmpty(t, check.Name)

	var offered []string
	for _, outcome := range check.Outcomes {
		offered = append(offered, outcome.Value)
	}

	assert.Contains(t, offered, "-31008", "it can refuse the operation")
	assert.NotContains(t, offered, "-31003",
		"there is no transaction yet, so it cannot report one missing")
}

// With no method chosen, narrowing has not happened yet, so everything is on
// offer.
func TestTheWildcardOffersTheWholeCatalog(t *testing.T) {
	codes := codesFor(everyMethod)

	assert.Len(t, codes, len(payerr.Catalog))
}

func TestCodesForAMethodEndWithTheGeneralFailures(t *testing.T) {
	codes := codesFor("CheckTransaction")

	assert.Contains(t, codes, payerr.CodeTransactionNotFnd)
	for _, general := range generalErrors {
		assert.Contains(t, codes, general)
	}
}

func TestCodesForAnUnknownMethodStillOffersTheGeneralFailures(t *testing.T) {
	assert.Equal(t, generalErrors, codesFor("NoSuchMethod"))
}

// The catalog's text is what the operator reads beside a code.
func TestOutcomeLabelsCarryTheCatalogMessage(t *testing.T) {
	groups := buildMethodGroups([]errorRow{{Code: -31008, Message: "Cannot perform"}})

	var labelled bool
	for _, method := range groups[0].Methods {
		for _, outcome := range method.Outcomes {
			if outcome.Value == "-31008" {
				assert.Equal(t, "-31008 · Cannot perform", outcome.Label)
				labelled = true
			}
		}
	}

	assert.True(t, labelled, "the catalog message must reach the dropdown")
}

func TestMethodLabel(t *testing.T) {
	assert.Equal(t, "every method", methodLabel(everyMethod))
	assert.Equal(t, "CheckTransaction", methodLabel("CheckTransaction"))
}

func TestFormatCode(t *testing.T) {
	assert.Equal(t, "-31008", formatCode(payerr.Code(-31008)))
}

func TestItoa(t *testing.T) {
	assert.Equal(t, "0", itoa(0))
	assert.Equal(t, "7", itoa(7))
	assert.Equal(t, "31008", itoa(31008))
	assert.Equal(t, "-31008", itoa(-31008))
}

// The register kinds are what say which way a stand moves money, so the form
// offers every one the domain defines.
func TestRegisterKinds(t *testing.T) {
	kinds := registerKinds()

	require.Len(t, kinds, len(billing.Kinds))
	for i, kind := range billing.Kinds {
		assert.Equal(t, string(kind), kinds[i].Value)
		assert.Equal(t, kind.Describe(), kinds[i].Meaning)
	}
}

// The dropdown carries the four documented states plus the two groupings the
// filter offers on top of them. Only the states may be forced onto a payment;
// "unfinished" and "failed" are questions, not states to move a payment into.
func TestTransactionStatesCoverTheProtocol(t *testing.T) {
	states := transactionStates()

	var settable []stateOption
	for _, state := range states {
		if state.Settable {
			settable = append(settable, state)
		}
	}

	require.Len(t, settable, 4)
	assert.Equal(t, filterUnfinished, states[0].Value, "the groupings come first")
	assert.Equal(t, filterFailed, states[1].Value)
	assert.False(t, states[0].Settable)
	assert.False(t, states[1].Settable)
}

func TestStateLabel(t *testing.T) {
	assert.Equal(t, "created", stateLabel(1))
	assert.Equal(t, "performed", stateLabel(2))
	assert.Equal(t, "cancelled", stateLabel(-1))
	assert.Equal(t, "cancelled after perform", stateLabel(-2))
	assert.Equal(t, "unknown", stateLabel(9))
}

// A recorded body is shown indented, because a wall of one-line JSON is what
// the log is meant to save an operator from.
func TestPrettyJSON(t *testing.T) {
	assert.Equal(t, "{\n  \"a\": 1\n}", prettyJSON(`{"a":1}`))
	assert.Empty(t, prettyJSON(""))

	// Broken output is what this stand produces on purpose; reformatting it
	// would hide what was actually sent.
	assert.Equal(t, "not json at all", prettyJSON("not json at all"))

	// A payload the column could not hold is stored as a JSON string, and
	// reads better unquoted than as one escaped line.
	assert.Equal(t, "raw body", prettyJSON(`"raw body"`))
}
