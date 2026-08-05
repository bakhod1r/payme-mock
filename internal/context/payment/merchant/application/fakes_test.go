package application_test

import (
	"context"
	"errors"
	"fmt"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
)

// errBoom stands in for any infrastructure failure a port can raise.
var errBoom = errors.New("storage is down")

// fakeTransactions is an in-memory TransactionRepository. Each Fail* field
// makes the corresponding method return errBoom, so error branches are
// reachable without a database.
type fakeTransactions struct {
	byPaymeID map[string]*domain.Transaction
	active    map[int64]*domain.Transaction
	statement []*domain.Transaction
	nextID    int64

	failByPaymeID     bool
	failCreate        bool
	failUpdate        bool
	failStatement     bool
	failActiveByOrder bool

	// duplicateOnCreate makes Create report that a concurrent delivery of the
	// same request already inserted the row, which is what the unique index
	// raises under a real race.
	duplicateOnCreate *domain.Transaction
	// failByPaymeIDAfterCreate breaks the read-back that follows a lost race.
	failByPaymeIDAfterCreate bool
	created                  bool
}

func newFakeTransactions() *fakeTransactions {
	return &fakeTransactions{
		byPaymeID: make(map[string]*domain.Transaction),
		active:    make(map[int64]*domain.Transaction),
		nextID:    5123, // the identifier the documentation's examples use
	}
}

