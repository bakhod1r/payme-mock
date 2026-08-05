// Package inmem holds the Subscribe API's ports backed by memory rather than
// by Postgres.
//
// It exists because the stand's protocol logic has no database in it: the
// domain declares ports and the application layer talks to nothing else, so
// the same service that runs against Postgres runs against maps. That is what
// lets the whole Subscribe API be compiled to WebAssembly and answered inside
// a browser tab, with no server behind it — which is what the published
// playground is.
//
// It is not a substitute for the Postgres repositories. Nothing here survives
// the process, nothing is shared between callers, and one process is one
// register: the sandbox scoping every real repository carries has no meaning
// when the whole store belongs to a single page.
package inmem

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	domain "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
)

// Cards is a CardRepository held in memory.
//
// Every method takes the lock: a browser is single-threaded today, but the same
// store is used by the Go tests, and a repository that is only safe by accident
// is one bug away from being unsafe.
type Cards struct {
	mu sync.Mutex
	// byID is the store; the other maps are indexes into it.
	byID   map[int64]*domain.Card
	tokens map[string]int64
	next   int64
}

// NewCards returns an empty card store.
func NewCards() *Cards {
	return &Cards{byID: map[int64]*domain.Card{}, tokens: map[string]int64{}}
}

// ByToken finds the card a register knows by this token.
func (c *Cards) ByToken(_ context.Context, token string) (*domain.Card, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id, ok := c.tokens[token]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return clone(c.byID[id]), nil
}

// ByID finds a card by its own identifier.
func (c *Cards) ByID(_ context.Context, id int64) (*domain.Card, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	card, ok := c.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return clone(card), nil
}

// ByNumber finds the card already held for a number, whoever put it there.
func (c *Cards) ByNumber(_ context.Context, number string) (*domain.Card, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, card := range c.byID {
		if card.NumberFull == number {
			return clone(card), nil
		}
	}
	return nil, domain.ErrNotFound
}

// Create stores a new card and gives it an identifier.
func (c *Cards) Create(_ context.Context, card *domain.Card) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.next++
	card.ID = c.next
	c.byID[card.ID] = clone(card)
	if card.Token != "" {
		c.tokens[card.Token] = card.ID
	}
	return nil
}

// Update writes a card back.
func (c *Cards) Update(_ context.Context, card *domain.Card) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.byID[card.ID]; !ok {
		return domain.ErrNotFound
	}
	c.byID[card.ID] = clone(card)
	return nil
}

// TokenFor returns the token this store holds for a card, taking the offered
// one when it holds none.
//
// A real stand keeps a token per register, so the same card carries a
// different string in each. There is only one register here, so the first
// token issued is the one the card keeps.
func (c *Cards) TokenFor(_ context.Context, cardID int64, offered string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	card, ok := c.byID[cardID]
	if !ok {
		return "", domain.ErrNotFound
	}
	if card.Token == "" {
		card.Token = offered
	}
	c.tokens[card.Token] = cardID
	return card.Token, nil
}

