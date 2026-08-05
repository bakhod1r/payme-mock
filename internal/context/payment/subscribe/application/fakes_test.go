package application_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
)

// errBoom stands in for any infrastructure failure a port can raise.
var errBoom = errors.New("storage is down")

type fakeCards struct {
	byToken map[string]*domain.Card
	byID    map[int64]*domain.Card
	tokens  map[int64]string
	nextID  int64

	failByToken  bool
	failByNumber bool
	failByID     bool
	failCreate   bool
	failUpdate   bool
	failTokenFor bool
	missingByID  bool
}

func newFakeCards() *fakeCards {
	return &fakeCards{
		byToken: make(map[string]*domain.Card),
		byID:    make(map[int64]*domain.Card),
		nextID:  7,
	}
}

func (f *fakeCards) ByToken(_ context.Context, token string) (*domain.Card, error) {
	if f.failByToken {
		return nil, errBoom
	}
	c, ok := f.byToken[token]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return c, nil
}

func (f *fakeCards) ByNumber(_ context.Context, number string) (*domain.Card, error) {
	if f.failByNumber {
		return nil, errBoom
	}
	for _, c := range f.byToken {
		if c.NumberFull == number {
			return c, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakeCards) ByID(_ context.Context, id int64) (*domain.Card, error) {
	if f.failByID {
		return nil, errBoom
	}
	if f.missingByID {
		return nil, domain.ErrNotFound
	}
	c, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return c, nil
}

func (f *fakeCards) Create(_ context.Context, c *domain.Card) error {
	if f.failCreate {
		return errBoom
	}
	c.ID = f.nextID
	f.nextID++
	f.byToken[c.Token] = c
	f.byID[c.ID] = c
	return nil
}

// TokenFor hands out the offered token the first time a card is asked for and
// the same one after that, which is what the real store does per register. The
// fake stands for a single register, so the card is the only key it needs.
func (f *fakeCards) TokenFor(_ context.Context, cardID int64, offered string) (string, error) {
	if f.failTokenFor {
		return "", errBoom
	}

	if f.tokens == nil {
		f.tokens = map[int64]string{}
	}
	if held, ok := f.tokens[cardID]; ok {
		return held, nil
	}

	// A card already in the store was tokenized by this register before, so its
	// own token is the one it holds. The real store keeps that in a table; the
	// fake stands for one register and reads it off the card.
	for _, card := range f.byToken {
		if card.ID == cardID && card.Token != "" {
			f.tokens[cardID] = card.Token
			return card.Token, nil
		}
	}

	f.tokens[cardID] = offered

	return offered, nil
}

func (f *fakeCards) Update(_ context.Context, c *domain.Card) error {
	if f.failUpdate {
		return errBoom
	}
	f.byToken[c.Token] = c
	f.byID[c.ID] = c
	return nil
}

type fakeReceipts struct {
	byID   map[string]*domain.Receipt
	nextID int64

	failByID   bool
	failCreate bool
	failUpdate bool
	failList   bool
	listed     []*domain.Receipt
	lastList   struct {
		from, to      int64
		count, offset int
	}
}

func newFakeReceipts() *fakeReceipts {
	return &fakeReceipts{byID: make(map[string]*domain.Receipt), nextID: 1}
}

func (f *fakeReceipts) ByReceiptID(_ context.Context, id string) (*domain.Receipt, error) {
	if f.failByID {
		return nil, errBoom
	}
	r, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return r, nil
}

func (f *fakeReceipts) Create(_ context.Context, r *domain.Receipt) error {
	if f.failCreate {
		return errBoom
	}
	r.ID = f.nextID
	f.nextID++
	f.byID[r.ReceiptID] = r
	return nil
}

func (f *fakeReceipts) Update(_ context.Context, r *domain.Receipt) error {
	if f.failUpdate {
		return errBoom
	}
	f.byID[r.ReceiptID] = r
	return nil
}

func (f *fakeReceipts) List(_ context.Context, from, to int64, count, offset int) ([]*domain.Receipt, error) {
	if f.failList {
		return nil, errBoom
	}
	f.lastList.from, f.lastList.to = from, to
	f.lastList.count, f.lastList.offset = count, offset
	return f.listed, nil
}

// fakeMerchant records the Merchant API calls the Payme side made, in order.
// The sequence is the contract between the two protocols.
type fakeMerchant struct {
	calls []string

	failCheck   error
	failCreate  error
	failPerform error
	failCancel  error
}

func (f *fakeMerchant) CheckPerformTransaction(_ context.Context, _ int64, _ map[string]string) error {
	f.calls = append(f.calls, "CheckPerformTransaction")
	return f.failCheck
}

func (f *fakeMerchant) CreateTransaction(_ context.Context, id string, _, _ int64, _ map[string]string) (string, error) {
	f.calls = append(f.calls, "CreateTransaction")
	if f.failCreate != nil {
		return "", f.failCreate
	}
	return "merchant-" + id, nil
}

func (f *fakeMerchant) PerformTransaction(_ context.Context, _ string) error {
	f.calls = append(f.calls, "PerformTransaction")
	return f.failPerform
}

func (f *fakeMerchant) CancelTransaction(_ context.Context, _ string, _ int) error {
	f.calls = append(f.calls, "CancelTransaction")
	return f.failCancel
}

// fakeScheduler records the background steps that were queued.
type fakeScheduler struct {
	queued []string
	fail   bool
}

func (f *fakeScheduler) ScheduleAdvance(_ context.Context, receiptID string, _ int) error {
	if f.fail {
		return errBoom
	}
	f.queued = append(f.queued, receiptID)
	return nil
}

// fixedTokens produces predictable identifiers so responses can be asserted
// byte for byte.
type fixedTokens struct{ n int }

func (f *fixedTokens) CardToken() string {
	f.n++
	return fmt.Sprintf("token-%d", f.n)
}

func (f *fixedTokens) ReceiptID() string {
	f.n++
	return fmt.Sprintf("2e0b1bc1f1eb50d487ba268%d", f.n)
}

func (f *fixedTokens) TransactionID() string {
	f.n++
	return fmt.Sprintf("5305e3bab097f420a62ced%02d", f.n)
}

// fakeSMS records deliveries instead of sending them.
type fakeSMS struct {
	sent []string
	fail bool
}

func (f *fakeSMS) Send(_ context.Context, phone, message string) error {
	if f.fail {
		return errBoom
	}
	f.sent = append(f.sent, phone+":"+message)
	return nil
}
