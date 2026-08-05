package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/application"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	merchanthttp "github.com/bakhod1r/payme-mock/internal/context/payment/merchant/interfaces/http"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
)

const (
	testKey     = "5NPh3f4rTLPa0Vk1LOZ8AT8gK4EbAqTaPHnk"
	paymeID     = "5305e3bab097f420a62ced0b"
	orderAmount = 500000
	payerPhone  = "901234567"
)

func newHandler(t *testing.T, resolve merchanthttp.KeyResolver) http.Handler {
	t.Helper()

	txs := newMemTransactions()
	accts := map[string]*billing.Account{
		"phone=" + payerPhone: {ID: 42, SandboxID: 1, Phone: payerPhone},
	}
	order := &billing.Order{ID: 197, SandboxID: 1, AccountID: 42, Amount: orderAmount, Status: billing.StatusNew}
	orders := &memOrders{byID: map[int64]*billing.Order{197: order}, byAccount: map[int64][]*billing.Order{42: {order}}}

	svc := application.NewService(
		txs, noopEvents{}, memAccounts(accts), orders, noWalkIns{},
		clock.NewFake(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)),
		application.Settings{TransactionTimeoutMillis: 43_200_000, AccountField: "phone"},
	)

	return merchanthttp.NewHandler(svc, resolve)
}

// noWalkIns is the walk-in port of a stand that does not accept unknown
// payers, which is what these tests exercise.
type noWalkIns struct{}

func (noWalkIns) Register(context.Context, string, int64) (*billing.Order, error) {
	return nil, domain.ErrNotFound
}

func staticKey(ctx context.Context) (string, error) { return testKey, nil } //nolint:revive // signature is the port

func post(t *testing.T, h http.Handler, body, auth string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/payme/merchant", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
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

	body := decode(t, rec)
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "expected an error object, got %s", rec.Body.String())
	return payerr.Code(errObj["code"].(float64))
}

// Every response, success or failure, is HTTP 200. Any other status is a
// transport error to Payme.
func TestAlwaysAnswersHTTP200(t *testing.T) {
	h := newHandler(t, staticKey)
	auth := httpx.MerchantAuthHeader(testKey)

	cases := []struct {
		name string
		body string
		auth string
	}{
		{"valid call", `{"method":"CheckPerformTransaction","params":{"amount":500000,"account":{"phone":"901234567"}},"id":1}`, auth},
		{"unauthorized", `{"method":"CheckPerformTransaction","params":{},"id":1}`, "Basic bad"},
		{"missing authorization", `{"method":"CheckPerformTransaction","params":{},"id":1}`, ""},
		{"malformed body", `{`, auth},
		{"unknown method", `{"method":"Nope","params":{},"id":1}`, auth},
		{"business error", `{"method":"CheckTransaction","params":{"id":"missing"},"id":1}`, auth},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, h, tt.body, tt.auth)

			assert.Equal(t, http.StatusOK, rec.Code)
			// The provider answers with text/json, which is what an
			// integration written against it expects to see.
			assert.Contains(t, rec.Header().Get("Content-Type"), "text/json")
		})
	}
}

func TestAuthorization(t *testing.T) {
	h := newHandler(t, staticKey)
	const body = `{"method":"CheckPerformTransaction","params":{"amount":500000,"account":{"phone":"901234567"}},"id":1}`

	t.Run("accepts the documented Basic credentials", func(t *testing.T) {
		rec := post(t, h, body, httpx.MerchantAuthHeader(testKey))

		got := decode(t, rec)
		assert.NotContains(t, got, "error")
	})

	t.Run("rejects a wrong key", func(t *testing.T) {
		rec := post(t, h, body, httpx.MerchantAuthHeader("wrong-key"))

		assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec))
	})

	t.Run("rejects a missing header", func(t *testing.T) {
		rec := post(t, h, body, "")

		assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec))
	})

	t.Run("rejects when the key cannot be resolved", func(t *testing.T) {
		failing := newHandler(t, func(context.Context) (string, error) {
			return "", errors.New("no such sandbox")
		})

		rec := post(t, failing, body, httpx.MerchantAuthHeader(testKey))

		assert.Equal(t, payerr.CodeUnauthorized, errorCode(t, rec))
	})
}

func TestRejectsNonPostMethods(t *testing.T) {
	h := newHandler(t, staticKey)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/payme/merchant", nil)
			req.Header.Set("Authorization", httpx.MerchantAuthHeader(testKey))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, payerr.CodeInvalidHTTP, errorCode(t, rec))
		})
	}
}

