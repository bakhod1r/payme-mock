package infrastructure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
	"github.com/bakhod1r/payme-mock/internal/kernel/jsonrpc"
)

// maxResponseBytes caps what a merchant may answer with, so a runaway billing
// side cannot exhaust the emulator's memory.
const maxResponseBytes = 1 << 20 // 1 MiB

// MerchantClient calls a merchant's Merchant API over JSON-RPC.
//
// It is the same client whether the endpoint is a stand in this process or a
// real cash register on the internet: the Payme side does not know which, and
// nothing here treats the mock specially.
type MerchantClient struct {
	endpoint string
	key      string
	http     *http.Client
	// nextID numbers the calls. The protocol only requires that a reply can be
	// matched to its request.
	nextID func() int64
}

// NewMerchantClient wires a client to one merchant's endpoint and key.
func NewMerchantClient(endpoint, key string, client *http.Client, nextID func() int64) *MerchantClient {
	return &MerchantClient{endpoint: endpoint, key: key, http: client, nextID: nextID}
}

// CheckPerformTransaction asks whether the payment may proceed.
func (c *MerchantClient) CheckPerformTransaction(ctx context.Context, amount int64, account map[string]string) error {
	_, err := c.call(ctx, "CheckPerformTransaction", map[string]any{
		"amount":  amount,
		"account": account,
	})
	return err
}

// CreateTransaction registers the transaction in the merchant's billing and
// returns the identifier the merchant assigned it.
func (c *MerchantClient) CreateTransaction(ctx context.Context, id string, timeMillis, amount int64,
	account map[string]string,
) (string, error) {
	raw, err := c.call(ctx, "CreateTransaction", map[string]any{
		"id":      id,
		"time":    timeMillis,
		"amount":  amount,
		"account": account,
	})
	if err != nil {
		return "", err
	}

	var result struct {
		Transaction string `json:"transaction"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		// A merchant that answers with something other than the documented
		// result is a broken integration, which is exactly what this stand
		// exists to surface.
		return "", payerr.ErrTransport
	}

	return result.Transaction, nil
}

// PerformTransaction closes the transaction as paid.
func (c *MerchantClient) PerformTransaction(ctx context.Context, id string) error {
	_, err := c.call(ctx, "PerformTransaction", map[string]any{"id": id})
	return err
}

// CancelTransaction reverses the transaction with a reason.
func (c *MerchantClient) CancelTransaction(ctx context.Context, id string, reason int) error {
	_, err := c.call(ctx, "CancelTransaction", map[string]any{"id": id, "reason": reason})
	return err
}

// call performs one JSON-RPC request and returns the raw result member.
//
// A protocol error the merchant reports is returned as-is, so the Payme side
// reacts to the merchant's own code rather than to a generic failure.
func (c *MerchantClient) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	// Every parameter is a string, a number or a string map, so the envelope
	// cannot fail to encode.
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID(),
		"method":  method,
		"params":  params,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", httpx.MerchantAuthHeader(c.key))

	resp, err := c.http.Do(req)
	if err != nil {
		// An unreachable or timed-out merchant is a transport failure, which
		// is what the provider reports too.
		return nil, payerr.ErrTransport
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	// The Merchant API always answers 200; anything else means the request
	// never reached the handler.
	if resp.StatusCode != http.StatusOK {
		return nil, payerr.ErrTransport
	}

	var envelope struct {
		Result json.RawMessage      `json:"result"`
		Error  *jsonrpc.ErrorObject `json:"error"`
	}
	// Decoding straight off the body treats a stream cut short and a body that
	// is not JSON-RPC as the same thing, which they are to the caller: the
	// merchant did not answer.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&envelope); err != nil {
		return nil, payerr.ErrTransport
	}

	if envelope.Error != nil {
		return nil, payerr.New(envelope.Error.Code, envelope.Error.Message, envelope.Error.Data)
	}

	return envelope.Result, nil
}
