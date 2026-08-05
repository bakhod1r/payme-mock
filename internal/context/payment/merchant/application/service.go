package application

import (
	"context"
	"errors"
	"strconv"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/clock"
	payerr "github.com/bakhod1r/payme-mock/internal/kernel/errors"
)

// orderIDField is the account key that names an order rather than a property
// of the payer. It is the default, and the only key whose value is the order
// itself, so the order it names is the one that settles.
const orderIDField = "order_id"

// Settings are the tunables the console exposes. They arrive per request so a
// configuration profile switch takes effect without a restart.
type Settings struct {
	// TransactionTimeoutMillis is how long a created transaction may wait for
	// confirmation before PerformTransaction refuses it.
	TransactionTimeoutMillis int64
	// AccountField is the account object key this merchant identifies payers by.
	AccountField string
	// RegisterKind is the direction this stand's cash register moves money in:
	// topup takes from the payer, dividend and deposit hand money back.
	RegisterKind string
	// AutoRegisterAccounts accepts a payer this merchant has never seen,
	// registering them and their order as the payment arrives.
	AutoRegisterAccounts bool
}

// Service implements the six Merchant API methods.
type Service struct {
	transactions domain.TransactionRepository
	events       domain.EventRecorder
	accounts     billing.AccountRepository
	orders       billing.OrderRepository
	walkIns      billing.WalkInRepository
	clock        clock.Clock
	settings     Settings
}

// NewService wires the use cases to their ports.
func NewService(
	transactions domain.TransactionRepository,
	events domain.EventRecorder,
	accounts billing.AccountRepository,
	orders billing.OrderRepository,
	walkIns billing.WalkInRepository,
	clk clock.Clock,
	settings Settings,
) *Service {
	return &Service{
		transactions: transactions,
		events:       events,
		accounts:     accounts,
		orders:       orders,
		walkIns:      walkIns,
		clock:        clk,
		settings:     settings,
	}
}

// CheckPerformTransaction reports whether a payment may be created.
func (s *Service) CheckPerformTransaction(ctx context.Context, p CheckPerformParams) (*CheckPerformResult, error) {
	order, err := s.resolveOrder(ctx, p.Account, p.Amount)
	if err != nil {
		return nil, err
	}

	if err := order.CheckAmount(p.Amount); err != nil {
		return nil, err
	}

	// This is the call whose whole job is to say whether the payment may go
	// ahead, so a stopped register is reported here rather than after the payer
	// has been taken through a payment that was never going to settle.
	if err := s.registerUsable(ctx); err != nil {
		return nil, err
	}

	return &CheckPerformResult{Allow: true}, nil
}

