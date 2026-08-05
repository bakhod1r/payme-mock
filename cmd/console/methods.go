package main

import (
	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// merchantMethods are the Merchant API methods: the webhook Payme calls on the
// merchant. The wildcard comes first because breaking a stand as a whole is
// more common than breaking one call.
var merchantMethods = []string{
	everyMethod,
	"CheckPerformTransaction",
	"CreateTransaction",
	"PerformTransaction",
	"CancelTransaction",
	"CheckTransaction",
	"GetStatement",
	"ChangePassword",
	"SetFiscalData",
}

// subscribeMethods are the Subscribe API methods: what a merchant's own
// backend and checkout page call on Payme. Cards are tokenized first, then a
// receipt is created, paid and followed to its end.
var subscribeMethods = []string{
	"cards.create",
	"cards.get_verify_code",
	"cards.verify",
	"cards.check",
	"cards.remove",
	"receipts.create",
	"receipts.pay",
	"receipts.send",
	"receipts.cancel",
	"receipts.check",
	"receipts.get",
	"receipts.get_all",
	"receipts.set_fiscal_data",
}

// methodErrors lists the errors each method may answer with.
//
// The protocol defines them per method: PerformTransaction can report that a
// transaction is missing, CheckPerformTransaction cannot, because at that point
// there is no transaction yet. Offering only the ones a method can actually
// return keeps the console from setting up a scenario the real provider would
// never produce.
var methodErrors = map[string][]payerr.Code{
	// Merchant API.
	"CheckPerformTransaction": {payerr.CodeInvalidAmount, payerr.CodeAccountMax, payerr.CodeCannotPerform},
	"CreateTransaction":       {payerr.CodeInvalidAmount, payerr.CodeAccountMax, payerr.CodeCannotPerform},
	"PerformTransaction":      {payerr.CodeTransactionNotFnd, payerr.CodeCannotPerform},
	"CancelTransaction":       {payerr.CodeTransactionNotFnd, payerr.CodeOrderCompleted, payerr.CodeCannotPerform},
	"CheckTransaction":        {payerr.CodeTransactionNotFnd},
	"GetStatement":            {payerr.CodeCannotPerform},
	"ChangePassword":          {payerr.CodeCannotPerform},
	"SetFiscalData":           {payerr.CodeTransactionNotFnd, payerr.CodeCannotPerform},

	// Subscribe API. The card and receipt calls report the same protocol-level
	// failures; what differs is which of them can be reached.
	"cards.create":             {payerr.CodeInvalidAmount, payerr.CodeCannotPerform},
	"cards.get_verify_code":    {payerr.CodeCannotPerform},
	"cards.verify":             {payerr.CodeCannotPerform},
	"cards.check":              {payerr.CodeCannotPerform},
	"cards.remove":             {payerr.CodeCannotPerform},
	"receipts.create":          {payerr.CodeInvalidAmount, payerr.CodeAccountMax, payerr.CodeCannotPerform},
	"receipts.pay":             {payerr.CodeCannotPerform, payerr.CodeTransactionNotFnd},
	"receipts.send":            {payerr.CodeTransactionNotFnd, payerr.CodeCannotPerform},
	"receipts.cancel":          {payerr.CodeTransactionNotFnd, payerr.CodeOrderCompleted, payerr.CodeCannotPerform},
	"receipts.check":           {payerr.CodeTransactionNotFnd},
	"receipts.get":             {payerr.CodeTransactionNotFnd},
	"receipts.get_all":         {payerr.CodeCannotPerform},
	"receipts.set_fiscal_data": {payerr.CodeTransactionNotFnd, payerr.CodeCannotPerform},
}

// generalErrors are the transport and JSON-RPC failures any method can answer
// with, whatever it was asked to do.
var generalErrors = []payerr.Code{
	payerr.CodeUnauthorized,
	payerr.CodeTransport,
	payerr.CodeInvalidHTTP,
	payerr.CodeParse,
	payerr.CodeInvalidRequest,
	payerr.CodeMethodNotFound,
}

// methodGroup is one labelled block of the method dropdown, so the two halves
// of the protocol read as the documentation presents them.
type methodGroup struct {
	Label   string
	Methods []methodOption
}

// methodOption is one entry of the method dropdown, carrying the outcomes that
// method can produce so the form can narrow the second dropdown to them.
type methodOption struct {
	Name string
	// Label is what the operator reads; the wildcard is not a method name.
	Label    string
	Service  string
	Outcomes []outcomeOption
}

// outcomeOption is one entry of the result dropdown.
type outcomeOption struct {
	// Value is what the form submits: "success", "timeout" or an error code.
	Value string
	Label string
}

// buildMethodGroups pairs every method with what it can be made to return.
//
// Success and timeout come first because they are not errors at all: one is
// the stand behaving, the other is it never answering, and between them they
// cover most of what an integration needs to rehearse.
func buildMethodGroups(catalog []errorRow) []methodGroup {
	messages := make(map[int]string, len(catalog))
	for _, entry := range catalog {
		messages[entry.Code] = entry.Message
	}

	return []methodGroup{
		{Label: "Merchant API", Methods: optionsFor(merchantMethods, serviceMerchant, messages)},
		{Label: "Subscribe API", Methods: optionsFor(subscribeMethods, servicePaymeMock, messages)},
	}
}

// The services a rule targets, named as the fault engine spells them.
const (
	serviceMerchant  = "merchant"
	servicePaymeMock = "paymemock"
)

func optionsFor(methods []string, service string, messages map[int]string) []methodOption {
	out := make([]methodOption, 0, len(methods))

	for _, method := range methods {
		option := methodOption{
			Name:    method,
			Label:   methodLabel(method),
			Service: service,
			Outcomes: []outcomeOption{
				{Value: outcomeSuccess, Label: "success (answer normally)"},
				{Value: outcomeTimeout, Label: "timeout (never answer)"},
			},
		}

		for _, code := range codesFor(method) {
			option.Outcomes = append(option.Outcomes, outcomeOption{
				Value: formatCode(code),
				Label: formatCode(code) + " · " + messages[int(code)],
			})
		}

		out = append(out, option)
	}

	return out
}

// codesFor is what a method may answer with: its own errors first, then the
// transport failures that apply to everything.
func codesFor(method string) []payerr.Code {
	if method == everyMethod {
		// With no method chosen, every documented error is on offer; narrowing
		// is what picking a method is for.
		all := make([]payerr.Code, 0, len(payerr.Catalog))
		for _, entry := range payerr.Catalog {
			all = append(all, entry.Code)
		}
		return all
	}

	return append(append([]payerr.Code{}, methodErrors[method]...), generalErrors...)
}

func methodLabel(method string) string {
	if method == everyMethod {
		return "every method"
	}
	return method
}

func formatCode(code payerr.Code) string {
	return itoa(int(code))
}

// itoa avoids pulling strconv in for one call in a template helper.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}

	negative := v < 0
	if negative {
		v = -v
	}

	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}

	if negative {
		return "-" + string(digits)
	}

	return string(digits)
}

// kindOption is one entry of the register kind dropdown.
type kindOption struct {
	Value   string
	Label   string
	Meaning string
}

// registerKinds are the directions a cash register can move money in, in the
// order the console offers them.
func registerKinds() []kindOption {
	out := make([]kindOption, 0, len(billing.Kinds))

	for _, kind := range billing.Kinds {
		out = append(out, kindOption{
			Value:   string(kind),
			Label:   string(kind),
			Meaning: kind.Describe(),
		})
	}

	return out
}