func (f *fakeTransactions) ByPaymeID(_ context.Context, paymeID string) (*domain.Transaction, error) {
	if f.failByPaymeID || (f.created && f.failByPaymeIDAfterCreate) {
		return nil, errBoom
	}
	tx, ok := f.byPaymeID[paymeID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return tx, nil
}

func (f *fakeTransactions) Create(_ context.Context, tx *domain.Transaction) error {
	if f.failCreate {
		return errBoom
	}
	f.created = true
	if winner := f.duplicateOnCreate; winner != nil {
		// The racing delivery's row is now visible to the loser.
		f.byPaymeID[winner.PaymeID] = winner
		f.duplicateOnCreate = nil
		return domain.ErrDuplicate
	}
	tx.ID = f.nextID
	f.nextID++
	f.byPaymeID[tx.PaymeID] = tx
	f.active[tx.OrderID] = tx
	return nil
}

func (f *fakeTransactions) Update(_ context.Context, tx *domain.Transaction) error {
	if f.failUpdate {
		return errBoom
	}
	f.byPaymeID[tx.PaymeID] = tx
	if tx.State.IsActive() {
		f.active[tx.OrderID] = tx
	} else {
		delete(f.active, tx.OrderID)
	}
	return nil
}

func (f *fakeTransactions) Statement(_ context.Context, from, to int64) ([]*domain.Transaction, error) {
	if f.failStatement {
		return nil, errBoom
	}
	var out []*domain.Transaction
	for _, tx := range f.statement {
		if tx.CreateTime >= from && tx.CreateTime <= to {
			out = append(out, tx)
		}
	}
	return out, nil
}

func (f *fakeTransactions) ActiveByOrder(_ context.Context, orderID int64) (*domain.Transaction, error) {
	if f.failActiveByOrder {
		return nil, errBoom
	}
	tx, ok := f.active[orderID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return tx, nil
}

// fakeEvents records the audit trail and can be made to fail, proving audit
// failures do not change what the caller sees.
type fakeEvents struct {
	recorded []domain.Event
	fail     bool
}

func (f *fakeEvents) Record(_ context.Context, e domain.Event) error {
	if f.fail {
		return errBoom
	}
	f.recorded = append(f.recorded, e)
	return nil
}

func (f *fakeEvents) last() domain.Event { return f.recorded[len(f.recorded)-1] }

// fakeAccounts resolves account fields to payers and records balance moves.
type fakeAccounts struct {
	byField map[string]*billing.Account
	byID    map[int64]*billing.Account
	fail    bool
	// failByID and failUpdate break the balance move, which is what a payment
	// settling against a lost database looks like.
	failByID   bool
	failUpdate bool
	// balances holds what was stored, so a test can assert the move without
	// reaching into the aggregate.
	balances map[int64]int64
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{
		byField:  make(map[string]*billing.Account),
		byID:     make(map[int64]*billing.Account),
		balances: make(map[int64]int64),
	}
}

// add registers a payer under both lookups, since the service resolves them by
// field and then loads them by identifier to move the balance.
func (f *fakeAccounts) add(field, value string, acc *billing.Account) {
	f.byField[field+"="+value] = acc
	f.byID[acc.ID] = acc
}

func (f *fakeAccounts) ByField(_ context.Context, field, value string) (*billing.Account, error) {
	if f.fail {
		return nil, errBoom
	}
	acc, ok := f.byField[field+"="+value]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return acc, nil
}

func (f *fakeAccounts) ByID(_ context.Context, id int64) (*billing.Account, error) {
	if f.failByID {
		return nil, errBoom
	}
	acc, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return acc, nil
}

// Register is the stand's own payer: the lowest identifier, which is the row a
// sandbox is created with.
func (f *fakeAccounts) Register(_ context.Context) (*billing.Account, error) {
	if f.failByID {
		return nil, errBoom
	}

	var out *billing.Account
	for _, acc := range f.byID {
		if out == nil || acc.ID < out.ID {
			out = acc
		}
	}
	if out == nil {
		return nil, domain.ErrNotFound
	}

	return out, nil
}

func (f *fakeAccounts) UpdateBalance(_ context.Context, id, balance int64) error {
	if f.failUpdate {
		return errBoom
	}
	f.balances[id] = balance
	return nil
}

// fakeOrders is an in-memory OrderRepository.
type fakeOrders struct {
	byID        map[int64]*billing.Order
	byAccount   map[int64][]*billing.Order
	failByID    bool
	failByAcct  bool
	failUpdate  bool
	missingByID bool
	updates     int
}

func newFakeOrders() *fakeOrders {
	return &fakeOrders{
		byID:      make(map[int64]*billing.Order),
		byAccount: make(map[int64][]*billing.Order),
	}
}

func (f *fakeOrders) ByID(_ context.Context, id int64) (*billing.Order, error) {
	if f.failByID {
		return nil, errBoom
	}
	if f.missingByID {
		return nil, domain.ErrNotFound
	}
	o, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return o, nil
}

func (f *fakeOrders) ByAccount(_ context.Context, accountID int64) ([]*billing.Order, error) {
	if f.failByAcct {
		return nil, errBoom
	}
	return f.byAccount[accountID], nil
}

func (f *fakeOrders) Update(_ context.Context, o *billing.Order) error {
	if f.failUpdate {
		return errBoom
	}
	f.updates++
	f.byID[o.ID] = o
	return nil
}

// fakeWalkIns is an in-memory WalkInRepository. It hands back the same order
// for the same value and amount, which is what the real one guarantees.
type fakeWalkIns struct {
	orders map[string]*billing.Order
	calls  int
	nextID int64
	fail   bool
	absent bool
}

func newFakeWalkIns() *fakeWalkIns {
	return &fakeWalkIns{orders: make(map[string]*billing.Order), nextID: 900}
}

func (f *fakeWalkIns) Register(_ context.Context, value string, amount int64) (*billing.Order, error) {
	f.calls++

	if f.fail {
		return nil, errBoom
	}
	if f.absent {
		return nil, domain.ErrNotFound
	}

	key := fmt.Sprintf("%s:%d", value, amount)
	if existing, ok := f.orders[key]; ok {
		return existing, nil
	}

	f.nextID++
	order := &billing.Order{
		ID: f.nextID, SandboxID: 1, AccountID: 77,
		Amount: amount, Status: billing.StatusNew,
	}
	f.orders[key] = order

	return order, nil
}
