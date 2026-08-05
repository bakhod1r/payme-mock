package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The command has to be the call: same address, same credential, same body,
// runnable as it stands. What it must not carry is a header the client sets for
// itself — a stale Content-Length makes a replayed request hang, which is a
// worse failure than not offering the command at all.
func TestCurlOfRendersTheCallAgain(t *testing.T) {
	entry := trafficDetail{
		trafficRow: trafficRow{
			Sandbox: "dividend",
			Service: "paymemock",
			Method:  "transactions.create",
		},
		ID:          805,
		RequestBody: `{"method":"transactions.create","params":{"amount":100000}}`,
		RequestHeaders: []accountField{
			{Name: "X-Auth", Value: "merchant:key"},
			{Name: "Content-Length", Value: "625"},
			{Name: "Accept-Encoding", Value: "gzip"},
			{Name: "User-Agent", Value: "Go-http-client/1.1"},
		},
	}

	got := curlOf("http://localhost", entry)

	assert.Contains(t, got, "curl -i -X POST 'http://localhost:8082/api'")
	assert.Contains(t, got, `-H 'X-Auth: merchant:key'`)
	assert.Contains(t, got, `-d '{"method":"transactions.create","params":{"amount":100000}}'`)

	for _, dropped := range []string{"Content-Length", "Accept-Encoding", "User-Agent"} {
		assert.NotContains(t, got, dropped,
			"a header the client sets for itself must not be replayed")
	}

	assert.Contains(t, got, "Content-Type: application/json",
		"a call logged without a content type still has to be sent with one")
}

// The merchant API is addressed by the stand's slug, so its command has to go
// to that stand rather than to the shared address.
func TestCurlOfAddressesTheMerchantAPIBySlug(t *testing.T) {
	entry := trafficDetail{
		trafficRow: trafficRow{Sandbox: "topup", Service: "merchant", Method: "CreateTransaction"},
		ID:         12,
	}

	assert.Contains(t, curlOf("http://localhost/", entry),
		"'http://localhost:8081/s/topup/payme/merchant'")
}

// A body is JSON and JSON is full of quotes, so the quoting is what decides
// whether the command reproduces the request or a mangled version of it.
func TestCurlOfSurvivesAQuoteInTheBody(t *testing.T) {
	entry := trafficDetail{
		trafficRow:  trafficRow{Sandbox: "s", Service: "paymemock", Method: "receipts.create"},
		RequestBody: `{"note":"it's here"}`,
	}

	got := curlOf("http://localhost", entry)

	assert.Contains(t, got, `'{"note":"it'\''s here"}'`)
	assert.Equal(t, 1, strings.Count(got, "-d "), "one body, one flag")
}