// Cards returns every card held, newest first. It is not a port: the
// playground lists them so a reader can see what the stand is holding.
func (c *Cards) Cards() []*domain.Card {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]*domain.Card, 0, len(c.byID))
	for _, card := range c.byID {
		out = append(out, clone(card))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// clone copies a card on its way out of the store. Handing back the stored
// pointer would let a caller rewrite the store from outside it.
//
// It is never given nil: every caller has already established that the card is
// there, so a guard here would be a branch no test could reach.
func clone(c *domain.Card) *domain.Card {
	copied := *c
	return &copied
}

// Receipts is a ReceiptRepository held in memory.
type Receipts struct {
	mu    sync.Mutex
	byID  map[string]*domain.Receipt
	order []string
}

// NewReceipts returns an empty receipt store.
func NewReceipts() *Receipts {
	return &Receipts{byID: map[string]*domain.Receipt{}}
}

// ByReceiptID finds a receipt by the identifier the protocol uses.
func (r *Receipts) ByReceiptID(_ context.Context, id string) (*domain.Receipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	receipt, ok := r.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneReceipt(receipt), nil
}

// Create stores a new receipt.
func (r *Receipts) Create(_ context.Context, receipt *domain.Receipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.byID[receipt.ReceiptID] = cloneReceipt(receipt)
	r.order = append(r.order, receipt.ReceiptID)
	return nil
}

// Update writes a receipt back.
func (r *Receipts) Update(_ context.Context, receipt *domain.Receipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byID[receipt.ReceiptID]; !ok {
		return domain.ErrNotFound
	}
	r.byID[receipt.ReceiptID] = cloneReceipt(receipt)
	return nil
}

// List returns receipts created within the window, newest first.
func (r *Receipts) List(_ context.Context, from, to int64, count, offset int) ([]*domain.Receipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var matched []*domain.Receipt
	for i := len(r.order) - 1; i >= 0; i-- {
		receipt := r.byID[r.order[i]]
		if receipt.CreateTime < from || (to > 0 && receipt.CreateTime > to) {
			continue
		}
		matched = append(matched, cloneReceipt(receipt))
	}

	if offset >= len(matched) {
		return nil, nil
	}
	matched = matched[offset:]
	if count > 0 && count < len(matched) {
		matched = matched[:count]
	}
	return matched, nil
}

// Receipts returns everything held, newest first, for the playground's own
// listing rather than for the protocol.
func (r *Receipts) Receipts() []*domain.Receipt {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*domain.Receipt, 0, len(r.order))
	for i := len(r.order) - 1; i >= 0; i-- {
		out = append(out, cloneReceipt(r.byID[r.order[i]]))
	}
	return out
}

// cloneReceipt copies a receipt on its way out, for the reason clone does.
func cloneReceipt(r *domain.Receipt) *domain.Receipt {
	copied := *r
	return &copied
}

// Ledger is a CashboxLedger held in memory: one register, one balance.
type Ledger struct {
	mu      sync.Mutex
	balance int64
	kind    string
	// payouts records what was opened and what settled, so a balance that
	// looks wrong is a question about a row.
	payouts []domain.Payout
	settled map[string]bool
}

// NewLedger returns a register holding the given balance.
func NewLedger(balance int64, kind string) *Ledger {
	return &Ledger{balance: balance, kind: kind, settled: map[string]bool{}}
}

// OpenPayout records a payout that has been asked for. Nothing moves yet.
func (l *Ledger) OpenPayout(_ context.Context, payout domain.Payout) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.payouts = append(l.payouts, payout)
	return nil
}

// SettlePayout takes the money out of the register.
//
// A payout settled twice would move the balance twice, and the protocol allows
// a settling call to be repeated, so the identifier is remembered.
func (l *Ledger) SettlePayout(_ context.Context, payout domain.Payout) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.settled[payout.TransactionID] {
		return nil
	}
	l.settled[payout.TransactionID] = true
	l.balance -= payout.Amount
	return nil
}

// Balance is what the register holds now.
func (l *Ledger) Balance(_ context.Context) (domain.Cashbox, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return domain.Cashbox{Balance: l.balance, Currency: 860, Kind: l.kind}, nil
}

// Merchant is a MerchantClient that always agrees.
//
// The playground has no merchant behind it — there is no backend to call — so
// the provider's side is rehearsed alone. Every check passes and every
// transaction is accepted, which makes the card's own rigging the only thing
// that decides how a payment ends.
type Merchant struct {
	mu sync.Mutex
	// calls records what would have gone out, so the page can show the webhook
	// chain a real integration would have received.
	calls []string
}

// NewMerchant returns a merchant that accepts everything.
func NewMerchant() *Merchant { return &Merchant{} }