// CreateTransaction creates a transaction, or replays the stored response when
// Payme repeats a request it has already sent.
func (s *Service) CreateTransaction(ctx context.Context, p CreateParams) (*CreateResult, error) {
	existing, err := s.transactions.ByPaymeID(ctx, p.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	if existing != nil {
		// A repeat of a request already answered. The protocol requires the
		// original response, so the state is reported as it stands rather than
		// recomputed; a transaction no longer in the created state cannot be
		// re-created.
		if existing.State != domain.StateCreated {
			s.record(ctx, existing, "CreateTransaction", nil, true, payerr.CodeCannotPerform)
			return nil, payerr.ErrCannotPerform
		}
		s.record(ctx, existing, "CreateTransaction", nil, true, 0)
		return createResultOf(existing), nil
	}

	order, err := s.resolveOrder(ctx, p.Account, p.Amount)
	if err != nil {
		return nil, err
	}

	if err := order.CheckAmount(p.Amount); err != nil {
		return nil, err
	}

	// A stopped register creates nothing. Refusing only when the money moves
	// would leave a created transaction behind that can never be performed.
	if err := s.registerUsable(ctx); err != nil {
		return nil, err
	}

	// One order may hold only one active transaction at a time.
	active, err := s.transactions.ActiveByOrder(ctx, order.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if active != nil {
		return nil, payerr.ErrCannotPerform
	}

	tx := domain.NewTransaction(
		order.SandboxID, p.ID, order.ID, order.AccountID,
		p.Account, p.Amount, p.Time, s.clock.NowMillis(),
	)

	if err := s.transactions.Create(ctx, tx); err != nil {
		if errors.Is(err, domain.ErrDuplicate) {
			// A concurrent delivery of the same request won the race. The
			// protocol wants the original response, so it is loaded rather
			// than reported as a failure.
			return s.replayCreate(ctx, p.ID)
		}
		return nil, err
	}

	order.MarkProcessing()
	if err := s.orders.Update(ctx, order); err != nil {
		return nil, err
	}

	created := domain.StateCreated
	s.recordEvent(ctx, tx.ID, "CreateTransaction", nil, &created, false, 0)

	return createResultOf(tx), nil
}

// PerformTransaction confirms a transaction and credits the merchant.
func (s *Service) PerformTransaction(ctx context.Context, p PerformParams) (*PerformResult, error) {
	tx, err := s.load(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	before := tx.State
	replay := tx.State == domain.StatePerformed

	if err := tx.Perform(s.clock.NowMillis(), s.settings.TransactionTimeoutMillis); err != nil {
		// An expired transaction was auto-cancelled inside Perform; that
		// change has to reach storage even though the call failed.
		if tx.State != before {
			if updateErr := s.transactions.Update(ctx, tx); updateErr != nil {
				return nil, updateErr
			}
			s.recordEvent(ctx, tx.ID, "PerformTransaction", &before, &tx.State, false, payerr.CodeCannotPerform)
		}
		return nil, err
	}

	if !replay {
		// The balance moves before anything is stored: a payout the register
		// cannot cover has to fail the whole call, leaving the transaction
		// created rather than performed against money that is not there.
		if err := s.settle(ctx, tx.AccountID, tx.Amount); err != nil {
			s.recordEvent(ctx, tx.ID, "PerformTransaction", &before, nil, false, payerr.CodeCannotPerform)
			return nil, err
		}

		if err := s.transactions.Update(ctx, tx); err != nil {
			return nil, err
		}
		if err := s.markOrderPaid(ctx, tx.OrderID); err != nil {
			return nil, err
		}
	}

	s.recordEvent(ctx, tx.ID, "PerformTransaction", &before, &tx.State, replay, 0)

	return &PerformResult{
		Transaction: transactionNumber(tx),
		PerformTime: tx.PerformTime,
		State:       tx.State,
	}, nil
}

// CancelTransaction cancels a transaction, before or after it was performed.
func (s *Service) CancelTransaction(ctx context.Context, p CancelParams) (*CancelResult, error) {
	tx, err := s.load(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	before := tx.State
	replay := before.IsFinal()

	if err := tx.Cancel(p.Reason, s.clock.NowMillis()); err != nil {
		return nil, err
	}

	if !replay {
		// Only a payment that was performed moved the balance, so only that one
		// has anything to put back.
		if before == domain.StatePerformed {
			if err := s.unsettle(ctx, tx.AccountID, tx.Amount); err != nil {
				return nil, err
			}
		}

		if err := s.transactions.Update(ctx, tx); err != nil {
			return nil, err
		}
		if err := s.releaseOrder(ctx, tx.OrderID); err != nil {
			return nil, err
		}
	}

	s.recordEvent(ctx, tx.ID, "CancelTransaction", &before, &tx.State, replay, 0)

	return &CancelResult{
		Transaction: transactionNumber(tx),
		CancelTime:  tx.CancelTime,
		State:       tx.State,
	}, nil
}

// CheckTransaction reports a transaction's current state.
func (s *Service) CheckTransaction(ctx context.Context, p CheckParams) (*CheckResult, error) {
	tx, err := s.load(ctx, p.ID)
	if err != nil {
		return nil, err
	}

	return &CheckResult{
		CreateTime:  tx.CreateTime,
		PerformTime: tx.PerformTime,
		CancelTime:  tx.CancelTime,
		Transaction: transactionNumber(tx),
		State:       tx.State,
		Reason:      tx.Reason,
	}, nil
}

// GetStatement lists transactions created in a period, for reconciliation.
func (s *Service) GetStatement(ctx context.Context, p StatementParams) (*StatementResult, error) {
	found, err := s.transactions.Statement(ctx, p.From, p.To)
	if err != nil {
		return nil, err
	}

	// An empty period returns an empty array, never null.
	entries := make([]StatementEntry, 0, len(found))
	for _, tx := range found {
		entries = append(entries, StatementEntry{
			ID:          tx.PaymeID,
			Time:        tx.PaymeTime,
			Amount:      tx.Amount,
			Account:     tx.Account,
			CreateTime:  tx.CreateTime,
			PerformTime: tx.PerformTime,
			CancelTime:  tx.CancelTime,
			Transaction: transactionNumber(tx),
			State:       tx.State,
			Reason:      tx.Reason,
			Receivers:   tx.Receivers,
		})
	}

	return &StatementResult{Transactions: entries}, nil
}

// replayCreate returns the stored response for a transaction that another
// concurrent delivery created first.
func (s *Service) replayCreate(ctx context.Context, paymeID string) (*CreateResult, error) {
	existing, err := s.transactions.ByPaymeID(ctx, paymeID)
	if err != nil {
		return nil, err
	}

	if existing.State != domain.StateCreated {
		s.record(ctx, existing, "CreateTransaction", nil, true, payerr.CodeCannotPerform)
		return nil, payerr.ErrCannotPerform
	}

	s.record(ctx, existing, "CreateTransaction", nil, true, 0)
	return createResultOf(existing), nil
}

// load fetches a transaction, reporting the documented -31003 when it is absent.
func (s *Service) load(ctx context.Context, paymeID string) (*domain.Transaction, error) {
	tx, err := s.transactions.ByPaymeID(ctx, paymeID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, payerr.ErrTransactionNotFound
		}
		return nil, err
	}
	return tx, nil
}

// resolveOrder turns the `account` object into the order being paid. A missing
// or unknown field is an account error naming the field, as the protocol requires.
func (s *Service) resolveOrder(ctx context.Context, account map[string]string, amount int64) (*billing.Order, error) {
	field := s.settings.AccountField

	value, ok := account[field]
	if !ok || value == "" {
		return nil, payerr.ErrAccountNotFound.WithData(field)
	}

	acc, err := s.accounts.ByField(ctx, field, value)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// A stand that accepts walk-in payers registers this one instead
			// of refusing the payment.
			if s.settings.AutoRegisterAccounts {
				return s.registerWalkIn(ctx, field, value, amount)
			}
			return nil, payerr.ErrAccountNotFound.WithData(field)
		}
		return nil, err
	}

	// When the account names an order outright, that order is the one being
	// paid. Falling through to "the payer's first payable order" would settle
	// a different order from the one the payment named, which is how a payment
	// for one order silently closes another.
	if field == orderIDField {
		return s.namedOrder(ctx, field, value, acc.ID, amount)
	}

	orders, err := s.orders.ByAccount(ctx, acc.ID)
	if err != nil {
		return nil, err
	}

	for _, o := range orders {
		if o.Payable() {
			return o, nil
		}
	}

	if s.settings.AutoRegisterAccounts {
		// The payer is known but has nothing left to pay. Registering another
		// order keeps a repeated payment working, which is what a register
		// that identifies payers per payment expects.
		return s.registerWalkIn(ctx, field, value, amount)
	}

	return nil, payerr.ErrAccountNotFound.WithData(field)
}

// namedOrder returns the order the account object named, once it is sure the
// order is that payer's and can still be paid.
//
// The payer was resolved through this same value, so a mismatch means the two
// disagree — the order moved to another payer between the lookups — and the
// payment is refused rather than settled against whoever holds it now.
func (s *Service) namedOrder(ctx context.Context, field, value string, accountID, amount int64) (*billing.Order, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, payerr.ErrAccountNotFound.WithData(field)
	}

	order, err := s.orders.ByID(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, payerr.ErrAccountNotFound.WithData(field)
		}
		return nil, err
	}

	if order.AccountID != accountID {
		return nil, payerr.ErrAccountNotFound.WithData(field)
	}

	if !order.Payable() {
		// A stand that accepts walk-in payers writes another order rather than
		// refusing a repeated payment, which is what a register that generates
		// an order per payment expects.
		if s.settings.AutoRegisterAccounts {
			return s.registerWalkIn(ctx, field, value, amount)
		}
		return nil, payerr.ErrAccountNotFound.WithData(field)
	}

	return order, nil
}

