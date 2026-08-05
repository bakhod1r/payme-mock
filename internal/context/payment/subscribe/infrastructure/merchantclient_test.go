package infrastructure_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
)

// capture is what one recorded Merchant API call looked like on the wire.
type capture struct {
	auth   string
	method string
	params map[string]any
	id     json.RawMessage
}

// merchantStub answers like a merchant, recording what it was asked.
func merchantStub(t *testing.T, reply func(method string) (status int, body string)) (*infrastructure.MerchantClient, *capture) {
	t.Helper()

	seen := &capture{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var envelope struct {
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			ID     json.RawMessage `json:"id"`
		}
		require.NoError(t, json.Unmarshal(body, &envelope))

		seen.auth = r.Header.Get("Authorization")
		seen.method = envelope.Method
		seen.params = envelope.Params
		seen.id = envelope.ID

		status, reply := reply(envelope.Method)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)

	var counter atomic.Int64
	client := infrastructure.NewMerchantClient(server.URL, "test-key", server.Client(),
		func() int64 { return counter.Add(1) })

	return client, seen
}

func TestMerchantClientSendsTheDocumentedChain(t *testing.T) {
	client, seen := merchantStub(t, func(method string) (int, string) {
		if method == "CreateTransaction" {
			return http.StatusOK, `{"result":{"create_time":1399114284039,"transaction":"1","state":1},"id":1}`
		}
		return http.StatusOK, `{"result":{"allow":true},"id":1}`
	})

	ctx := context.Background()
	account := map[string]string{"order_id": "197"}

	require.NoError(t, client.CheckPerformTransaction(ctx, amount, account))
	assert.Equal(t, "CheckPerformTransaction", seen.method)
	assert.Equal(t, httpx.MerchantAuthHeader("test-key"), seen.auth,
		"the key travels as Basic base64(\"Paycom:KEY\"), as the provider sends it")
	assert.InDelta(t, float64(amount), seen.params["amount"], 0)
	assert.Equal(t, map[string]any{"order_id": "197"}, seen.params["account"])

	transaction, err := client.CreateTransaction(ctx, receiptID, nowMs, amount, account)
	require.NoError(t, err)
	assert.Equal(t, "1", transaction, "the merchant's own identifier is returned")
	assert.Equal(t, receiptID, seen.params["id"])
	assert.InDelta(t, float64(nowMs), seen.params["time"], 0)

	require.NoError(t, client.PerformTransaction(ctx, receiptID))
	assert.Equal(t, "PerformTransaction", seen.method)

	require.NoError(t, client.CancelTransaction(ctx, receiptID, 3))
	assert.Equal(t, "CancelTransaction", seen.method)
	assert.InDelta(t, float64(3), seen.params["reason"], 0)

	assert.Equal(t, json.RawMessage("4"), seen.id, "each call carries its own identifier")
}

// A protocol error the merchant reports must reach the caller with its own
// code, so the Payme side reacts to the merchant's answer rather than to a
// generic failure.
func TestMerchantClientPassesTheMerchantsErrorThrough(t *testing.T) {
	client, _ := merchantStub(t, func(string) (int, string) {
		return http.StatusOK, `{"error":{"code":-31008,"message":{"ru":"р","uz":"u","en":"e"},"data":"order_id"},"id":1}`
	})

	err := client.CheckPerformTransaction(context.Background(), amount, nil)

	var pe *payerr.ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, payerr.CodeCannotPerform, pe.Code)
	assert.Equal(t, "order_id", pe.Data)
	assert.Equal(t, "e", pe.Message.EN)
}

func TestMerchantClientErrorOnEveryMethod(t *testing.T) {
	client, _ := merchantStub(t, func(string) (int, string) {
		return http.StatusOK, `{"error":{"code":-31008,"message":{"ru":"р","uz":"u","en":"e"}},"id":1}`
	})

	ctx := context.Background()

	_, err := client.CreateTransaction(ctx, receiptID, nowMs, amount, nil)
	assert.ErrorIs(t, err, payerr.ErrCannotPerform)
	assert.ErrorIs(t, client.PerformTransaction(ctx, receiptID), payerr.ErrCannotPerform)
	assert.ErrorIs(t, client.CancelTransaction(ctx, receiptID, 3), payerr.ErrCannotPerform)
}

// The Merchant API always answers 200 with a JSON-RPC body. Anything else
// means the request never reached the handler, which is a transport failure.
func TestMerchantClientTreatsAMalformedAnswerAsTransport(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"an HTTP status other than 200", http.StatusInternalServerError, `{"result":{}}`},
		{"a body that is not JSON", http.StatusOK, `not json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := merchantStub(t, func(string) (int, string) { return tt.status, tt.body })

			err := client.CheckPerformTransaction(context.Background(), amount, nil)

			assert.ErrorIs(t, err, payerr.ErrTransport)
		})
	}
}

// A merchant that answers 200 with a result missing the documented fields is a
// broken integration, which is exactly what this stand exists to surface.
func TestMerchantClientRejectsAResultItCannotRead(t *testing.T) {
	client, _ := merchantStub(t, func(string) (int, string) {
		return http.StatusOK, `{"result":"not an object","id":1}`
	})

	_, err := client.CreateTransaction(context.Background(), receiptID, nowMs, amount, nil)

	assert.ErrorIs(t, err, payerr.ErrTransport)
}

// A merchant that starts answering and then drops the connection leaves a
// body that cannot be read to the end, which is a transport failure too.
func TestMerchantClientReportsAnAnswerCutShort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "512")
		_, _ = io.WriteString(w, `{"result":`)
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(server.Close)

	client := infrastructure.NewMerchantClient(server.URL, "k", server.Client(),
		func() int64 { return 1 })

	err := client.PerformTransaction(context.Background(), receiptID)

	assert.ErrorIs(t, err, payerr.ErrTransport)
}

func TestMerchantClientReportsAnUnreachableMerchant(t *testing.T) {
	client := infrastructure.NewMerchantClient("http://127.0.0.1:1", "k",
		&http.Client{}, func() int64 { return 1 })

	err := client.PerformTransaction(context.Background(), receiptID)

	assert.ErrorIs(t, err, payerr.ErrTransport)
}

// An endpoint that is not a URL cannot be turned into a request at all, which
// is a wiring fault rather than something the merchant did.
func TestMerchantClientReportsAnUnusableEndpoint(t *testing.T) {
	client := infrastructure.NewMerchantClient("http://\x7f", "k",
		&http.Client{}, func() int64 { return 1 })

	err := client.PerformTransaction(context.Background(), receiptID)

	assert.ErrorContains(t, err, "build PerformTransaction request")
}
