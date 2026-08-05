package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/application"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	subhttp "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/interfaces/http"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

const (
	merchantID = "587f72c72cac0d162c722ae2"
	testKey    = "5NPh3f4rTLPa0Vk1LOZ8AT8gK4EbAqTaPHnk"
	cardNumber = "8600069195406311"
	// verifyCode is the OTP that card takes: its expiry with the year repeated.
	verifyCode = "039999"
)

func newHandler(t *testing.T, resolve subhttp.CredentialResolver) http.Handler {
	t.Helper()

	svc := application.NewService(
		newMemCards(), newMemReceipts(), &noopMerchant{}, &noopScheduler{},
		&seqTokens{}, &noopSMS{},
		clock.NewFake(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)),
		application.Settings{
			SandboxID: 1, MerchantID: merchantID,
			VerifyCode: verifyCode, VerifyWaitMillis: domain.DefaultVerifyWaitMillis,
			CardBalance: 100_000_000,
		},
	)

	return subhttp.NewHandler(svc, resolve)
}

func staticCreds(context.Context) (subhttp.Credentials, error) {
	return subhttp.Credentials{MerchantID: merchantID, Key: testKey}, nil
}

func post(t *testing.T, h http.Handler, body, auth string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("X-Auth", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) payerr.Code {
	t.Helper()

	errObj, ok := decode(t, rec)["error"].(map[string]any)
	require.True(t, ok, "expected an error object, got %s", rec.Body.String())
	return payerr.Code(errObj["code"].(float64))
}

func backendAuth() string { return merchantID + ":" + testKey }
func browserAuth() string { return merchantID }

// Every method is registered; none answers "method not found".
func TestEveryMethodIsRegistered(t *testing.T) {
	h := newHandler(t, staticCreds)

	methods := []string{
		"cards.create", "cards.get_verify_code", "cards.verify", "cards.check", "cards.remove",
		"receipts.create", "receipts.pay", "receipts.send", "receipts.cancel",
		"receipts.check", "receipts.get", "receipts.get_all", "receipts.confirm_hold",
		"transactions.create", "transactions.complete",
		"accounts.getBalance",
	}
	require.Len(t, methods, 16)

	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			rec := post(t, h, `{"id":1,"method":"`+m+`","params":{}}`, backendAuth())

			body := decode(t, rec)
			if errObj, isErr := body["error"].(map[string]any); isErr {
				assert.NotEqual(t, float64(payerr.CodeMethodNotFound), errObj["code"],
					"%s must be registered", m)
			}
		})
	}
}

func TestAuthorization(t *testing.T) {
	h := newHandler(t, staticCreds)
	const body = `{"id":1,"method":"cards.check","params":{"token":"nope"}}`

	tests := []struct {
		name string
		auth string
		want bool // whether the call is refused as unauthorized
	}{
		{"a server-side credential is accepted", backendAuth(), false},
		{"a wrong key is refused", merchantID + ":wrong", true},
		{"a wrong merchant id is refused", "other:" + testKey, true},
		{"a missing header is refused", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, h, body, tt.auth)

			if tt.want {
				assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec))
				return
			}
			assert.NotEqual(t, payerr.CodeUnauthorized, errorCode(t, rec))
		})
	}

	t.Run("refuses when credentials cannot be resolved", func(t *testing.T) {
		failing := newHandler(t, func(context.Context) (subhttp.Credentials, error) {
			return subhttp.Credentials{}, errors.New("no such sandbox")
		})

		rec := post(t, failing, body, backendAuth())

		assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec))
	})
}

