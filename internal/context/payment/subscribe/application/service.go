package application

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// MaxGetAllCount is the ceiling the documentation puts on receipts.get_all.
const MaxGetAllCount = 50

// Settings are the tunables the console exposes for the Payme side.
type Settings struct {
	SandboxID  int64
	MerchantID string
	// MerchantName is the organization the payer sees on the receipt. The stand
	// reported it empty before it had anywhere to keep it, which made a rendered
	// receipt read as a payment to nobody.
	MerchantName string
	// VerifyCode is the fallback OTP, "666666" by default. A card's own expiry
	// spells out the code it takes; this is what is sent when an expiry spells
	// none.
	VerifyCode string
	// VerifyWaitMillis is how long that code stays usable.
	VerifyWaitMillis int64
	// StepDelayMillis spaces out the receipt state walk, so payment takes
	// plausible time instead of completing instantly.
	StepDelayMillis int
	// HoldWindowMillis is how long held funds survive before release.
	HoldWindowMillis int64
	// CardBalance is the starting balance given to a newly tokenized card.
	CardBalance int64
}

// Service implements the Subscribe API.
type Service struct {
	cards     domain.CardRepository
	receipts  domain.ReceiptRepository
	merchant  domain.MerchantClient
	scheduler domain.Scheduler
	tokens    domain.TokenGenerator
	sms       domain.SMSSender
	clock     clock.Clock
	settings  Settings
	// ledger moves the register's own balance for a payout. It is optional: a
	// stand wired without it still pays out, it just does not move the figure.
	ledger domain.CashboxLedger
}

// NewService wires the Subscribe API to its ports.
func NewService(
	cards domain.CardRepository,
	receipts domain.ReceiptRepository,
	merchant domain.MerchantClient,
	scheduler domain.Scheduler,
	tokens domain.TokenGenerator,
	sms domain.SMSSender,
	clk clock.Clock,
	settings Settings,
	options ...Option,
) *Service {
	s := &Service{
		cards: cards, receipts: receipts, merchant: merchant,
		scheduler: scheduler, tokens: tokens, sms: sms,
		clock: clk, settings: settings,
	}

	for _, option := range options {
		option(s)
	}

	return s
}

// Option adjusts a service after it is built. The ports every method needs are
// arguments; a port only one path uses is an option, so the constructor does
// not grow a parameter that most callers would pass nil for.
type Option func(*Service)

// WithLedger gives the service somewhere to record what a payout did to the
// register's balance. Without it a payout still settles and still credits the
// card — the register's own figure is simply not moved, which is what a stand
// wired up before the ledger existed did.
func WithLedger(ledger domain.CashboxLedger) Option {
	return func(s *Service) { s.ledger = ledger }
}

// ---------- cards ----------

// CardsCreate tokenizes a card. A card saved with save=false yields a
// single-use token that is discarded after payment.
func (s *Service) CardsCreate(ctx context.Context, p CardsCreateParams) (*CardResult, error) {
	if p.Card.Number == "" || p.Card.Expire == "" {
		return nil, payerr.ErrInvalidRequest
	}

	card := &domain.Card{
		SandboxID:  s.settings.SandboxID,
		Token:      s.tokens.CardToken(),
		NumberFull: p.Card.Number,
		Expire:     domain.FormatExpire(p.Card.Expire),
		Recurrent:  p.Save,
		Balance:    s.settings.CardBalance,
		// A card the register tokenized is an ordinary one: it can be texted,
		// its balance moves, and it answers at once. Only an operator rigs a
		// card into anything else.
		SMSEnabled: true,
		Account:    p.Account,
		Customer:   p.Customer,
		// Tokenized by the register, here and now.
		RegisteredAt: s.clock.NowMillis(),
	}

	// A card the merchant already holds is handed back as it stands, with the
	// token it already has.
	//
	// A card is one card: one balance, one behaviour, one verification. Adding
	// a second row for the same number would split all three, and it is the
	// same number the payer would tap either way. It also keeps an operator's
	// rigging in force after the integration tokenizes that number, which is
	// the whole reason the provider's test numbers are on the stand.
	held, err := s.cards.ByNumber(ctx, card.NumberFull)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if err == nil {
		// This register gets its own token for the card, or the one it was
		// handed before: a card is shared across the merchant, a token is not.
		token, err := s.cards.TokenFor(ctx, held.ID, card.Token)
		if err != nil {
			return nil, err
		}
		held.Token = token

		return &CardResult{Card: viewOf(s.reregister(ctx, held, p))}, nil
	}

	if err := s.cards.Create(ctx, card); err != nil {
		return nil, err
	}

	return &CardResult{Card: viewOf(card)}, nil
}

