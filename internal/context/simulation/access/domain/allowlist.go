// Package domain holds the per-sandbox address rules: who may reach a stand's
// API at all, before any credential is looked at.
package domain

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ErrNotFound reports a rule that is not there.
var ErrNotFound = errors.New("no such ip rule")

// Rule is one address or network allowed to reach a stand.
type Rule struct {
	ID        int64
	SandboxID int64
	// Prefix is the network the rule allows. A single address is held as a
	// full-length prefix, so one field covers "this machine" and "this
	// network" and the test is the same either way.
	Prefix netip.Prefix
	Note   string
}

// Allowlist is every rule a stand carries.
type Allowlist []Rule

// Allows reports whether an address may reach the stand.
//
// An empty list allows everyone. That is the state every stand starts in, and
// it is the honest reading of "no rules were written": a stand nobody has
// restricted is not a stand nobody may reach.
func (a Allowlist) Allows(addr netip.Addr) bool {
	if len(a) == 0 {
		return true
	}

	// A mapped IPv4 address (::ffff:10.0.0.1) must be compared as the IPv4 it
	// is, or a rule written as 10.0.0.0/8 would never match a request that
	// arrived over a dual-stack listener.
	addr = addr.Unmap()

	for _, rule := range a {
		if rule.Prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// ParsePrefix reads what an operator typed: an address, or an address with a
// mask. A bare address becomes a full-length prefix, so "this one machine" and
// "this network" are written the way each is normally written.
func ParsePrefix(raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Prefix{}, fmt.Errorf("an address is required")
	}

	if strings.Contains(raw, "/") {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not an address and mask", raw)
		}
		// Masked returns the network the mask actually describes, so a rule
		// entered as 10.1.2.3/8 is stored as the 10.0.0.0/8 it means rather
		// than as a prefix whose text disagrees with its effect.
		return prefix.Masked(), nil
	}

	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address", raw)
	}

	return netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()), nil
}

// ClientAddr reads the address a request came from.
//
// Behind the gateway, or behind Docker, the peer is a proxy rather than the
// caller, so a trusted deployment reads the forwarded chain instead. The
// rightmost entry is taken: everything further left was written by whoever is
// calling and can say anything at all.
func ClientAddr(remoteAddr, forwardedFor string, trustForwarded bool) (netip.Addr, bool) {
	if trustForwarded && forwardedFor != "" {
		parts := strings.Split(forwardedFor, ",")

		for i := len(parts) - 1; i >= 0; i-- {
			if addr, err := netip.ParseAddr(strings.TrimSpace(parts[i])); err == nil {
				return addr.Unmap(), true
			}
		}
	}

	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}

	return addr.Unmap(), true
}