// A browser holds only the cash register id, so it may tokenize and verify a
// card but must not reach a method that moves money.
func TestBrowserCredentialReachesOnlyCardEntryMethods(t *testing.T) {
	h := newHandler(t, staticCreds)

	allowed := []string{"cards.create", "cards.get_verify_code", "cards.verify", "cards.check"}
	for _, m := range allowed {
		t.Run("allows "+m, func(t *testing.T) {
			rec := post(t, h, `{"id":1,"method":"`+m+`","params":{}}`, browserAuth())

			body := decode(t, rec)
			if errObj, isErr := body["error"].(map[string]any); isErr {
				assert.NotEqual(t, float64(payerr.CodeUnauthorized), errObj["code"])
			}
		})
	}

	refused := []string{
		"cards.remove", "transactions.create", "transactions.complete",
		"accounts.getBalance", "receipts.create", "receipts.pay",
		"receipts.send", "receipts.cancel", "receipts.check", "receipts.get",
		"receipts.get_all", "receipts.confirm_hold",
	}
	for _, m := range refused {
		t.Run("refuses "+m, func(t *testing.T) {
			rec := post(t, h, `{"id":1,"method":"`+m+`","params":{}}`, browserAuth())

			assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec))
		})
	}
}

func TestBrowserCredentialWithAnUnreadableBodyIsRefused(t *testing.T) {
	h := newHandler(t, staticCreds)

	rec := post(t, h, `{not json`, browserAuth())

	assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec),
		"a call whose method cannot be read must not be treated as browser-safe")
}

func TestRejectsNonPostMethods(t *testing.T) {
	h := newHandler(t, staticCreds)

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set("X-Auth", backendAuth())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	assert.Equal(t, payerr.CodeInvalidHTTP, errorCode(t, rec))
}

func TestBodyReadFailureBecomesTransportError(t *testing.T) {
	h := newHandler(t, staticCreds)

	req := httptest.NewRequest(http.MethodPost, "/api", failingBody{})
	req.Header.Set("X-Auth", backendAuth())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	assert.Equal(t, payerr.CodeTransport, errorCode(t, rec))
}

// The documented card flow, end to end over HTTP.
func TestCardFlowOverHTTP(t *testing.T) {
	h := newHandler(t, staticCreds)

	rec := post(t, h, `{"id":1,"method":"cards.create","params":{"card":{"number":"`+cardNumber+`","expire":"0399"},"save":true}}`, browserAuth())
	card := decode(t, rec)["result"].(map[string]any)["card"].(map[string]any)
	assert.Equal(t, "860006******6311", card["number"], "the full number must never leave the mock")
	assert.Equal(t, "03/99", card["expire"])
	assert.Equal(t, false, card["verify"])
	token := card["token"].(string)

	rec = post(t, h, `{"id":2,"method":"cards.get_verify_code","params":{"token":"`+token+`"}}`, browserAuth())
	code := decode(t, rec)["result"].(map[string]any)
	assert.Equal(t, true, code["sent"])
	assert.Equal(t, float64(60000), code["wait"], "the documented OTP window")

	rec = post(t, h, `{"id":3,"method":"cards.verify","params":{"token":"`+token+`","code":"`+verifyCode+`"}}`, browserAuth())
	card = decode(t, rec)["result"].(map[string]any)["card"].(map[string]any)
	assert.Equal(t, true, card["verify"])
}

// Paying a receipt walks the documented states rather than jumping to paid.
func TestReceiptFlowOverHTTP(t *testing.T) {
	h := newHandler(t, staticCreds)

	rec := post(t, h, `{"id":1,"method":"cards.create","params":{"card":{"number":"`+cardNumber+`","expire":"0399"},"save":true}}`, browserAuth())
	token := decode(t, rec)["result"].(map[string]any)["card"].(map[string]any)["token"].(string)

	post(t, h, `{"id":2,"method":"cards.get_verify_code","params":{"token":"`+token+`"}}`, browserAuth())
	post(t, h, `{"id":3,"method":"cards.verify","params":{"token":"`+token+`","code":"`+verifyCode+`"}}`, browserAuth())

	rec = post(t, h, `{"id":4,"method":"receipts.create","params":{"amount":500000,"account":{"order_id":"197"}}}`, backendAuth())
	receipt := decode(t, rec)["result"].(map[string]any)["receipt"].(map[string]any)
	assert.Equal(t, float64(domain.StateCreated), receipt["state"])
	assert.Equal(t, float64(domain.CurrencyUZS), receipt["currency"])
	receiptID := receipt["_id"].(string)

	rec = post(t, h, `{"id":5,"method":"receipts.pay","params":{"id":"`+receiptID+`","token":"`+token+`"}}`, backendAuth())
	receipt = decode(t, rec)["result"].(map[string]any)["receipt"].(map[string]any)
	assert.Equal(t, float64(domain.StateChecking), receipt["state"],
		"payment starts the walk rather than completing instantly")

	rec = post(t, h, `{"id":6,"method":"receipts.check","params":{"id":"`+receiptID+`"}}`, backendAuth())
	assert.Equal(t, float64(domain.StateChecking), decode(t, rec)["result"].(map[string]any)["state"])
}

