// Package domain models a sandbox: an isolated stand with its own credentials,
// data and fault rules, so one experiment cannot disturb another.
package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalid reports a sandbox that cannot be created as asked.
var ErrInvalid = errors.New("invalid sandbox")

// ErrNotFound is what the repository returns when no sandbox matches.
var ErrNotFound = errors.New("sandbox not found")

// ErrDuplicate reports a slug or cash register id already in use. Both appear
// in addresses integrations have been given, so neither may be reassigned.
var ErrDuplicate = errors.New("sandbox already exists")

// slugPattern is what a sandbox slug may contain. It appears in the endpoint
// URL, so it stays to characters that need no escaping.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

// Sandbox is an isolated stand.
type Sandbox struct {
	ID   int64
	Slug string
	Name string
	// MerchantID is the cash register identifier, a 24-character hex string
	// like the ones the real provider issues.
	MerchantID string
	// Key authenticates production traffic and TestKey authenticates sandbox
	// traffic, mirroring the two keys the provider hands out.
	Key      string
	TestKey  string
	ConfigID *int64
	Archived bool
	// Kind is the direction this register moves money in. It is carried as a
	// string because the meaning belongs to billing, not to the stand: here it
	// is only something to store and hand on.
	Kind string
	// MerchantName is the organization the payer sees on a receipt: what the
	// provider reports in the receipt's merchant object. It is not the stand's
	// own name, which is an operator's label for it and never leaves the
	// console.
	MerchantName string
	// MerchantGroup names the merchant these registers belong to. Stands that
	// share a name share their cards, because a merchant's card is the
	// merchant's whatever register the payment goes through. Empty means the
	// stand is alone and sees only its own cards.
	MerchantGroup string
}

// New builds a sandbox, generating its credentials.
func New(slug, name string, gen CredentialGenerator) (*Sandbox, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))

	if !slugPattern.MatchString(slug) {
		return nil, fmt.Errorf("%w: slug %q must be 2-40 lowercase letters, digits or hyphens", ErrInvalid, slug)
	}

	if strings.TrimSpace(name) == "" {
		name = slug
	}

	return &Sandbox{
		Slug:       slug,
		Name:       name,
		MerchantID: gen.MerchantID(),
		Key:        gen.Key(),
		TestKey:    gen.Key(),
	}, nil
}

// EndpointURL is the address to paste into the cash register settings. Each
// sandbox gets its own path so one gateway serves all of them.
func (s *Sandbox) EndpointURL(base string) string {
	return fmt.Sprintf("%s/s/%s/payme/merchant", strings.TrimSuffix(base, "/"), s.Slug)
}

// SubscribeURL is the Subscribe API address for this sandbox.
func (s *Sandbox) SubscribeURL(base string) string {
	return fmt.Sprintf("%s/s/%s/api", strings.TrimSuffix(base, "/"), s.Slug)
}

// KeyFor returns the key that authenticates traffic of the given kind.
func (s *Sandbox) KeyFor(test bool) string {
	if test {
		return s.TestKey
	}
	return s.Key
}

// CredentialGenerator produces the identifiers a new sandbox needs.
type CredentialGenerator interface {
	// MerchantID returns a 24-character hex cash register identifier.
	MerchantID() string
	// Key returns a random API key.
	Key() string
}

// Repository stores sandboxes.
type Repository interface {
	BySlug(ctx context.Context, slug string) (*Sandbox, error)
	ByMerchantID(ctx context.Context, merchantID string) (*Sandbox, error)
	List(ctx context.Context) ([]*Sandbox, error)
	Create(ctx context.Context, s *Sandbox) error
	Update(ctx context.Context, s *Sandbox) error
	Delete(ctx context.Context, id int64) error
	// Reset clears a sandbox's data while keeping its credentials.
	Reset(ctx context.Context, id int64) error
}
