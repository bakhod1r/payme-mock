package jsonrpc_test

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/jsonrpc"
)

func TestNewResultEchoesID(t *testing.T) {
	got := jsonrpc.NewResult(json.RawMessage(`2032`), map[string]bool{"allow": true})

	assert.Equal(t, json.RawMessage(`2032`), got.ID)
	assert.Nil(t, got.Error)
}

// The documentation's own error example, byte for byte.
func TestErrorResponseMatchesDocumentedShape(t *testing.T) {
	err := payerr.New(payerr.Code(-31050), payerr.NewMessage(
		"Номер телефона не найден",
		"Raqam ro'yhatda yo'q",
		"Phone number not found",
	), "phone")

	resp := jsonrpc.NewError(json.RawMessage(`2032`), err)

	got, marshalErr := json.Marshal(resp)
	require.NoError(t, marshalErr)

	const want = `{"error":{"code":-31050,` +
		`"message":{"ru":"Номер телефона не найден","uz":"Raqam ro'yhatda yo'q","en":"Phone number not found"},` +
		`"data":"phone"},"id":2032}`

	assert.JSONEq(t, want, string(got))
}

func TestSuccessResponseOmitsError(t *testing.T) {
	resp := jsonrpc.NewResult(json.RawMessage(`1`), map[string]any{"allow": true})

	got, err := json.Marshal(resp)
	require.NoError(t, err)

	assert.JSONEq(t, `{"result":{"allow":true},"id":1}`, string(got))
	assert.NotContains(t, string(got), "error")
}

func TestErrorResponseOmitsEmptyData(t *testing.T) {
	resp := jsonrpc.NewError(json.RawMessage(`7`), payerr.ErrCannotPerform)

	got, err := json.Marshal(resp)
	require.NoError(t, err)

	assert.NotContains(t, string(got), `"data"`)
}

func TestErrorFrom(t *testing.T) {
	id := json.RawMessage(`5`)

	t.Run("keeps protocol code", func(t *testing.T) {
		resp := jsonrpc.ErrorFrom(id, payerr.ErrTransactionNotFound)

		require.NotNil(t, resp.Error)
		assert.Equal(t, payerr.CodeTransactionNotFnd, resp.Error.Code)
	})

	t.Run("hides internal failures behind a system error", func(t *testing.T) {
		resp := jsonrpc.ErrorFrom(id, stderrors.New("pq: connection refused"))

		require.NotNil(t, resp.Error)
		assert.Equal(t, payerr.CodeTransport, resp.Error.Code)
		assert.NotContains(t, resp.Error.Message.RU, "connection refused")
	})
}

func newTestRouter(t *testing.T) *jsonrpc.Router {
	t.Helper()

	r := jsonrpc.NewRouter()
	r.Register("CheckPerformTransaction", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			Amount int64 `json:"amount"`
		}
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		if p.Amount <= 0 {
			return nil, payerr.ErrInvalidAmount
		}
		return map[string]bool{"allow": true}, nil
	})
	r.Register("Boom", func(context.Context, json.RawMessage) (any, error) {
		return nil, stderrors.New("internal explosion")
	})
	return r
}

func TestRouterDispatch(t *testing.T) {
	r := newTestRouter(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		body     string
		wantCode payerr.Code
		wantOK   bool
	}{
		{
			name:   "valid call succeeds",
			body:   `{"method":"CheckPerformTransaction","params":{"amount":500000},"id":1}`,
			wantOK: true,
		},
		{
			name:     "malformed JSON is a parse error",
			body:     `{"method":`,
			wantCode: payerr.CodeParse,
		},
		{
			name:     "missing method is an invalid request",
			body:     `{"params":{},"id":1}`,
			wantCode: payerr.CodeInvalidRequest,
		},
		{
			name:     "unknown method is not found",
			body:     `{"method":"NoSuchMethod","params":{},"id":1}`,
			wantCode: payerr.CodeMethodNotFound,
		},
		{
			name:     "handler protocol error passes through",
			body:     `{"method":"CheckPerformTransaction","params":{"amount":0},"id":1}`,
			wantCode: payerr.CodeInvalidAmount,
		},
		{
			name:     "handler internal error becomes system error",
			body:     `{"method":"Boom","params":{},"id":1}`,
			wantCode: payerr.CodeTransport,
		},
		{
			name:     "missing params is an invalid request",
			body:     `{"method":"CheckPerformTransaction","id":1}`,
			wantCode: payerr.CodeInvalidRequest,
		},
		{
			name:     "params of wrong type is an invalid request",
			body:     `{"method":"CheckPerformTransaction","params":{"amount":"lots"},"id":1}`,
			wantCode: payerr.CodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := r.Dispatch(ctx, []byte(tt.body))

			if tt.wantOK {
				require.Nil(t, resp.Error)
				assert.NotNil(t, resp.Result)
				return
			}
			require.NotNil(t, resp.Error)
			assert.Equal(t, tt.wantCode, resp.Error.Code)
		})
	}
}

func TestDispatchEchoesIDEvenOnError(t *testing.T) {
	r := newTestRouter(t)

	resp := r.Dispatch(context.Background(), []byte(`{"method":"Nope","params":{},"id":2032}`))

	assert.Equal(t, json.RawMessage(`2032`), resp.ID)
}

func TestDispatchOnParseErrorHasNoID(t *testing.T) {
	r := newTestRouter(t)

	resp := r.Dispatch(context.Background(), []byte(`not json`))

	assert.Nil(t, resp.ID, "the id cannot be known when the body does not parse")
}

func TestRegisterReplacesPreviousHandler(t *testing.T) {
	r := jsonrpc.NewRouter()
	r.Register("M", func(context.Context, json.RawMessage) (any, error) {
		return "first", nil
	})
	r.Register("M", func(context.Context, json.RawMessage) (any, error) {
		return "second", nil
	})

	resp := r.Dispatch(context.Background(), []byte(`{"method":"M","params":{},"id":1}`))

	assert.Equal(t, "second", resp.Result)
}

func TestRouterMethods(t *testing.T) {
	r := newTestRouter(t)

	assert.ElementsMatch(t, []string{"CheckPerformTransaction", "Boom"}, r.Methods())
}

func TestDecodeParams(t *testing.T) {
	type target struct {
		ID string `json:"id"`
	}

	t.Run("decodes named params", func(t *testing.T) {
		var got target

		err := jsonrpc.DecodeParams(json.RawMessage(`{"id":"5305e3bab097f420a62ced0b"}`), &got)

		require.NoError(t, err)
		assert.Equal(t, "5305e3bab097f420a62ced0b", got.ID)
	})

	t.Run("empty params is an invalid request", func(t *testing.T) {
		var got target

		err := jsonrpc.DecodeParams(nil, &got)

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})

	t.Run("malformed params is an invalid request", func(t *testing.T) {
		var got target

		err := jsonrpc.DecodeParams(json.RawMessage(`[1,2,3]`), &got)

		assert.ErrorIs(t, err, payerr.ErrInvalidRequest)
	})
}

// IDOf is what lets a refusal raised before dispatch answer the request that
// caused it, so it must read the id whatever shape it arrives in.
func TestIDOf(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"a number", `{"id":2032,"method":"cards.check"}`, "2032"},
		{"a string", `{"id":"abc","method":"cards.check"}`, `"abc"`},
		{"no id", `{"method":"cards.check"}`, ""},
		{"not json", `nonsense`, ""},
		{"null", `{"id":null}`, "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, string(jsonrpc.IDOf([]byte(tt.body))))
		})
	}
}