func TestProtocolErrorsSurfaceWithTheirCodes(t *testing.T) {
	h := newHandler(t, staticCreds)

	tests := []struct {
		name string
		body string
		want payerr.Code
	}{
		{"unknown receipt", `{"id":1,"method":"receipts.get","params":{"id":"nope"}}`, payerr.CodeTransactionNotFnd},
		{"unknown card", `{"id":1,"method":"cards.check","params":{"token":"nope"}}`, payerr.CodeCannotPerform},
		{"count above fifty", `{"id":1,"method":"receipts.get_all","params":{"count":51,"from":1,"to":2}}`, payerr.CodeInvalidRequest},
		{"unknown method", `{"id":1,"method":"receipts.explode","params":{}}`, payerr.CodeMethodNotFound},
		{"malformed JSON", `{"id":1,`, payerr.CodeParse},
		{"positional params", `{"id":1,"method":"cards.check","params":["token"]}`, payerr.CodeInvalidRequest},
		{"missing params", `{"id":1,"method":"cards.check"}`, payerr.CodeInvalidRequest},
		{"wrongly typed param", `{"id":1,"method":"receipts.get_all","params":{"count":"many"}}`, payerr.CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, h, tt.body, backendAuth())

			assert.Equal(t, tt.want, errorCode(t, rec))
		})
	}
}

func TestErrorsCarryAllThreeLanguages(t *testing.T) {
	h := newHandler(t, staticCreds)

	rec := post(t, h, `{"id":1,"method":"receipts.get","params":{"id":"nope"}}`, backendAuth())

	msg := decode(t, rec)["error"].(map[string]any)["message"].(map[string]any)
	assert.NotEmpty(t, msg["ru"])
	assert.NotEmpty(t, msg["uz"])
	assert.NotEmpty(t, msg["en"])
}

// ---------- test doubles ----------

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingBody) Close() error             { return nil }

type noopMerchant struct{}

func (noopMerchant) CheckPerformTransaction(context.Context, int64, map[string]string) error {
	return nil
}

func (noopMerchant) CreateTransaction(_ context.Context, id string, _, _ int64, _ map[string]string) (string, error) {
	return id, nil
}
func (noopMerchant) PerformTransaction(context.Context, string) error     { return nil }
func (noopMerchant) CancelTransaction(context.Context, string, int) error { return nil }

type noopScheduler struct{}

func (noopScheduler) ScheduleAdvance(context.Context, string, int) error { return nil }

type noopSMS struct{}

func (noopSMS) Send(context.Context, string, string) error { return nil }

type seqTokens struct{ n int }

func (s *seqTokens) CardToken() string     { s.n++; return "token-" + itoa(s.n) }
func (s *seqTokens) ReceiptID() string     { s.n++; return "receipt-" + itoa(s.n) }
func (s *seqTokens) TransactionID() string { s.n++; return "txn-" + itoa(s.n) }

func itoa(n int) string { return string(rune('0' + n%10)) }

type memCards struct {
	byToken map[string]*domain.Card
	byID    map[int64]*domain.Card
	tokens  map[int64]string
	nextID  int64
}

