package domain_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/access/domain"
)

func prefix(t *testing.T, raw string) netip.Prefix {
	t.Helper()

	p, err := domain.ParsePrefix(raw)
	require.NoError(t, err)
	return p
}

// A stand nobody has restricted is not a stand nobody may reach.
func TestEmptyAllowlistAllowsEveryone(t *testing.T) {
	var list domain.Allowlist

	assert.True(t, list.Allows(netip.MustParseAddr("203.0.113.7")))
}

func TestAllowlistAllows(t *testing.T) {
	list := domain.Allowlist{
		{Prefix: prefix(t, "203.0.113.7")},
		{Prefix: prefix(t, "10.0.0.0/8")},
	}

	tests := []struct {
		addr string
		want bool
	}{
		{"203.0.113.7", true},
		{"203.0.113.8", false},
		{"10.4.5.6", true},
		{"11.4.5.6", false},
		// A dual-stack listener reports IPv4 callers as mapped addresses; a
		// rule written in IPv4 has to match one.
		{"::ffff:10.4.5.6", true},
		{"2001:db8::1", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			assert.Equal(t, tt.want, list.Allows(netip.MustParseAddr(tt.addr)))
		})
	}
}

func TestParsePrefix(t *testing.T) {
	assert.Equal(t, "203.0.113.7/32", prefix(t, "203.0.113.7").String(),
		"a bare address admits one machine")
	assert.Equal(t, "2001:db8::1/128", prefix(t, "2001:db8::1").String())
	assert.Equal(t, "10.0.0.0/8", prefix(t, "10.0.0.0/8").String())

	// A prefix whose text disagrees with its effect is stored as what it means.
	assert.Equal(t, "10.0.0.0/8", prefix(t, "10.1.2.3/8").String())

	for _, raw := range []string{"", "  ", "not-an-address", "10.0.0.0/64", "10.0.0.0/x"} {
		_, err := domain.ParsePrefix(raw)
		assert.Error(t, err, raw)
	}
}

// Behind a proxy the peer is the proxy. The rightmost forwarded entry is the
// only one it wrote; everything left of it came from the caller.
func TestClientAddr(t *testing.T) {
	tests := []struct {
		name      string
		remote    string
		forwarded string
		trust     bool
		want      string
		ok        bool
	}{
		{"the peer when nothing is forwarded", "203.0.113.7:5000", "", true, "203.0.113.7", true},
		{"the peer when forwarding is not trusted", "203.0.113.7:5000", "10.0.0.1", false, "203.0.113.7", true},
		{"the rightmost forwarded entry", "172.18.0.1:5000", "9.9.9.9, 203.0.113.7", true, "203.0.113.7", true},
		{"a single forwarded entry", "172.18.0.1:5000", "203.0.113.7", true, "203.0.113.7", true},
		{"junk in the chain is skipped", "172.18.0.1:5000", "203.0.113.7, nonsense", true, "203.0.113.7", true},
		{"an address with no port", "203.0.113.7", "", true, "203.0.113.7", true},
		{"a peer that is not an address", "not-an-address", "", true, "", false},
		{"a mapped peer reads as IPv4", "[::ffff:10.0.0.1]:5000", "", true, "10.0.0.1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, ok := domain.ClientAddr(tt.remote, tt.forwarded, tt.trust)

			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, addr.String())
			}
		})
	}
}
