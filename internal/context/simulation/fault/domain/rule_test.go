package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/fault/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }

// enabledRule is a rule that matches everything, so each test can constrain
// exactly one dimension and see its effect in isolation.
func enabledRule() *domain.Rule {
	return &domain.Rule{
		ID:          1,
		Enabled:     true,
		Service:     domain.ServiceAny,
		Method:      "*",
		Action:      domain.ActionRPCError,
		Probability: 1,
	}
}

func baseRequest() domain.Request {
	return domain.Request{
		Service:   domain.ServiceMerchant,
		Method:    "PerformTransaction",
		SandboxID: 1,
		Account:   map[string]string{"phone": "901234567"},
		PaymeID:   "5305e3bab097f420a62ced0b",
		Amount:    500000,
	}
}

func TestActionValid(t *testing.T) {
	valid := []domain.Action{
		domain.ActionDelay, domain.ActionRPCError, domain.ActionHTTPStatus,
		domain.ActionMalformed, domain.ActionDrop, domain.ActionDuplicate,
		domain.ActionPassthrough,
	}
	for _, a := range valid {
		assert.True(t, a.Valid(), "action %q should be valid", a)
	}

	assert.False(t, domain.Action("explode").Valid())
	assert.False(t, domain.Action("").Valid())
}

func TestRuleMatchesService(t *testing.T) {
	tests := []struct {
		name    string
		service domain.Service
		want    bool
	}{
		{"wildcard matches", domain.ServiceAny, true},
		{"exact match", domain.ServiceMerchant, true},
		{"other service does not match", domain.ServicePaymeMock, false},
		{"gateway does not match", domain.ServiceGateway, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := enabledRule()
			r.Service = tt.service

			assert.Equal(t, tt.want, r.Matches(baseRequest()))
		})
	}
}

func TestRuleMatchesMethodGlob(t *testing.T) {
	tests := []struct {
		pattern string
		method  string
		want    bool
	}{
		{"*", "PerformTransaction", true},
		{"", "PerformTransaction", true},
		{"PerformTransaction", "PerformTransaction", true},
		{"PerformTransaction", "CancelTransaction", false},
		{"receipts.*", "receipts.pay", true},
		{"receipts.*", "receipts.create", true},
		{"receipts.*", "cards.create", false},
		{"cards.*", "cards.get_verify_code", true},
		{"*Transaction", "PerformTransaction", true},
		{"[", "anything", false}, // a malformed pattern must not match everything
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.method, func(t *testing.T) {
			r := enabledRule()
			r.Method = tt.pattern
			req := baseRequest()
			req.Method = tt.method

			assert.Equal(t, tt.want, r.Matches(req))
		})
	}
}

func TestRuleMatchesAccount(t *testing.T) {
	tests := []struct {
		name    string
		match   map[string]string
		account map[string]string
		want    bool
	}{
		{"no constraint matches anything", nil, map[string]string{"phone": "901234567"}, true},
		{"exact value", map[string]string{"phone": "901234567"}, map[string]string{"phone": "901234567"}, true},
		{"glob prefix", map[string]string{"phone": "9012*"}, map[string]string{"phone": "901234567"}, true},
		{"glob mismatch", map[string]string{"phone": "9999*"}, map[string]string{"phone": "901234567"}, false},
		{"missing field", map[string]string{"login": "bob"}, map[string]string{"phone": "901234567"}, false},
		{
			name:    "every field must match",
			match:   map[string]string{"phone": "901234567", "login": "bob"},
			account: map[string]string{"phone": "901234567"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := enabledRule()
			r.MatchAccount = tt.match
			req := baseRequest()
			req.Account = tt.account

			assert.Equal(t, tt.want, r.Matches(req))
		})
	}
}