func newMemCards() *memCards {
	return &memCards{byToken: map[string]*domain.Card{}, byID: map[int64]*domain.Card{}, nextID: 1}
}

func (m *memCards) ByToken(_ context.Context, token string) (*domain.Card, error) {
	c, ok := m.byToken[token]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return c, nil
}

func (m *memCards) ByNumber(_ context.Context, number string) (*domain.Card, error) {
	for _, c := range m.byToken {
		if c.NumberFull == number {
			return c, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (m *memCards) ByID(_ context.Context, id int64) (*domain.Card, error) {
	c, ok := m.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return c, nil
}

func (m *memCards) Create(_ context.Context, c *domain.Card) error {
	c.ID = m.nextID
	m.nextID++
	m.byToken[c.Token] = c
	m.byID[c.ID] = c
	return nil
}

func (m *memCards) TokenFor(_ context.Context, cardID int64, offered string) (string, error) {
	if m.tokens == nil {
		m.tokens = map[int64]string{}
	}
	if held, ok := m.tokens[cardID]; ok {
		return held, nil
	}

	m.tokens[cardID] = offered

	return offered, nil
}

func (m *memCards) Update(_ context.Context, c *domain.Card) error {
	m.byToken[c.Token] = c
	m.byID[c.ID] = c
	return nil
}

type memReceipts struct {
	byID   map[string]*domain.Receipt
	nextID int64
}

func newMemReceipts() *memReceipts {
	return &memReceipts{byID: map[string]*domain.Receipt{}, nextID: 1}
}

func (m *memReceipts) ByReceiptID(_ context.Context, id string) (*domain.Receipt, error) {
	r, ok := m.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return r, nil
}

func (m *memReceipts) Create(_ context.Context, r *domain.Receipt) error {
	r.ID = m.nextID
	m.nextID++
	m.byID[r.ReceiptID] = r
	return nil
}

func (m *memReceipts) Update(_ context.Context, r *domain.Receipt) error {
	m.byID[r.ReceiptID] = r
	return nil
}

func (m *memReceipts) List(context.Context, int64, int64, int, int) ([]*domain.Receipt, error) {
	return nil, nil
}

// The documented error envelope names the request it answers and arrives as
// text/json. A refusal raised before dispatch is still an answer to a request,
// so it carries the same id a successful one would.
func TestRefusalEchoesTheRequestIDAndContentType(t *testing.T) {
	h := newHandler(t, staticCreds)

	// cards.remove is server-side, so a browser credential is refused before
	// the call is ever dispatched.
	rec := post(t, h, `{"id":2032,"method":"cards.remove","params":{"token":"x"}}`, browserAuth())

	assert.Contains(t, rec.Header().Get("Content-Type"), "text/json")
	assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec))

	body := decode(t, rec)
	assert.Equal(t, float64(2032), body["id"], "the refusal must name the request it answers")
}

// A body that is not JSON has no id to echo. The Subscribe API still answers
// with the field present and null, which is what the documented examples show
// and what a client decoding into a fixed shape expects.
func TestRefusalWithoutAReadableIDAnswersNull(t *testing.T) {
	h := newHandler(t, staticCreds)

	rec := post(t, h, `not json at all`, browserAuth())

	body := decode(t, rec)
	value, present := body["id"]
	assert.True(t, present)
	assert.Nil(t, value)
}

// Every Subscribe API response names the protocol version; the Merchant API's
// documented replies do not, and the two envelopes are kept apart.
func TestEveryResponseNamesTheProtocolVersion(t *testing.T) {
	h := newHandler(t, staticCreds)

	for _, body := range []string{
		`not json at all`,
		`{"id":1,"method":"nosuch.method","params":{}}`,
		`{"id":1,"method":"cards.create","params":{"card":{"number":"` + cardNumber + `","expire":"0399"}}}`,
	} {
		rec := post(t, h, body, browserAuth())
		assert.Equal(t, "2.0", decode(t, rec)["jsonrpc"], body)
	}
}
