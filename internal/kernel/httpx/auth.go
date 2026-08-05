// Package httpx holds the HTTP plumbing every service shares: authentication,
// sandbox resolution, fault injection and the JSON-RPC entry point.
package httpx

import (
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// MerchantLogin is the fixed username Payme sends in the Basic credentials.
// The protocol always uses this literal; only the key varies.
const MerchantLogin = "Paycom"

// CheckMerchantAuth validates an Authorization header against the expected key.
//
// The comparison is constant time so a wrong key cannot be discovered by
// timing the response, and the login is compared too: a header carrying the
// right key under a different login is not valid Payme traffic.
func CheckMerchantAuth(header, expectedKey string) bool {
	const prefix = "Basic "

	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return false
	}

	login, key, found := strings.Cut(string(raw), ":")
	if !found {
		return false
	}

	loginOK := subtle.ConstantTimeCompare([]byte(login), []byte(MerchantLogin)) == 1
	keyOK := subtle.ConstantTimeCompare([]byte(key), []byte(expectedKey)) == 1

	return loginOK && keyOK
}

// MerchantAuthHeader builds the header Payme would send for a key. The mock
// side and the tests use it so both sides agree on one construction.
func MerchantAuthHeader(key string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(MerchantLogin+":"+key))
}

// SubscribeAuth is the X-Auth credential of the Subscribe API. Browser-side
// calls send only the cash register id; server-side calls append the key.
type SubscribeAuth struct {
	MerchantID string
	Key        string
	// Backend reports whether a key was supplied, which distinguishes a
	// server-side call from a browser-side one.
	Backend bool
}

// ParseSubscribeAuth reads an X-Auth header. It reports false when the header
// is empty or carries no merchant id.
func ParseSubscribeAuth(header string) (SubscribeAuth, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return SubscribeAuth{}, false
	}

	id, key, found := strings.Cut(header, ":")
	if id == "" {
		return SubscribeAuth{}, false
	}

	return SubscribeAuth{MerchantID: id, Key: key, Backend: found}, true
}

// Authorize checks a parsed credential against the expected merchant id and
// key. Browser-side calls carry no key, so only methods marked as browser-side
// may be reached with one.
func (a SubscribeAuth) Authorize(merchantID, key string) bool {
	if subtle.ConstantTimeCompare([]byte(a.MerchantID), []byte(merchantID)) != 1 {
		return false
	}
	if !a.Backend {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(a.Key), []byte(key)) == 1
}
