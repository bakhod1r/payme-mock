// Package application implements the thirteen Subscribe API methods. Response
// shapes mirror the documentation exactly, so a caller cannot tell the mock
// from the real provider.
package application

import (
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// CardView is the `card` object every card method returns.
type CardView struct {
	Number    string `json:"number"`
	Expire    string `json:"expire"`
	Token     string `json:"token"`
	Recurrent bool   `json:"recurrent"`
	Verify    bool   `json:"verify"`
}

func viewOf(c *domain.Card) CardView {
	return CardView{
		Number:    c.NumberMask(),
		Expire:    c.Expire,
		Token:     c.Token,
		Recurrent: c.Recurrent,
		Verify:    c.Verify,
	}
}

// CardResult wraps a card view, matching the documented `{"card": {...}}`.
type CardResult struct {
	Card CardView `json:"card"`
}

// CardsCreateParams are the parameters of cards.create.
//
// Account and Customer are optional and the provider does nothing with them
// beyond keeping them against the token: an application sends the account it
// intends to pay, or its own identifier for the payer, so that a card saved on
// one screen can be recognized on another.
type CardsCreateParams struct {
	Card struct {
		Number string `json:"number"`
		Expire string `json:"expire"`
	} `json:"card"`
	Account  map[string]string `json:"account,omitempty"`
	Save     bool              `json:"save"`
	Customer string            `json:"customer,omitempty"`
}

// CardsTokenParams are the parameters of the methods that take only a token.
type CardsTokenParams struct {
	Token string `json:"token"`
}

// VerifyCodeResult is the response of cards.get_verify_code.
type VerifyCodeResult struct {
	Sent  bool   `json:"sent"`
	Phone string `json:"phone"`
	Wait  int64  `json:"wait"`
}

// CardsVerifyParams are the parameters of cards.verify.
type CardsVerifyParams struct {
	Token string `json:"token"`
	Code  string `json:"code"`
}

// SuccessResult is the response of methods that only report completion.
type SuccessResult struct {
	Success bool `json:"success"`
}

// ReceiptView is the `receipt` object receipt methods return. The field set
// and their order follow the documented responses, including the ones a stand
// has nothing to say about: a client decoding the full object must find them
// present and null rather than missing.
type ReceiptView struct {
	ID          string              `json:"_id"`
	CreateTime  int64               `json:"create_time"`
	PayTime     int64               `json:"pay_time"`
	CancelTime  int64               `json:"cancel_time"`
	State       domain.ReceiptState `json:"state"`
	Type        int                 `json:"type"`
	External    bool                `json:"external"`
	Operation   int                 `json:"operation"`
	Category    any                 `json:"category"`
	Error       any                 `json:"error"`
	Description string              `json:"description"`
	Detail      map[string]any      `json:"detail"`
	Amount      int64               `json:"amount"`
	Currency    int                 `json:"currency"`
	Commission  int64               `json:"commission"`
	Account     []AccountFieldView  `json:"account"`
	// Card is null until the receipt is paid, and carries only the masked
	// number and expiry once it is: the token belongs to the card methods.
	Card         *ReceiptCardView `json:"card"`
	Merchant     MerchantView     `json:"merchant"`
	Meta         map[string]any   `json:"meta"`
	ProcessingID any              `json:"processing_id"`
}

// ReceiptCardView is the `card` object inside a receipt.
type ReceiptCardView struct {
	Number string `json:"number"`
	Expire string `json:"expire"`
}

// MerchantView is the `merchant` object inside a receipt. A stand keeps only
// the cash register id, so the rest of the documented shape is reported empty
// rather than invented.
type MerchantView struct {
	ID           string         `json:"_id"`
	Name         string         `json:"name"`
	Organization string         `json:"organization"`
	Address      string         `json:"address"`
	Epos         EposView       `json:"epos"`
	Date         int64          `json:"date"`
	Logo         any            `json:"logo"`
	Type         string         `json:"type"`
	Terms        any            `json:"terms"`
	Payer        map[string]any `json:"payer,omitempty"`
}

// EposView is the terminal the register is served by.
type EposView struct {
	MerchantID string `json:"merchantId"`
	TerminalID string `json:"terminalId"`
}

// AccountFieldView is one entry of a receipt's `account`. The provider returns
// the account as a list of labelled fields rather than the object the caller
// sent, which is what a client written against the real API decodes.
//
// The title is the three-language object the documented receipts.get_all
// response carries. The older examples on other pages show a bare string; the
// localized form is the one a client can actually display to a payer, and it
// is the shape the newest documented response uses.
type AccountFieldView struct {
	Name  string         `json:"name"`
	Title payerr.Message `json:"title"`
	Value string         `json:"value"`
	// Main marks the field the provider shows first, which the documented
	// response carries and a client renders larger than the rest.
	Main bool `json:"main"`
}

// ReceiptResult wraps a receipt view.
type ReceiptResult struct {
	Receipt ReceiptView `json:"receipt"`
}

// ReceiptsCreateParams are the parameters of receipts.create.
type ReceiptsCreateParams struct {
	Amount      int64             `json:"amount"`
	Account     map[string]string `json:"account"`
	Detail      map[string]any    `json:"detail,omitempty"`
	Description string            `json:"description,omitempty"`
	Hold        bool              `json:"hold,omitempty"`
}

// ReceiptsPayParams are the parameters of receipts.pay.
type ReceiptsPayParams struct {
	ID    string         `json:"id"`
	Token string         `json:"token"`
	Payer map[string]any `json:"payer,omitempty"`
	Hold  bool           `json:"hold,omitempty"`
}

// TransactionsCreateParams are the parameters of transactions.create, which
// opens a payout to a saved card.
type TransactionsCreateParams struct {
	Amount  int64             `json:"amount"`
	Token   string            `json:"token"`
	Account map[string]string `json:"account"`
}

// TransactionsCompleteParams are the parameters of transactions.complete. The
// amount and the card are repeated so the settling call can be checked against
// the payout it claims to settle.
type TransactionsCompleteParams struct {
	ID      string            `json:"id"`
	Amount  int64             `json:"amount"`
	Token   string            `json:"token"`
	Account map[string]string `json:"account"`
}

// ReceiptsIDParams are the parameters of methods that take only a receipt id.
type ReceiptsIDParams struct {
	ID string `json:"id"`
}

// ReceiptsSendParams are the parameters of receipts.send.
type ReceiptsSendParams struct {
	ID    string `json:"id"`
	Phone string `json:"phone"`
}

// ReceiptsCheckResult is the response of receipts.check.
type ReceiptsCheckResult struct {
	State domain.ReceiptState `json:"state"`
}

// ReceiptsGetAllParams are the parameters of receipts.get_all.
type ReceiptsGetAllParams struct {
	Count  int   `json:"count"`
	From   int64 `json:"from"`
	To     int64 `json:"to"`
	Offset int   `json:"offset"`
}

// AccountsGetBalanceParams are the parameters of accounts.getBalance. The
// register is already named by the credentials the call is made with; the
// merchant id is sent as well, and a call naming another merchant is refused
// rather than quietly answered with the caller's own figure.
type AccountsGetBalanceParams struct {
	MerchantID string `json:"merchant_id"`
}

// AccountsGetBalanceResult is what the register holds, in tiyin, as every
// amount in the protocol is.
type AccountsGetBalanceResult struct {
	Balance int64 `json:"balance"`
}