func TestRuleMatchesAmountRange(t *testing.T) {
	tests := []struct {
		name   string
		min    *int64
		max    *int64
		amount int64
		want   bool
	}{
		{"no bounds", nil, nil, 500000, true},
		{"inside both bounds", ptrInt64(100), ptrInt64(1000000), 500000, true},
		{"at the lower bound", ptrInt64(500000), nil, 500000, true},
		{"at the upper bound", nil, ptrInt64(500000), 500000, true},
		{"below the lower bound", ptrInt64(500001), nil, 500000, false},
		{"above the upper bound", nil, ptrInt64(499999), 500000, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := enabledRule()
			r.AmountMin, r.AmountMax = tt.min, tt.max
			req := baseRequest()
			req.Amount = tt.amount

			assert.Equal(t, tt.want, r.Matches(req))
		})
	}
}

func TestRuleMatchesPaymeID(t *testing.T) {
	r := enabledRule()
	r.MatchPaymeID = "5305e3bab097f420a62ced0b"

	assert.True(t, r.Matches(baseRequest()))

	other := baseRequest()
	other.PaymeID = "different"
	assert.False(t, r.Matches(other))
}

func TestRuleMatchesSandbox(t *testing.T) {
	t.Run("a rule scoped to a sandbox ignores others", func(t *testing.T) {
		r := enabledRule()
		r.SandboxID = ptrInt64(2)

		assert.False(t, r.Matches(baseRequest()))

		req := baseRequest()
		req.SandboxID = 2
		assert.True(t, r.Matches(req))
	})

	t.Run("an unscoped rule applies to every sandbox", func(t *testing.T) {
		r := enabledRule()

		req := baseRequest()
		req.SandboxID = 99
		assert.True(t, r.Matches(req))
	})
}

func TestDisabledRuleNeverMatches(t *testing.T) {
	r := enabledRule()
	r.Enabled = false

	assert.False(t, r.Matches(baseRequest()))
}

func TestRuleExhausted(t *testing.T) {
	tests := []struct {
		name  string
		times *int
		want  bool
	}{
		{"unlimited", nil, false},
		{"uses remaining", ptrInt(1), false},
		{"spent", ptrInt(0), true},
		{"over-spent", ptrInt(-1), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := enabledRule()
			r.TimesLeft = tt.times

			assert.Equal(t, tt.want, r.Exhausted())
		})
	}
}

func TestRuleProtocolError(t *testing.T) {
	t.Run("uses the rule's own message when complete", func(t *testing.T) {
		r := enabledRule()
		r.ErrorCode = payerr.CodeCannotPerform
		r.ErrorMessage = payerr.NewMessage("свой", "o'ziniki", "custom")

		got := r.ProtocolError()

		assert.Equal(t, payerr.CodeCannotPerform, got.Code)
		assert.Equal(t, "custom", got.Message.EN)
	})

	t.Run("falls back to the documented message", func(t *testing.T) {
		r := enabledRule()
		r.ErrorCode = payerr.CodeTransactionNotFnd

		got := r.ProtocolError()

		assert.Equal(t, payerr.ErrTransactionNotFound.Message, got.Message)
		assert.True(t, got.Message.Complete())
	})

	t.Run("keeps the account field when falling back", func(t *testing.T) {
		r := enabledRule()
		r.ErrorCode = payerr.CodeAccountMax
		r.ErrorData = "phone"

		got := r.ProtocolError()

		assert.Equal(t, "phone", got.Data)
		assert.True(t, got.Message.Complete())
	})

	t.Run("an unknown code still answers with all three languages", func(t *testing.T) {
		r := enabledRule()
		r.ErrorCode = payerr.Code(-31234)

		got := r.ProtocolError()

		assert.Equal(t, payerr.Code(-31234), got.Code)
		require.True(t, got.Message.Complete(), "responses must never ship an empty translation")
	})

	t.Run("a partial message does not leak an empty translation", func(t *testing.T) {
		r := enabledRule()
		r.ErrorCode = payerr.CodeCannotPerform
		r.ErrorMessage = payerr.NewMessage("только русский", "", "")

		got := r.ProtocolError()

		assert.True(t, got.Message.Complete())
	})
}