// CheckPerformTransaction always agrees the payment may go ahead.
func (m *Merchant) CheckPerformTransaction(_ context.Context, _ int64, _ map[string]string) error {
	m.record("CheckPerformTransaction")
	return nil
}

// CreateTransaction accepts the transaction and names it.
func (m *Merchant) CreateTransaction(_ context.Context, id string, _, _ int64, _ map[string]string) (string, error) {
	m.record("CreateTransaction")
	return "merchant-" + id, nil
}

// PerformTransaction settles on the merchant's side.
func (m *Merchant) PerformTransaction(_ context.Context, _ string) error {
	m.record("PerformTransaction")
	return nil
}

// CancelTransaction cancels on the merchant's side.
func (m *Merchant) CancelTransaction(_ context.Context, _ string, _ int) error {
	m.record("CancelTransaction")
	return nil
}

// Calls returns the webhook chain so far.
func (m *Merchant) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.calls...)
}

func (m *Merchant) record(method string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, method)
}

// Scheduler is a Scheduler that settles immediately.
//
// A real stand advances a receipt on a worker after a delay, because real
// payments are not instantaneous. There is no worker in a browser tab and
// nothing to run one, so the delay is dropped rather than faked: a reader
// watching a payment settle at once is told the truth about this build, where a
// spinner that never resolved would not be.
type Scheduler struct{}

// NewScheduler returns the immediate scheduler.
func NewScheduler() Scheduler { return Scheduler{} }

// ScheduleAdvance does nothing at all.
func (Scheduler) ScheduleAdvance(_ context.Context, _ string, _ int) error { return nil }

// SMS is an SMSSender that keeps what it was asked to send.
//
// It is the useful half of a stand's SMS: nothing is delivered, but the code is
// readable, which is what lets a verification be rehearsed without a phone.
type SMS struct {
	mu   sync.Mutex
	sent []Message
}

// Message is one delivery the stand did not make.
type Message struct {
	Phone string
	Text  string
}

// NewSMS returns an empty outbox.
func NewSMS() *SMS { return &SMS{} }

// Send records the message.
func (s *SMS) Send(_ context.Context, phone, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sent = append(s.sent, Message{Phone: phone, Text: text})
	return nil
}

// Sent returns the outbox, newest last.
func (s *SMS) Sent() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Message(nil), s.sent...)
}

// Last returns the most recent message, and whether there was one.
func (s *SMS) Last() (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.sent) == 0 {
		return Message{}, false
	}
	return s.sent[len(s.sent)-1], true
}

// Tokens produces identifiers that look like the provider's without needing a
// source of randomness.
//
// A browser has crypto, so this is not about what is available: it is about a
// playground whose ids are the same on every run, so a page can be reloaded and
// read against what it said before.
type Tokens struct {
	mu sync.Mutex
	n  int
}

// NewTokens returns a counter-backed generator.
func NewTokens() *Tokens { return &Tokens{} }

// CardToken returns a 64-character token, as the provider issues.
func (t *Tokens) CardToken() string {
	return t.pad("card", 64)
}

// ReceiptID returns a 24-character hex identifier, as the provider issues.
func (t *Tokens) ReceiptID() string {
	return t.pad("a1b2c3d4e5f6", 24)
}

// TransactionID returns a 24-character hex identifier.
func (t *Tokens) TransactionID() string {
	return t.pad("f6e5d4c3b2a1", 24)
}

// pad builds one identifier of exactly the given width.
//
// The counter goes at the end so two ids differ in their last characters, which
// is where a reader comparing them looks. Width is held by the format rather
// than by a trailing truncation, so there is no arm that only fires if somebody
// picks a prefix longer than the identifier.
func (t *Tokens) pad(prefix string, width int) string {
	t.mu.Lock()
	t.n++
	n := t.n
	t.mu.Unlock()

	tail := fmt.Sprintf("%04d", n%10000)
	body := prefix + strings.Repeat("0", width)

	return body[:width-len(tail)] + tail
}
