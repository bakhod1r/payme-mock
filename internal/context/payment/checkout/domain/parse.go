package domain

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseGET decodes the base64 parameter string a checkout URL carries:
//
//	m=587f72c72cac0d162c722ae2;ac.order_id=197;a=500
//
// Keys are m (merchant), ac.<field> (account), a (amount in tiyin), l
// (language), c (callback), ct (callback timeout), cr (currency).
func ParseGET(encoded string) (*Receipt, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: parameters are not base64", ErrMalformed)
	}

	receipt := &Receipt{Account: map[string]string{}}

	for _, pair := range strings.Split(string(raw), ";") {
		if pair == "" {
			continue
		}

		key, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("%w: %q is not a key=value pair", ErrMalformed, pair)
		}

		if err := receipt.applyGETKey(key, value); err != nil {
			return nil, err
		}
	}

	return receipt, receipt.Validate()
}

// applyGETKey assigns one decoded key to the receipt.
func (r *Receipt) applyGETKey(key, value string) error {
	if field, isAccount := strings.CutPrefix(key, "ac."); isAccount {
		if field == "" {
			return fmt.Errorf("%w: account field has no name", ErrMalformed)
		}
		r.Account[field] = value
		return nil
	}

	switch key {
	case "m":
		r.MerchantID = value
	case "a":
		amount, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: amount %q is not a number", ErrMalformed, value)
		}
		r.Amount = amount
	case "l":
		r.Lang = Language(value)
	case "c":
		r.Callback = value
	case "ct":
		timeout, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%w: callback timeout %q is not a number", ErrMalformed, value)
		}
		r.CallbackTimeout = timeout
	case "cr":
		currency, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%w: currency %q is not a number", ErrMalformed, value)
		}
		r.Currency = currency
	default:
		// An unknown key is ignored rather than refused, so a form carrying a
		// parameter this mock does not model still reaches payment.
	}

	return nil
}

// ParsePOST decodes a submitted checkout form. Fields are merchant, amount,
// account[<field>], lang, callback, callback_timeout, description and detail.
func ParsePOST(form url.Values) (*Receipt, error) {
	receipt := &Receipt{
		MerchantID:  form.Get("merchant"),
		Account:     map[string]string{},
		Lang:        Language(form.Get("lang")),
		Callback:    form.Get("callback"),
		Description: form.Get("description"),
	}

	if raw := form.Get("amount"); raw != "" {
		amount, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: amount %q is not a number", ErrMalformed, raw)
		}
		receipt.Amount = amount
	}

	for key, values := range form {
		field, ok := accountField(key)
		if !ok || len(values) == 0 {
			continue
		}
		receipt.Account[field] = values[0]
	}

	if raw := form.Get("callback_timeout"); raw != "" {
		timeout, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: callback timeout %q is not a number", ErrMalformed, raw)
		}
		receipt.CallbackTimeout = timeout
	}

	if raw := form.Get("detail"); raw != "" {
		detail, err := DecodeDetail(raw)
		if err != nil {
			return nil, err
		}
		receipt.Detail = detail
	}

	return receipt, receipt.Validate()
}

// accountField extracts "phone" from "account[phone]".
func accountField(key string) (string, bool) {
	inner, ok := strings.CutPrefix(key, "account[")
	if !ok {
		return "", false
	}
	field, ok := strings.CutSuffix(inner, "]")
	if !ok || field == "" {
		return "", false
	}
	return field, true
}