func TestBodyReadFailureBecomesTransportError(t *testing.T) {
	h := newHandler(t, staticKey)

	req := httptest.NewRequest(http.MethodPost, "/payme/merchant", failingReader{})
	req.Header.Set("Authorization", httpx.MerchantAuthHeader(testKey))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, payerr.CodeTransport, errorCode(t, rec))
}

// The full sandbox scenario over HTTP: create, perform, check, cancel.
func TestFullTransactionLifecycleOverHTTP(t *testing.T) {
	h := newHandler(t, staticKey)
	auth := httpx.MerchantAuthHeader(testKey)

	rec := post(t, h, `{"method":"CheckPerformTransaction","params":{"amount":500000,"account":{"phone":"901234567"}},"id":1}`, auth)
	result := decode(t, rec)["result"].(map[string]any)
	assert.Equal(t, true, result["allow"])

	rec = post(t, h, `{"method":"CreateTransaction","params":{"id":"`+paymeID+`","time":1399114284039,"amount":500000,"account":{"phone":"901234567"}},"id":2}`, auth)
	result = decode(t, rec)["result"].(map[string]any)
	assert.Equal(t, float64(domain.StateCreated), result["state"])
	transaction := result["transaction"].(string)

	rec = post(t, h, `{"method":"PerformTransaction","params":{"id":"`+paymeID+`"},"id":3}`, auth)
	result = decode(t, rec)["result"].(map[string]any)
	assert.Equal(t, float64(domain.StatePerformed), result["state"])
	assert.Equal(t, transaction, result["transaction"])

	rec = post(t, h, `{"method":"CheckTransaction","params":{"id":"`+paymeID+`"},"id":4}`, auth)
	result = decode(t, rec)["result"].(map[string]any)
	assert.Equal(t, float64(domain.StatePerformed), result["state"])
	assert.Nil(t, result["reason"], "an uncancelled transaction reports a null reason")

	rec = post(t, h, `{"method":"CancelTransaction","params":{"id":"`+paymeID+`","reason":5},"id":5}`, auth)
	result = decode(t, rec)["result"].(map[string]any)
	assert.Equal(t, float64(domain.StateCancelledAfterDo), result["state"])
}

// The sandbox sends every write twice and compares the two responses.
func TestRepeatedWritesReturnByteIdenticalResponses(t *testing.T) {
	h := newHandler(t, staticKey)
	auth := httpx.MerchantAuthHeader(testKey)
	create := `{"method":"CreateTransaction","params":{"id":"` + paymeID + `","time":1399114284039,"amount":500000,"account":{"phone":"901234567"}},"id":2}`

	first := post(t, h, create, auth).Body.String()
	second := post(t, h, create, auth).Body.String()

	assert.Equal(t, first, second, "a repeated CreateTransaction must return the same bytes")

	perform := `{"method":"PerformTransaction","params":{"id":"` + paymeID + `"},"id":3}`
	assert.Equal(t, post(t, h, perform, auth).Body.String(), post(t, h, perform, auth).Body.String())

	cancel := `{"method":"CancelTransaction","params":{"id":"` + paymeID + `","reason":5},"id":4}`
	assert.Equal(t, post(t, h, cancel, auth).Body.String(), post(t, h, cancel, auth).Body.String())
}

func TestProtocolErrorsSurfaceWithTheirCodes(t *testing.T) {
	h := newHandler(t, staticKey)
	auth := httpx.MerchantAuthHeader(testKey)

	tests := []struct {
		name string
		body string
		want payerr.Code
	}{
		{"unknown transaction", `{"method":"CheckTransaction","params":{"id":"nope"},"id":1}`, payerr.CodeTransactionNotFnd},
		{"wrong amount", `{"method":"CheckPerformTransaction","params":{"amount":1,"account":{"phone":"901234567"}},"id":1}`, payerr.CodeInvalidAmount},
		{"unknown method", `{"method":"Nope","params":{},"id":1}`, payerr.CodeMethodNotFound},
		{"malformed JSON", `{"method":`, payerr.CodeParse},
		{"missing params", `{"method":"CheckTransaction","id":1}`, payerr.CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(t, h, tt.body, auth)

			assert.Equal(t, tt.want, errorCode(t, rec))
		})
	}
}