// reregister brings a held card up to date with what this registration said
// about it, without touching what the card is.
//
// The balance, the behaviour and the verification stay: they are the card's,
// not this call's. What the caller may still say is who it is saving the card
// for, and whether the token is to be reusable — a card first tokenized for a
// single payment and saved properly later must end up saved.
func (s *Service) reregister(ctx context.Context, card *domain.Card, p CardsCreateParams) *domain.Card {
	// The registration itself is news about the card however little else this
	// call changed: it is what tells the console the register holds this card
	// and not only that an operator once added it.
	card.Register(s.clock.NowMillis())
	changed := true

	if p.Save && !card.Recurrent {
		card.Recurrent = true
		changed = true
	}
	if len(p.Account) > 0 {
		card.Account = p.Account
		changed = true
	}
	if p.Customer != "" {
		card.Customer = p.Customer
		changed = true
	}

	if changed {
		// A failed refresh must not fail the registration: the token the caller
		// asked for is valid either way, and the fields it would have updated
		// are a note about the card rather than part of it.
		_ = s.cards.Update(ctx, card)
	}

	return card
}

// CardsGetVerifyCode issues an OTP and reports where it was sent.
func (s *Service) CardsGetVerifyCode(ctx context.Context, p CardsTokenParams) (*VerifyCodeResult, error) {
	card, err := s.loadCard(ctx, p.Token)
	if err != nil {
		return nil, err
	}

	// A stopped or expired card is refused before an OTP is spent on it, which
	// is where the real provider refuses one too.
	if err := card.Operable(); err != nil {
		return nil, err
	}

	// A card whose owner never signed up for SMS cannot be sent a code at all,
	// which is a different failure from a code that turns out to be wrong.
	if err := card.SMSReachable(); err != nil {
		return nil, err
	}

	wait := s.settings.VerifyWaitMillis
	if wait <= 0 {
		wait = domain.DefaultVerifyWaitMillis
	}

	code := card.ExpectedVerifyCode(s.settings.VerifyCode)

	card.SendVerifyCode(code, s.clock.NowMillis(), wait)
	if err := s.cards.Update(ctx, card); err != nil {
		return nil, err
	}

	if s.sms != nil && card.Phone != "" {
		// A failed SMS must not fail the call: the provider reports delivery
		// optimistically and the code stays valid either way.
		_ = s.sms.Send(ctx, card.Phone, code)
	}

	return &VerifyCodeResult{Sent: true, Phone: maskPhone(card.Phone), Wait: wait}, nil
}

// CardsVerify confirms a card with the code sent over SMS.
func (s *Service) CardsVerify(ctx context.Context, p CardsVerifyParams) (*CardResult, error) {
	card, err := s.loadCard(ctx, p.Token)
	if err != nil {
		return nil, err
	}

	if err := card.VerifyWith(p.Code, s.clock.NowMillis()); err != nil {
		return nil, err
	}

	if err := s.cards.Update(ctx, card); err != nil {
		return nil, err
	}

	return &CardResult{Card: viewOf(card)}, nil
}

// CardsCheck reports a token's current standing.
//
// A card the bank stopped, or one the provider errors on, refuses to be read
// as much as it refuses to pay: an integration that polls a token must see the
// same answer here as it would at the till.
func (s *Service) CardsCheck(ctx context.Context, p CardsTokenParams) (*CardResult, error) {
	card, err := s.loadCard(ctx, p.Token)
	if err != nil {
		return nil, err
	}

	if err := card.Operable(); err != nil {
		return nil, err
	}

	return &CardResult{Card: viewOf(card)}, nil
}

// CardsRemove deletes a token.
func (s *Service) CardsRemove(ctx context.Context, p CardsTokenParams) (*SuccessResult, error) {
	card, err := s.loadCard(ctx, p.Token)
	if err != nil {
		return nil, err
	}

	card.Removed = true
	if err := s.cards.Update(ctx, card); err != nil {
		return nil, err
	}

	return &SuccessResult{Success: true}, nil
}

// ---------- receipts ----------

