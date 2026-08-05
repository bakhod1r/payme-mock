package domain_test

import (
	"encoding/base64"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/checkout/domain"
)

// The example the documentation gives, verbatim.
const (
	docParams    = "m=587f72c72cac0d162c722ae2;ac.order_id=197;a=500"
	docEncoded   = "bT01ODdmNzJjNzJjYWMwZDE2MmM3MjJhZTI7YWMub3JkZXJfaWQ9MTk3O2E9NTAw"
	docMerchant  = "587f72c72cac0d162c722ae2"
	docCheckout  = "https://checkout.paycom.uz"
	docOrderID   = "197"
	docAmountSum = int64(500)
)

func encode(params string) string {
	return base64.StdEncoding.EncodeToString([]byte(params))
}

// The documented parameter string must decode to the documented URL and back.
func TestDocumentedExampleRoundTrips(t *testing.T) {
	require.Equal(t, docEncoded, encode(docParams), "the documented base64 must match")

	got, err := domain.ParseGET(docEncoded)

	require.NoError(t, err)
	assert.Equal(t, docMerchant, got.MerchantID)
	assert.Equal(t, docAmountSum, got.Amount)
	assert.Equal(t, docOrderID, got.Account["order_id"])

	assert.Equal(t, docEncoded, got.EncodeGET(), "re-encoding must reproduce the documented string")
	assert.Equal(t, docCheckout+"/"+docEncoded, got.CheckoutURL(docCheckout))
}

func TestParseGET(t *testing.T) {
	t.Run("reads every documented key", func(t *testing.T) {
		params := "m=" + docMerchant + ";ac.order_id=197;ac.phone=901234567;a=500000;l=uz;c=https://shop.uz/return;ct=5000;cr=860"

		got, err := domain.ParseGET(encode(params))

		require.NoError(t, err)
		assert.Equal(t, docMerchant, got.MerchantID)
		assert.Equal(t, int64(500000), got.Amount)
		assert.Equal(t, domain.LangUZ, got.Lang)
		assert.Equal(t, "https://shop.uz/return", got.Callback)
		assert.Equal(t, 5000, got.CallbackTimeout)
		assert.Equal(t, 860, got.Currency)
		assert.Equal(t, map[string]string{"order_id": "197", "phone": "901234567"}, got.Account)
	})

	t.Run("ignores an unknown key rather than refusing", func(t *testing.T) {
		got, err := domain.ParseGET(encode(docParams + ";future_key=whatever"))

		require.NoError(t, err)
		assert.Equal(t, docMerchant, got.MerchantID)
	})

	t.Run("tolerates a trailing separator", func(t *testing.T) {
		got, err := domain.ParseGET(encode(docParams + ";"))

		require.NoError(t, err)
		assert.Equal(t, docAmountSum, got.Amount)
	})

	t.Run("tolerates surrounding whitespace", func(t *testing.T) {
		got, err := domain.ParseGET("  " + docEncoded + "  ")

		require.NoError(t, err)
		assert.Equal(t, docMerchant, got.MerchantID)
	})

	rejects := []struct {
		name   string
		params string
		raw    string
	}{
		{name: "not base64", raw: "!!! not base64 !!!"},
		{name: "a pair without an equals sign", params: "m=x;nonsense;a=1"},
		{name: "an amount that is not a number", params: "m=x;ac.order_id=1;a=lots"},
		{name: "a timeout that is not a number", params: "m=x;ac.order_id=1;a=1;ct=soon"},
		{name: "a currency that is not a number", params: "m=x;ac.order_id=1;a=1;cr=som"},
		{name: "an account field with no name", params: "m=x;ac.=1;a=1"},
		{name: "no merchant", params: "ac.order_id=1;a=1"},
		{name: "no account", params: "m=x;a=1"},
		{name: "a zero amount", params: "m=x;ac.order_id=1;a=0"},
		{name: "a negative amount", params: "m=x;ac.order_id=1;a=-5"},
	}

	for _, tt := range rejects {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			input := tt.raw
			if input == "" {
				input = encode(tt.params)
			}

			_, err := domain.ParseGET(input)

			assert.ErrorIs(t, err, domain.ErrMalformed)
		})
	}
}

