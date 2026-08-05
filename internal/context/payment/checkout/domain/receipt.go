// Package domain models the checkout form: the receipt a merchant hands to
// the payer, encoded either as base64 GET parameters or as a POST form.
package domain

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrMalformed reports a checkout receipt that cannot be understood.
var ErrMalformed = errors.New("malformed checkout receipt")

// Language is the checkout interface language.
type Language string

// The supported languages. Russian is the default the documentation names.
const (
	LangRU Language = "ru"
	LangUZ Language = "uz"
	LangEN Language = "en"
)

// Valid reports whether l is a supported language.
func (l Language) Valid() bool {
	return l == LangRU || l == LangUZ || l == LangEN
}

// DefaultCallbackTimeout is the wait before redirecting, in milliseconds.
const DefaultCallbackTimeout = 15

// Receipt is a checkout request: who is paying whom, how much, and where to
// return afterwards.
type Receipt struct {
	MerchantID string
	Account    map[string]string
	Amount     int64
	Lang       Language
	Callback   string
	// CallbackTimeout is the wait before redirecting, in milliseconds.
	CallbackTimeout int
	Currency        int
	Description     string
	Detail          *Detail
}

// Validate reports whether the receipt carries what a payment needs.
func (r *Receipt) Validate() error {
	if r.MerchantID == "" {
		return fmt.Errorf("%w: missing merchant id", ErrMalformed)
	}
	if r.Amount <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrMalformed)
	}
	if len(r.Account) == 0 {
		return fmt.Errorf("%w: missing account", ErrMalformed)
	}
	return nil
}

// ResolveCallback substitutes the placeholders the documentation defines, so
// the merchant learns which transaction completed.
func (r *Receipt) ResolveCallback(transactionID string) string {
	if r.Callback == "" {
		return ""
	}
	return strings.ReplaceAll(r.Callback, ":transaction", transactionID)
}

// EncodeGET renders the receipt as the base64 parameter string a checkout URL
// carries. Account fields are sorted so the same receipt always encodes to the
// same string, which makes the result testable and cacheable.
func (r *Receipt) EncodeGET() string {
	parts := []string{"m=" + r.MerchantID}

	fields := make([]string, 0, len(r.Account))
	for field := range r.Account {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		parts = append(parts, "ac."+field+"="+r.Account[field])
	}

	parts = append(parts, "a="+strconv.FormatInt(r.Amount, 10))

	if r.Lang != "" {
		parts = append(parts, "l="+string(r.Lang))
	}
	if r.Callback != "" {
		parts = append(parts, "c="+r.Callback)
	}
	if r.CallbackTimeout > 0 {
		parts = append(parts, "ct="+strconv.Itoa(r.CallbackTimeout))
	}
	if r.Currency > 0 {
		parts = append(parts, "cr="+strconv.Itoa(r.Currency))
	}

	return base64.StdEncoding.EncodeToString([]byte(strings.Join(parts, ";")))
}

// CheckoutURL builds the full link a payer opens.
func (r *Receipt) CheckoutURL(base string) string {
	return strings.TrimSuffix(base, "/") + "/" + r.EncodeGET()
}