// ReceiptsCreate opens a receipt awaiting payment.
func (s *Service) ReceiptsCreate(ctx context.Context, p ReceiptsCreateParams) (*ReceiptResult, error) {
	if p.Amount <= 0 {
		return nil, payerr.ErrInvalidAmount
	}

	// The merchant is asked whether the payment is possible before a receipt
	// exists, which is the order the real provider uses.
	if err := s.merchant.CheckPerformTransaction(ctx, p.Amount, p.Account); err != nil {
		return nil, err
	}

	receipt := domain.NewReceipt(
		s.settings.SandboxID, s.tokens.ReceiptID(), s.settings.MerchantID,
		p.Amount, p.Account, s.clock.NowMillis(),
	)
	receipt.Detail = p.Detail
	receipt.Description = p.Description
	receipt.Hold = p.Hold

	if err := s.receipts.Create(ctx, receipt); err != nil {
		return nil, err
	}

	return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
}

// ReceiptsPay charges a card and drives the merchant's Merchant API through
// CheckPerformTransaction, CreateTransaction and PerformTransaction.
func (s *Service) ReceiptsPay(ctx context.Context, p ReceiptsPayParams) (*ReceiptResult, error) {
	receipt, err := s.loadReceipt(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	card, err := s.loadCard(ctx, p.Token)
	if err != nil {
		return nil, err
	}

	if err := card.Usable(receipt.Amount); err != nil {
		return nil, err
	}

	now := s.clock.NowMillis()
	hold := p.Hold || receipt.Hold

	if err := receipt.BeginPay(card.ID, card.System(), hold, now); err != nil {
		return nil, err
	}

	if err := s.merchant.CheckPerformTransaction(ctx, receipt.Amount, receipt.Account); err != nil {
		return nil, err
	}

	transactionID := s.tokens.TransactionID()
	if _, err := s.merchant.CreateTransaction(ctx, transactionID, now, receipt.Amount, receipt.Account); err != nil {
		return nil, err
	}

	if err := s.merchant.PerformTransaction(ctx, transactionID); err != nil {
		return nil, err
	}

	// The receipt keeps the transaction it drove, which is the only record
	// tying a card to a payment on the merchant's books.
	receipt.MerchantTxn = transactionID

	card.Charge(receipt.Amount)
	if err := s.cards.Update(ctx, card); err != nil {
		return nil, err
	}

	receipt.Payer = p.Payer
	if hold {
		receipt.HoldExpire = now + s.settings.HoldWindowMillis
	}

	if err := s.receipts.Update(ctx, receipt); err != nil {
		return nil, err
	}

	// The remaining states are reached by the background walk, not here.
	if err := s.scheduler.ScheduleAdvance(ctx, receipt.ReceiptID, s.settings.StepDelayMillis); err != nil {
		return nil, err
	}

	return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
}

// TransactionsCreate opens a payout: money the register hands to a saved card.
//
// Nothing was bought, so there is no order and the merchant is never asked.
//
// The card only has to be operable. It needs neither a balance, since money is
// arriving rather than leaving, nor a verification: a payout is made against a
// token minted for the paying-out register, and only the register that saves a
// card ever confirms it with an OTP.
func (s *Service) TransactionsCreate(ctx context.Context, p TransactionsCreateParams) (*ReceiptResult, error) {
	if p.Amount <= 0 {
		return nil, payerr.ErrInvalidAmount
	}

	card, err := s.loadCard(ctx, p.Token)
	if err != nil {
		return nil, err
	}

	if err := card.Operable(); err != nil {
		return nil, err
	}

	receipt := domain.NewPayout(
		s.settings.SandboxID, s.tokens.ReceiptID(), s.settings.MerchantID,
		p.Amount, p.Account, s.clock.NowMillis(),
	)
	receipt.CardID = &card.ID
	receipt.CardSystem = card.System()
	// A payout asks nothing of a merchant, but it is still a payment the
	// register made, so it is named the way a payment is: the id links the
	// receipt to the row the register's books hold for it.
	receipt.MerchantTxn = s.tokens.TransactionID()

	if err := s.receipts.Create(ctx, receipt); err != nil {
		return nil, err
	}

	// The register's books learn of the payout as it is opened, not only once it
	// settles: a payout asked for and never completed is the row an operator
	// goes looking for, and it would not exist if only settlements were written.
	if s.ledger != nil {
		if err := s.ledger.OpenPayout(ctx, payoutOf(receipt)); err != nil {
			return nil, err
		}
	}

	return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
}

// TransactionsComplete settles a payout and credits the card.
//
// A payout already settled is returned as it stands rather than paid twice:
// the caller cannot tell a lost response from a lost payout, so it retries,
// and a retry must not move money again.
func (s *Service) TransactionsComplete(ctx context.Context, p TransactionsCompleteParams) (*ReceiptResult, error) {
	receipt, err := s.loadReceipt(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	if !receipt.Payout {
		return nil, payerr.ErrCannotPerform
	}

	card, err := s.payoutCard(ctx, receipt, p.Token)
	if err != nil {
		return nil, err
	}

	if receipt.State == domain.StatePaid {
		return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
	}

	if err := receipt.Complete(p.Amount, s.clock.NowMillis()); err != nil {
		return nil, err
	}

	// The register pays before the card is credited: a payout larger than the
	// register holds is refused here, and refusing after the card had already
	// been credited would hand out money the register never had.
	if s.ledger != nil {
		if err := s.ledger.SettlePayout(ctx, payoutOf(receipt)); err != nil {
			return nil, err
		}
	}

	card.Receive(receipt.Amount)
	if err := s.cards.Update(ctx, card); err != nil {
		return nil, err
	}

	if err := s.receipts.Update(ctx, receipt); err != nil {
		return nil, err
	}

	return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
}

// AccountsGetBalance reports what the register holds, in tiyin.
//
// A payout register that runs dry refuses every withdrawal, and an integration
// that only learns this from a refused payout learns it one customer too late,
// so it watches the figure instead. The method is not in the provider's
// published documentation — like the payout pair, it is the half of the stand a
// real integration calls anyway.
func (s *Service) AccountsGetBalance(ctx context.Context, p AccountsGetBalanceParams) (*AccountsGetBalanceResult, error) {
	if s.ledger == nil {
		// A stand wired without a ledger has no register to speak for, which is
		// a stand that cannot answer rather than a register holding nothing.
		return nil, payerr.ErrCannotPerform
	}

	// Answering a call that names another merchant with this register's figure
	// would let one integration watch a balance it has no credentials for.
	if p.MerchantID != "" && p.MerchantID != s.settings.MerchantID {
		return nil, payerr.ErrUnauthorized
	}

	cashbox, err := s.ledger.Balance(ctx)
	if err != nil {
		return nil, err
	}

	return &AccountsGetBalanceResult{Balance: cashbox.Balance}, nil
}

// payoutOf describes a payout to the register's books. The receipt is the whole
// truth of one, so the ledger is told about the receipt rather than handed a
// bare amount it cannot attribute to anything.
func payoutOf(receipt *domain.Receipt) domain.Payout {
	return domain.Payout{
		TransactionID: receipt.MerchantTxn,
		Amount:        receipt.Amount,
		Account:       receipt.Account,
		CreateTime:    receipt.CreateTime,
		PayTime:       receipt.PayTime,
	}
}

// payoutCard returns the card a payout settles to.
//
// The token is optional, because the payout already names its card: a caller
// that repeats it is checked against the payout rather than trusted, and one
// that leaves it out is not refused for it.
func (s *Service) payoutCard(ctx context.Context, receipt *domain.Receipt, token string) (*domain.Card, error) {
	if receipt.CardID == nil {
		return nil, payerr.ErrCannotPerform
	}

	if token != "" {
		card, err := s.loadCard(ctx, token)
		if err != nil {
			return nil, err
		}
		// Another card would be a different payout, however well the amount
		// matches.
		if card.ID != *receipt.CardID {
			return nil, payerr.ErrCannotPerform
		}
		return card, nil
	}

	card, err := s.cards.ByID(ctx, *receipt.CardID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, payerr.ErrCannotPerform
		}
		return nil, err
	}

	return card, nil
}

// ReceiptsConfirmHold releases held funds and completes the payment.
func (s *Service) ReceiptsConfirmHold(ctx context.Context, p ReceiptsIDParams) (*ReceiptResult, error) {
	receipt, err := s.loadReceipt(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	if err := receipt.ConfirmHold(s.clock.NowMillis()); err != nil {
		return nil, err
	}

	if err := s.receipts.Update(ctx, receipt); err != nil {
		return nil, err
	}

	return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
}

// ReceiptsCancel queues a receipt for cancellation and refunds the card.
func (s *Service) ReceiptsCancel(ctx context.Context, p ReceiptsIDParams) (*ReceiptResult, error) {
	receipt, err := s.loadReceipt(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	alreadyCancelling := receipt.State == domain.StateCancelQueued || receipt.State == domain.StateCancelled

	if err := receipt.Cancel(s.clock.NowMillis()); err != nil {
		return nil, err
	}

	if !alreadyCancelling {
		if err := s.refund(ctx, receipt); err != nil {
			return nil, err
		}
		if err := s.receipts.Update(ctx, receipt); err != nil {
			return nil, err
		}
		if receipt.State == domain.StateCancelQueued {
			if err := s.scheduler.ScheduleAdvance(ctx, receipt.ReceiptID, s.settings.StepDelayMillis); err != nil {
				return nil, err
			}
		}
	}

	return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
}

// ReceiptsSend delivers a receipt to a phone for payment.
func (s *Service) ReceiptsSend(ctx context.Context, p ReceiptsSendParams) (*SuccessResult, error) {
	receipt, err := s.loadReceipt(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	if p.Phone == "" {
		return nil, payerr.ErrInvalidRequest
	}

	// A failed delivery is reported as an unsuccessful send, not as an RPC
	// error: the protocol's answer to this method is a success flag, and a
	// caller told the call itself failed would retry a receipt that exists.
	if err := s.sms.Send(ctx, p.Phone, receipt.ReceiptID); err != nil {
		return &SuccessResult{Success: false}, nil //nolint:nilerr // the flag is the answer
	}

	return &SuccessResult{Success: true}, nil
}

// ReceiptsCheck reports only a receipt's state.
func (s *Service) ReceiptsCheck(ctx context.Context, p ReceiptsIDParams) (*ReceiptsCheckResult, error) {
	receipt, err := s.loadReceipt(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return &ReceiptsCheckResult{State: receipt.State}, nil
}

// ReceiptsGet returns a receipt in full.
func (s *Service) ReceiptsGet(ctx context.Context, p ReceiptsIDParams) (*ReceiptResult, error) {
	receipt, err := s.loadReceipt(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return &ReceiptResult{Receipt: s.view(ctx, receipt)}, nil
}

// ReceiptsGetAll lists receipts for a period. The documented ceiling of fifty
// is enforced here as the provider enforces it.
func (s *Service) ReceiptsGetAll(ctx context.Context, p ReceiptsGetAllParams) ([]ReceiptView, error) {
	if p.Count > MaxGetAllCount {
		return nil, payerr.ErrInvalidRequest
	}

	count := p.Count
	if count <= 0 {
		count = MaxGetAllCount
	}

	found, err := s.receipts.List(ctx, p.From, p.To, count, p.Offset)
	if err != nil {
		return nil, err
	}

	views := make([]ReceiptView, 0, len(found))
	for _, r := range found {
		views = append(views, s.view(ctx, r))
	}
	return views, nil
}

// AdvanceReceipt performs one background step of the state walk. The worker
// calls it; it reschedules itself until the receipt settles.
func (s *Service) AdvanceReceipt(ctx context.Context, receiptID string) error {
	receipt, err := s.loadReceipt(ctx, receiptID)
	if err != nil {
		return err
	}

	if !receipt.Advance(s.clock.NowMillis()) {
		return nil
	}

	if err := s.receipts.Update(ctx, receipt); err != nil {
		return err
	}

	if receipt.State.Terminal() || receipt.State == domain.StateHeld {
		return nil
	}

	return s.scheduler.ScheduleAdvance(ctx, receipt.ReceiptID, s.settings.StepDelayMillis)
}

// ---------- helpers ----------

// stall holds a request for as long as the card says before it is answered. A
// card that takes ten seconds and then fails is what an integration's timeout
// is written against, and nothing else on the stand can reproduce it per card.
func (s *Service) stall(ctx context.Context, card *domain.Card) {
	if card.DelayMillis <= 0 {
		return
	}

	s.clock.Sleep(ctx, time.Duration(card.DelayMillis)*time.Millisecond)
}

func (s *Service) loadCard(ctx context.Context, token string) (*domain.Card, error) {
	if token == "" {
		return nil, payerr.ErrInvalidRequest
	}

	card, err := s.cards.ByToken(ctx, token)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, payerr.ErrCannotPerform
		}
		return nil, err
	}

	// The stall belongs here rather than in each method: a slow card is slow
	// whatever is asked of it, and the delay must land before the outcome so
	// the pair reads as one behaviour — takes ten seconds, then fails.
	s.stall(ctx, card)

	return card, nil
}

func (s *Service) loadReceipt(ctx context.Context, receiptID string) (*domain.Receipt, error) {
	if receiptID == "" {
		return nil, payerr.ErrInvalidRequest
	}

	receipt, err := s.receipts.ByReceiptID(ctx, receiptID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, payerr.ErrTransactionNotFound
		}
		return nil, err
	}
	return receipt, nil
}

// refund returns money to the card when a charged receipt is cancelled. A
// receipt that never reached the card has nothing to give back.
func (s *Service) refund(ctx context.Context, r *domain.Receipt) error {
	if r.CardID == nil || r.PayTime == 0 {
		return nil
	}

	card, err := s.cards.ByID(ctx, *r.CardID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil // a card that has since vanished leaves nothing to refund
		}
		return err
	}

	card.Refund(r.Amount)
	return s.cards.Update(ctx, card)
}

// noOperation is the `operation` a receipt of the Subscribe API reports. The
// documented responses carry -1 throughout; nothing in the protocol asks a
// stand to compute another value.
const noOperation = -1

// view renders a receipt for the wire.
//
// The card is read here rather than carried on the receipt: a paid receipt
// reports the card it was paid with, and a receipt still waiting reports null.
func (s *Service) view(ctx context.Context, r *domain.Receipt) ReceiptView {
	return ReceiptView{
		ID:          r.ReceiptID,
		CreateTime:  r.CreateTime,
		PayTime:     r.PayTime,
		CancelTime:  r.CancelTime,
		State:       r.State,
		Type:        r.Type,
		Operation:   noOperation,
		Description: r.Description,
		Detail:      r.Detail,
		Amount:      r.Amount,
		Currency:    r.Currency,
		Commission:  r.Commission,
		Account:     accountView(r.Account),
		Card:        s.receiptCard(ctx, r),
		Merchant: MerchantView{
			ID: r.MerchantID, Name: s.settings.MerchantName,
			Organization: s.settings.MerchantName,
		},
		Meta: map[string]any{"source": "subscribe", "owner": r.MerchantID},
	}
}

// receiptCard renders the card a receipt was paid with, or nil when it has not
// been paid. A card that has since been removed leaves the receipt reporting
// null rather than failing a call that only reads it.
func (s *Service) receiptCard(ctx context.Context, r *domain.Receipt) *ReceiptCardView {
	if r.CardID == nil {
		return nil
	}

	card, err := s.cards.ByID(ctx, *r.CardID)
	if err != nil {
		return nil
	}

	return &ReceiptCardView{Number: card.NumberMask(), Expire: domain.PlainExpire(card.Expire)}
}

// accountTitles are the labels the provider shows for the account fields its
// documentation names. A field nobody has a label for is titled by its own
// name, in all three languages, rather than left blank: a payer reading a
// receipt is better served by "loan_id" than by nothing at all.
var accountTitles = map[string]payerr.Message{
	"order_id": payerr.NewMessage("Код заказа", "Buyurtma kodi", "Order code"),
	"phone":    payerr.NewMessage("Телефон", "Telefon", "Phone"),
	"login":    payerr.NewMessage("Логин", "Login", "Login"),
	"account":  payerr.NewMessage("Лицевой счёт", "Shaxsiy hisob", "Account"),
	"client_id": payerr.NewMessage("Идентификатор клиента", "Mijoz identifikatori",
		"Client id"),
}

// accountTitle returns the label a field is shown under.
func accountTitle(name string) payerr.Message {
	if title, ok := accountTitles[name]; ok {
		return title
	}

	return payerr.NewMessage(name, name, name)
}

// accountView turns the account object a receipt was created with into the
// list of labelled fields the provider answers with. The order is fixed by the
// field name so a response never varies between two identical receipts, and
// the first field is the main one, which is what a client renders largest.
func accountView(account map[string]string) []AccountFieldView {
	names := make([]string, 0, len(account))
	for name := range account {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]AccountFieldView, 0, len(names))
	for i, name := range names {
		out = append(out, AccountFieldView{
			Name:  name,
			Title: accountTitle(name),
			Value: account[name],
			Main:  i == 0,
		})
	}

	return out
}

// maskPhone hides the middle of a phone number, as the provider does when it
// reports where a code was sent.
func maskPhone(phone string) string {
	if len(phone) < 9 {
		return phone
	}
	return phone[:5] + "*****" + phone[len(phone)-2:]
}