// registerWalkIn creates the payer and the order a payment names, so a stand
// can be paid without anything being set up in advance.
func (s *Service) registerWalkIn(ctx context.Context, field, value string, amount int64) (*billing.Order, error) {
	// An amount the order could not carry is the payment's error, not a reason
	// to write a row the schema rejects.
	if amount <= 0 {
		return nil, payerr.ErrInvalidAmount
	}

	order, err := s.walkIns.Register(ctx, value, amount)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, payerr.ErrAccountNotFound.WithData(field)
		}
		return nil, err
	}

	return order, nil
}

// settle moves the register's balance for a performed payment.
//
// Which way it moves is the register's business: a topup register takes money
// from the payer, a dividend or deposit register hands it back.
func (s *Service) settle(ctx context.Context, _, amount int64) error {
	account, kind, err := s.registerAccount(ctx)
	if err != nil {
		return err
	}

	if err := account.Apply(kind, amount); err != nil {
		return err
	}

	return s.accounts.UpdateBalance(ctx, account.ID, account.Balance)
}

// unsettle puts back what settle moved, which is what a cancellation after a
// performed payment has to do.
func (s *Service) unsettle(ctx context.Context, _, amount int64) error {
	account, kind, err := s.registerAccount(ctx)
	if err != nil {
		return err
	}

	account.Reverse(kind, amount)

	return s.accounts.UpdateBalance(ctx, account.ID, account.Balance)
}

