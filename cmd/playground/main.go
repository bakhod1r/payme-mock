// Command playground answers the Subscribe API inside a browser tab.
//
// It is the same service the real stand runs — the same domain, the same
// application layer, the same JSON-RPC handler — wired to memory instead of to
// Postgres, and compiled to WebAssembly. Nothing about the protocol is
// reimplemented for the web: if a rule holds here, it holds on the stand,
// because it is one body of code.
//
// What it is not is the whole stand. There is no merchant behind it, so the
// Merchant API chain is accepted rather than driven; there is no worker, so a
// receipt settles at once; and there is no database, so a reload starts over.
// Those are the three things a reader should know, and the page says all three.
//
// Build:
//
//	GOOS=js GOARCH=wasm go build -o docs/playground/payme-mock.wasm ./cmd/playground
//
// The build tag keeps syscall/js out of every other platform's build: without
// it `go build ./...` fails on a package that was never meant for them.

//go:build js && wasm

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"syscall/js"

	subscribeapp "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/application"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure/inmem"
	subscribehttp "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/interfaces/http"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
)

// merchantID is the register the page speaks to. There is one, so it is fixed
// rather than configurable: a playground that made the reader invent an id
// before it would answer would be asking for the least interesting decision
// first.
const merchantID = "000000000000000000000001"

// testKey is the server-side credential. It is published on the page on
// purpose — everything here is in the reader's own tab, and a key held back
// would only stop them trying the methods that need it.
const testKey = "playground-test-key"

// stand is everything the page holds. It is rebuilt by reset(), so a reader who
// has made a mess can start over without reloading.
type stand struct {
	handler  http.Handler
	cards    *inmem.Cards
	receipts *inmem.Receipts
	sms      *inmem.SMS
	merchant *inmem.Merchant
	ledger   *inmem.Ledger
}

func newStand() *stand {
	cards := inmem.NewCards()
	receipts := inmem.NewReceipts()
	sms := inmem.NewSMS()
	merchant := inmem.NewMerchant()
	ledger := inmem.NewLedger(100_000_000_000, "topup")

	svc := subscribeapp.NewService(
		cards, receipts, merchant, inmem.NewScheduler(), inmem.NewTokens(), sms,
		clock.System{},
		subscribeapp.Settings{
			SandboxID:        1,
			MerchantID:       merchantID,
			MerchantName:     "Playground",
			VerifyCode:       "666666",
			VerifyWaitMillis: 60_000,
			// The state walk is spaced out on the stand because real payments
			// take time. Nothing here runs the walk, so a delay would only
			// leave receipts stuck.
			StepDelayMillis:  0,
			HoldWindowMillis: 43_200_000,
			CardBalance:      1_000_000_000,
		},
		subscribeapp.WithLedger(ledger),
	)

	// The handler decides for itself whether a call may proceed, so the
	// credential is resolved the way the real service resolves it rather than
	// waved through: the browser-only methods have to stay browser-only, or the
	// page would teach the wrong thing about the protocol.
	resolve := func(context.Context) (subscribehttp.Credentials, error) {
		return subscribehttp.Credentials{MerchantID: merchantID, Key: testKey}, nil
	}

	return &stand{
		handler:  subscribehttp.NewHandler(svc, resolve),
		cards:    cards,
		receipts: receipts,
		sms:      sms,
		merchant: merchant,
		ledger:   ledger,
	}
}

// call runs one JSON-RPC request through the handler and returns the answer.
//
// The handler is an http.Handler, and an http.Handler needs neither a listener
// nor a network: a recorder and a request built in memory are the whole
// transport. That is the reason this works in a browser at all.
func (s *stand) call(body, auth string) string {
	req := httptest.NewRequest(http.MethodPost, "/api", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth", auth)

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	return rec.Body.String()
}

// state is what the page shows beside the answer: what the stand now holds.
func (s *stand) state() string {
	type cardView struct {
		Token    string `json:"token"`
		Number   string `json:"number"`
		Outcome  string `json:"outcome"`
		Verified bool   `json:"verified"`
	}
	type receiptView struct {
		ID     string `json:"id"`
		Amount int64  `json:"amount"`
		State  int    `json:"state"`
		Payout bool   `json:"payout"`
	}

	out := struct {
		Cards    []cardView    `json:"cards"`
		Receipts []receiptView `json:"receipts"`
		SMS      []string      `json:"sms"`
		Webhooks []string      `json:"webhooks"`
		Balance  int64         `json:"balance"`
	}{
		Cards:    []cardView{},
		Receipts: []receiptView{},
		SMS:      []string{},
		Webhooks: s.merchant.Calls(),
	}

	for _, c := range s.cards.Cards() {
		out.Cards = append(out.Cards, cardView{
			Token: c.Token, Number: c.NumberFull,
			Outcome: string(c.Outcome), Verified: c.Verify,
		})
	}
	for _, r := range s.receipts.Receipts() {
		out.Receipts = append(out.Receipts, receiptView{
			ID: r.ReceiptID, Amount: r.Amount, State: int(r.State), Payout: r.Payout,
		})
	}
	for _, m := range s.sms.Sent() {
		out.SMS = append(out.SMS, fmt.Sprintf("%s · %s", m.Phone, m.Text))
	}
	if box, err := s.ledger.Balance(context.Background()); err == nil {
		out.Balance = box.Balance
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		// Marshalling these types cannot fail, but returning a string means
		// the page has something to show if it ever does.
		return `{"error":"could not encode the stand's state"}`
	}
	return string(encoded)
}

func main() {
	current := newStand()

	js.Global().Set("paymeCall", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 {
			return `{"error":"paymeCall(body, auth) needs a request body"}`
		}
		auth := merchantID
		if len(args) > 1 && args[1].Type() == js.TypeString {
			auth = args[1].String()
		}
		return current.call(args[0].String(), auth)
	}))

	js.Global().Set("paymeState", js.FuncOf(func(js.Value, []js.Value) any {
		return current.state()
	}))

	js.Global().Set("paymeReset", js.FuncOf(func(js.Value, []js.Value) any {
		current = newStand()
		return current.state()
	}))

	js.Global().Set("paymeCredentials", js.FuncOf(func(js.Value, []js.Value) any {
		return map[string]any{"merchantID": merchantID, "testKey": testKey}
	}))

	// The page waits on this: the functions above exist only once main has run,
	// and a build that failed to start should not look like one that is slow.
	if ready := js.Global().Get("paymeReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}

	// main returning would tear down the functions it just published.
	select {}
}