func TestParsePOST(t *testing.T) {
	t.Run("reads every documented field", func(t *testing.T) {
		detail := domain.EncodeDetail(&domain.Detail{
			Items: []domain.Item{{Title: "Coffee", Price: 25000, Count: 2, Code: "00702001001000001"}},
		})

		form := url.Values{
			"merchant":          {docMerchant},
			"amount":            {"500000"},
			"account[order_id]": {"197"},
			"account[phone]":    {"901234567"},
			"lang":              {"en"},
			"callback":          {"https://shop.uz/return?tx=:transaction"},
			"callback_timeout":  {"15"},
			"description":       {"Order 197"},
			"detail":            {detail},
		}

		got, err := domain.ParsePOST(form)

		require.NoError(t, err)
		assert.Equal(t, docMerchant, got.MerchantID)
		assert.Equal(t, int64(500000), got.Amount)
		assert.Equal(t, domain.LangEN, got.Lang)
		assert.Equal(t, 15, got.CallbackTimeout)
		assert.Equal(t, "Order 197", got.Description)
		assert.Equal(t, map[string]string{"order_id": "197", "phone": "901234567"}, got.Account)
		require.NotNil(t, got.Detail)
		assert.Equal(t, "Coffee", got.Detail.Items[0].Title)
	})

	t.Run("ignores a field that is not an account entry", func(t *testing.T) {
		form := url.Values{
			"merchant":          {docMerchant},
			"amount":            {"500"},
			"account[order_id]": {"197"},
			"account[":          {"unterminated"},
			"account[]":         {"empty"},
			"unrelated":         {"x"},
		}

		got, err := domain.ParsePOST(form)

		require.NoError(t, err)
		assert.Equal(t, map[string]string{"order_id": "197"}, got.Account)
	})

	rejects := []struct {
		name string
		form url.Values
	}{
		{"an amount that is not a number", url.Values{"merchant": {"x"}, "amount": {"lots"}, "account[order_id]": {"1"}}},
		{"a timeout that is not a number", url.Values{"merchant": {"x"}, "amount": {"1"}, "account[order_id]": {"1"}, "callback_timeout": {"soon"}}},
		{"a detail that is not base64", url.Values{"merchant": {"x"}, "amount": {"1"}, "account[order_id]": {"1"}, "detail": {"!!!"}}},
		{"a detail that is not JSON", url.Values{"merchant": {"x"}, "amount": {"1"}, "account[order_id]": {"1"}, "detail": {base64.StdEncoding.EncodeToString([]byte("nope"))}}},
		{"no merchant", url.Values{"amount": {"1"}, "account[order_id]": {"1"}}},
		{"no amount", url.Values{"merchant": {"x"}, "account[order_id]": {"1"}}},
		{"no account", url.Values{"merchant": {"x"}, "amount": {"1"}}},
	}

	for _, tt := range rejects {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := domain.ParsePOST(tt.form)

			assert.ErrorIs(t, err, domain.ErrMalformed)
		})
	}
}

// The callback placeholder is how the merchant learns which payment finished.
func TestResolveCallback(t *testing.T) {
	tests := []struct {
		name     string
		callback string
		want     string
	}{
		{
			name:     "substitutes the transaction placeholder",
			callback: "https://shop.uz/return?tx=:transaction",
			want:     "https://shop.uz/return?tx=5305e3bab097f420a62ced0b",
		},
		{
			name:     "substitutes every occurrence",
			callback: "https://shop.uz/:transaction/done?id=:transaction",
			want:     "https://shop.uz/5305e3bab097f420a62ced0b/done?id=5305e3bab097f420a62ced0b",
		},
		{
			name:     "leaves a callback without a placeholder alone",
			callback: "https://shop.uz/return",
			want:     "https://shop.uz/return",
		},
		{name: "an absent callback stays empty", callback: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &domain.Receipt{Callback: tt.callback}

			assert.Equal(t, tt.want, r.ResolveCallback("5305e3bab097f420a62ced0b"))
		})
	}
}

func TestLanguageValid(t *testing.T) {
	for _, l := range []domain.Language{domain.LangRU, domain.LangUZ, domain.LangEN} {
		assert.True(t, l.Valid(), "%q is documented", l)
	}
	assert.False(t, domain.Language("fr").Valid())
	assert.False(t, domain.Language("").Valid())
}

// Encoding sorts account fields, so the same receipt always yields the same
// URL rather than one that changes with map iteration order.
func TestEncodeGETIsStable(t *testing.T) {
	r := &domain.Receipt{
		MerchantID: docMerchant,
		Account:    map[string]string{"z": "1", "a": "2", "m": "3"},
		Amount:     500,
	}

	first := r.EncodeGET()
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, r.EncodeGET())
	}

	decoded, err := base64.StdEncoding.DecodeString(first)
	require.NoError(t, err)
	assert.Equal(t, "m="+docMerchant+";ac.a=2;ac.m=3;ac.z=1;a=500", string(decoded))
}

// A receipt carrying every key must survive the round trip unchanged, so a
// link the console generates decodes back to the receipt it was built from.
func TestEncodeGETRoundTripsEveryKey(t *testing.T) {
	original := &domain.Receipt{
		MerchantID:      docMerchant,
		Account:         map[string]string{"order_id": "197", "phone": "901234567"},
		Amount:          500000,
		Lang:            domain.LangUZ,
		Callback:        "https://shop.uz/return",
		CallbackTimeout: 5000,
		Currency:        860,
	}

	got, err := domain.ParseGET(original.EncodeGET())

	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestEncodeGETOmitsUnsetOptionalKeys(t *testing.T) {
	r := &domain.Receipt{MerchantID: "x", Account: map[string]string{"order_id": "1"}, Amount: 500}

	decoded, err := base64.StdEncoding.DecodeString(r.EncodeGET())

	require.NoError(t, err)
	assert.Equal(t, "m=x;ac.order_id=1;a=500", string(decoded))
}

func TestCheckoutURLTrimsATrailingSlash(t *testing.T) {
	r := &domain.Receipt{MerchantID: "x", Account: map[string]string{"o": "1"}, Amount: 1}

	assert.Equal(t, r.CheckoutURL(docCheckout), r.CheckoutURL(docCheckout+"/"))
}