// Account errors must name the offending field and carry all three languages.
func TestAccountErrorShape(t *testing.T) {
	h := newHandler(t, staticKey)

	rec := post(t, h,
		`{"method":"CheckPerformTransaction","params":{"amount":500000,"account":{"phone":"000"}},"id":1}`,
		httpx.MerchantAuthHeader(testKey))

	errObj := decode(t, rec)["error"].(map[string]any)
	assert.True(t, payerr.Code(errObj["code"].(float64)).IsAccountCode())
	assert.Equal(t, "phone", errObj["data"])

	msg := errObj["message"].(map[string]any)
	assert.NotEmpty(t, msg["ru"])
	assert.NotEmpty(t, msg["uz"])
	assert.NotEmpty(t, msg["en"])
}

func TestResponseEchoesRequestID(t *testing.T) {
	h := newHandler(t, staticKey)

	rec := post(t, h, `{"method":"CheckTransaction","params":{"id":"nope"},"id":2032}`, httpx.MerchantAuthHeader(testKey))

	assert.Equal(t, float64(2032), decode(t, rec)["id"])
}

// ---------- test doubles ----------

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingReader) Close() error             { return nil }

type noopEvents struct{}

func (noopEvents) Record(context.Context, domain.Event) error { return nil }

type memAccounts map[string]*billing.Account

func (m memAccounts) ByField(_ context.Context, field, value string) (*billing.Account, error) {
	acc, ok := m[field+"="+value]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return acc, nil
}

// ByID scans the same payers, which is all a handler test needs: the balance
// move matters here only in that it must not fail the request.
func (m memAccounts) ByID(_ context.Context, id int64) (*billing.Account, error) {
	for _, acc := range m {
		if acc.ID == id {
			return acc, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (m memAccounts) Register(_ context.Context) (*billing.Account, error) {
	var out *billing.Account
	for _, acc := range m {
		if out == nil || acc.ID < out.ID {
			out = acc
		}
	}
	if out == nil {
		return nil, domain.ErrNotFound
	}
	return out, nil
}

func (m memAccounts) UpdateBalance(_ context.Context, id, balance int64) error {
	for _, acc := range m {
		if acc.ID == id {
			acc.Balance = balance
			return nil
		}
	}
	return domain.ErrNotFound
}

type memOrders struct {
	byID      map[int64]*billing.Order
	byAccount map[int64][]*billing.Order
}

func (m *memOrders) ByID(_ context.Context, id int64) (*billing.Order, error) {
	o, ok := m.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return o, nil
}

func (m *memOrders) ByAccount(_ context.Context, accountID int64) ([]*billing.Order, error) {
	return m.byAccount[accountID], nil
}

func (m *memOrders) Update(_ context.Context, o *billing.Order) error {
	m.byID[o.ID] = o
	return nil
}

type memTransactions struct {
	byPaymeID map[string]*domain.Transaction
	active    map[int64]*domain.Transaction
	nextID    int64
}

func newMemTransactions() *memTransactions {
	return &memTransactions{
		byPaymeID: make(map[string]*domain.Transaction),
		active:    make(map[int64]*domain.Transaction),
		nextID:    5123,
	}
}

func (m *memTransactions) ByPaymeID(_ context.Context, id string) (*domain.Transaction, error) {
	tx, ok := m.byPaymeID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return tx, nil
}

func (m *memTransactions) Create(_ context.Context, tx *domain.Transaction) error {
	tx.ID = m.nextID
	m.nextID++
	m.byPaymeID[tx.PaymeID] = tx
	m.active[tx.OrderID] = tx
	return nil
}

func (m *memTransactions) Update(_ context.Context, tx *domain.Transaction) error {
	m.byPaymeID[tx.PaymeID] = tx
	if tx.State.IsActive() {
		m.active[tx.OrderID] = tx
	} else {
		delete(m.active, tx.OrderID)
	}
	return nil
}

func (m *memTransactions) Statement(_ context.Context, from, to int64) ([]*domain.Transaction, error) {
	var out []*domain.Transaction
	for _, tx := range m.byPaymeID {
		if tx.CreateTime >= from && tx.CreateTime <= to {
			out = append(out, tx)
		}
	}
	return out, nil
}

func (m *memTransactions) ActiveByOrder(_ context.Context, orderID int64) (*domain.Transaction, error) {
	tx, ok := m.active[orderID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return tx, nil
}

var _ io.ReadCloser = failingReader{}
