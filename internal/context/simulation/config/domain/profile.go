// Package domain models named configuration profiles: a set of tunables plus
// the fault rules that belong with them, switchable in one click.
package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalid reports a profile that cannot be saved as asked.
var ErrInvalid = errors.New("invalid configuration profile")

// ErrNotFound is what the repository returns when no profile matches.
var ErrNotFound = errors.New("configuration profile not found")

// ErrBuiltinImmutable reports an attempt to delete a seeded profile.
var ErrBuiltinImmutable = errors.New("built-in profile cannot be deleted")

// Settings are the tunables a profile carries. They reach every bounded
// context per request, so switching a profile takes effect without a restart.
type Settings struct {
	// TransactionTimeoutMillis is the confirmation window for a created
	// transaction. Twelve hours by default.
	TransactionTimeoutMillis int64 `json:"transaction_timeout_ms"`
	// AccountField is the account object key the merchant identifies payers by.
	AccountField string `json:"account_field"`
	// CardVerifyCode is the OTP the mock accepts.
	CardVerifyCode string `json:"card_verify_code"`
	// CardVerifyWaitMillis is how long that code stays usable.
	CardVerifyWaitMillis int64 `json:"card_verify_wait_ms"`
	// StepDelayMillis spaces out the receipt state walk.
	StepDelayMillis int `json:"step_delay_ms"`
	// DefaultDelayMillis delays every response, faulted or not.
	DefaultDelayMillis int `json:"default_delay_ms"`
	// HoldWindowMillis is how long held funds survive. Uzcard releases after
	// thirty days.
	HoldWindowMillis int64 `json:"hold_window_ms"`
	// StatementPageSize caps a GetStatement response.
	StatementPageSize int `json:"statement_page_size"`
	// CardBalance is the balance a newly tokenized card starts with.
	CardBalance int64 `json:"card_balance"`
	// State5Meaning resolves the documentation's contradiction about receipt
	// state 5: "hold" or "archived".
	State5Meaning string `json:"state5_meaning"`
	// AutoRegisterAccounts makes the merchant accept a payer it has never
	// seen, registering them and the order as the payment arrives.
	//
	// A cash register that charges saved cards does not always identify the
	// payer beforehand: the account it sends is generated per payment and
	// matches nothing the merchant holds. Off, that is -31050 and no such
	// integration can be exercised against the stand at all.
	AutoRegisterAccounts bool `json:"auto_register_accounts"`
	// IPAllowlist restricts who may reach the webhook in production. Empty
	// means the check is off.
	IPAllowlist []string `json:"ip_allowlist,omitempty"`
}

// The two readings of receipt state 5.
const (
	State5Hold     = "hold"
	State5Archived = "archived"
)

// DefaultSettings are the values a stand starts from: the documented timings,
// no injected delay, nothing broken.
func DefaultSettings() Settings {
	return Settings{
		TransactionTimeoutMillis: 43_200_000, // 12 hours
		AccountField:             "order_id",
		CardVerifyCode:           "666666",
		CardVerifyWaitMillis:     60_000,
		StepDelayMillis:          250,
		DefaultDelayMillis:       0,
		HoldWindowMillis:         30 * 24 * 60 * 60 * 1000, // 30 days
		StatementPageSize:        1000,
		CardBalance:              100_000_000,
		State5Meaning:            State5Hold,
	}
}

// Validate reports settings that would make the stand behave nonsensically.
func (s Settings) Validate() error {
	switch {
	case s.TransactionTimeoutMillis <= 0:
		return fmt.Errorf("%w: transaction timeout must be positive", ErrInvalid)
	case s.AccountField == "":
		return fmt.Errorf("%w: account field is required", ErrInvalid)
	case s.CardVerifyCode == "":
		return fmt.Errorf("%w: card verify code is required", ErrInvalid)
	case s.StepDelayMillis < 0:
		return fmt.Errorf("%w: step delay cannot be negative", ErrInvalid)
	case s.DefaultDelayMillis < 0:
		return fmt.Errorf("%w: default delay cannot be negative", ErrInvalid)
	case s.StatementPageSize <= 0:
		return fmt.Errorf("%w: statement page size must be positive", ErrInvalid)
	case s.State5Meaning != State5Hold && s.State5Meaning != State5Archived:
		return fmt.Errorf("%w: state 5 means %q or %q", ErrInvalid, State5Hold, State5Archived)
	}
	return nil
}

// Profile is a named set of settings. Built-in profiles are seeded and cannot
// be deleted, though their settings may be edited.
type Profile struct {
	ID          int64
	Name        string
	Description string
	Settings    Settings
	Builtin     bool
}

// NewProfile builds a profile after checking its name and settings.
func NewProfile(name, description string, settings Settings) (*Profile, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}

	if err := settings.Validate(); err != nil {
		return nil, err
	}

	return &Profile{Name: name, Description: description, Settings: settings}, nil
}

// Deletable reports whether the profile may be removed.
func (p *Profile) Deletable() bool { return !p.Builtin }

// Repository stores configuration profiles.
type Repository interface {
	ByID(ctx context.Context, id int64) (*Profile, error)
	ByName(ctx context.Context, name string) (*Profile, error)
	List(ctx context.Context) ([]*Profile, error)
	Create(ctx context.Context, p *Profile) error
	Update(ctx context.Context, p *Profile) error
	Delete(ctx context.Context, id int64) error
}
