package main

import (
	"crypto/rand"
	"encoding/hex"
)

// credentials generates the identifiers a new sandbox needs, in the shapes the
// real provider issues them.
type credentials struct{}

// MerchantID returns a 24-character hex cash register identifier, which is the
// form Payme hands out.
func (credentials) MerchantID() string {
	raw := make([]byte, 12)
	// rand.Read from crypto/rand cannot fail; it panics internally instead.
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// Key returns a random API key.
func (credentials) Key() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