// registerAccount loads the stand's own balance together with the direction the
// register moves money in.
//
// It is deliberately not the payer the payment named. A stand that registers a
// payer for every unknown order would otherwise spread its takings over a row
// per payment, while the figure the console shows — the register's own — never
// moved at all.
func (s *Service) registerAccount(ctx context.Context) (*billing.Account, billing.Kind, error) {
	account, err := s.accounts.Register(ctx)
	if err != nil {
		return nil, "", err
	}

	kind := billing.Kind(s.settings.RegisterKind)
	if !kind.Valid() {
		// A stand that never said what kind of register it is takes money in,
		// which is what an integration starts from.
		kind = billing.KindTopup
	}

	return account, kind, nil
}

// registerUsable reports whether this register may take part in a payment.
//
// The block is on the register's own payer, which is the account every payment
// through it settles against, so one lookup answers it for all of them.
func (s *Service) registerUsable(ctx context.Context) error {
	account, _, err := s.registerAccount(ctx)
	if err != nil {
		// A register with no payer of its own is a stand that was never set up,
		// which the payment paths already report where they need it. It is not
		// a block.
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}

	return account.Usable()
}

func (s *Service) markOrderPaid(ctx context.Context, orderID int64) error {
	return s.updateOrder(ctx, orderID, (*billing.Order).MarkPaid)
}

func (s *Service) releaseOrder(ctx context.Context, orderID int64) error {
	return s.updateOrder(ctx, orderID, (*billing.Order).MarkCancelled)
}

func (s *Service) updateOrder(ctx context.Context, orderID int64, apply func(*billing.Order)) error {
	order, err := s.orders.ByID(ctx, orderID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil // a transaction without an order is nothing to update
		}
		return err
	}
	apply(order)
	return s.orders.Update(ctx, order)
}

// record writes an audit entry for a transaction whose state did not change.
func (s *Service) record(ctx context.Context, tx *domain.Transaction, method string, to *domain.State, replay bool, code payerr.Code) {
	s.recordEvent(ctx, tx.ID, method, &tx.State, to, replay, code)
}

// recordEvent writes an audit entry. Audit failures never fail the request:
// losing a log line must not change what the payment system reports.
func (s *Service) recordEvent(ctx context.Context, txID int64, method string,
	from, to *domain.State, replay bool, code payerr.Code,
) {
	e := domain.Event{
		TransactionID: txID,
		Method:        method,
		FromState:     from,
		ToState:       to,
		IdempotentHit: replay,
	}
	if code != 0 {
		c := int(code)
		e.ErrorCode = &c
	}
	_ = s.events.Record(ctx, e)
}

func createResultOf(tx *domain.Transaction) *CreateResult {
	return &CreateResult{
		CreateTime:  tx.CreateTime,
		Transaction: transactionNumber(tx),
		State:       tx.State,
		Receivers:   tx.Receivers,
	}
}

// transactionNumber is the merchant's own identifier for the transaction,
// which the protocol carries as a string.
func transactionNumber(tx *domain.Transaction) string {
	return strconv.FormatInt(tx.ID, 10)
}
