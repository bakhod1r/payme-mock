package httpx_test

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/kernel/httpx"
)

const testKey = "5NPh3f4rTLPa0Vk1LOZ8AT8gK4EbAqTaPHnk"

func basic(login, key string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(login+":"+key))
}

func TestCheckMerchantAuth(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{"correct credentials", basic("Paycom", testKey), true},
		{"lowercase scheme is accepted", "basic " + base64.StdEncoding.EncodeToString([]byte("Paycom:"+testKey)), true},
		{"wrong key", basic("Paycom", "wrong"), false},
		{"empty key", basic("Paycom", ""), false},
		{"wrong login", basic("Merchant", testKey), false},
		{"empty header", "", false},
		{"scheme only", "Basic ", false},
		{"bearer token", "Bearer " + testKey, false},
		{"not base64", "Basic !!!not-base64!!!", false},
		{"base64 without a colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("Paycomkey")), false},
		{"shorter than the scheme", "Bas", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, httpx.CheckMerchantAuth(tt.header, testKey))
		})
	}
}

// The header this package builds must be the header it accepts, so the mock
// and the merchant cannot drift apart.
func TestMerchantAuthHeaderRoundTrip(t *testing.T) {
	header := httpx.MerchantAuthHeader(testKey)

	assert.True(t, httpx.CheckMerchantAuth(header, testKey))
	assert.False(t, httpx.CheckMerchantAuth(header, "another-key"))
}

func TestMerchantAuthHeaderUsesTheProtocolLogin(t *testing.T) {
	header := httpx.MerchantAuthHeader(testKey)

	raw, err := base64.StdEncoding.DecodeString(header[len("Basic "):])
	require.NoError(t, err)
	assert.Equal(t, "Paycom:"+testKey, string(raw))
}

func TestParseSubscribeAuth(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		wantOK      bool
		wantID      string
		wantKey     string
		wantBackend bool
	}{
		{
			name:   "browser side carries only the id",
			header: "587f72c72cac0d162c722ae2",
			wantOK: true, wantID: "587f72c72cac0d162c722ae2", wantBackend: false,
		},
		{
			name:   "server side carries id and key",
			header: "587f72c72cac0d162c722ae2:" + testKey,
			wantOK: true, wantID: "587f72c72cac0d162c722ae2", wantKey: testKey, wantBackend: true,
		},
		{
			name:   "surrounding space is ignored",
			header: "  587f72c72cac0d162c722ae2  ",
			wantOK: true, wantID: "587f72c72cac0d162c722ae2",
		},
		{
			name:   "an empty key still marks a backend call",
			header: "587f72c72cac0d162c722ae2:",
			wantOK: true, wantID: "587f72c72cac0d162c722ae2", wantBackend: true,
		},
		{"empty header", "", false, "", "", false},
		{"blank header", "   ", false, "", "", false},
		{"missing id", ":" + testKey, false, "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := httpx.ParseSubscribeAuth(tt.header)

			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			assert.Equal(t, tt.wantID, got.MerchantID)
			assert.Equal(t, tt.wantKey, got.Key)
			assert.Equal(t, tt.wantBackend, got.Backend)
		})
	}
}

func TestSubscribeAuthAuthorize(t *testing.T) {
	const merchantID = "587f72c72cac0d162c722ae2"

	tests := []struct {
		name string
		auth httpx.SubscribeAuth
		want bool
	}{
		{
			name: "backend call with the right key",
			auth: httpx.SubscribeAuth{MerchantID: merchantID, Key: testKey, Backend: true},
			want: true,
		},
		{
			name: "backend call with a wrong key",
			auth: httpx.SubscribeAuth{MerchantID: merchantID, Key: "wrong", Backend: true},
			want: false,
		},
		{
			name: "browser call needs no key",
			auth: httpx.SubscribeAuth{MerchantID: merchantID},
			want: true,
		},
		{
			name: "wrong merchant id",
			auth: httpx.SubscribeAuth{MerchantID: "other", Key: testKey, Backend: true},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.auth.Authorize(merchantID, testKey))
		})
	}
}
