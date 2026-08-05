package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	sandboxdomain "github.com/bakhod1r/payme-mock/internal/context/simulation/sandbox/domain"
)

// A database that went away is the one failure every screen shares, and the one
// that must never be read as an answer: an empty list where the rows are
// unreachable says "there is nothing here", which is the opposite of the truth.
//
// Each of these is the same shape — close the pool, ask, and require that the
// store says so — so they are written as one table rather than fifty tests that
// differ only in the call.
func TestE2EEveryReadReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()

	// The rows are read before the database goes, so what is under test is the
	// reading rather than an identifier nothing could match.
	sandbox := s.newSandbox(t, "lost", "topup", 100000)
	card := s.riggedCard(t, sandbox, uzcard, "success")

	s.store.pool.Close()

	reads := map[string]func() error{
		"cards": func() error {
			_, err := s.store.Cards(ctx, tabMocks, cardFilter{}, pageRequest{Limit: 10})
			return err
		},
		"card counts": func() error {
			_, _, err := s.store.CardCounts(ctx)
			return err
		},
		"one card": func() error {
			_, err := s.store.CardByID(ctx, card)
			return err
		},
		"a number already held": func() error {
			_, err := s.store.CardExists(ctx, uzcard)
			return err
		},
		"the cashboxes of a card": func() error {
			_, err := s.store.CardCashboxes(ctx, card)
			return err
		},
		"what a card has been through": func() error {
			_, err := s.store.CardActivity(ctx, card)
			return err
		},
		"the receipts of a card": func() error {
			_, err := s.store.ReceiptsForCard(ctx, card, 10)
			return err
		},
		"stands": func() error {
			_, err := s.store.Sandboxes(ctx, "https://example")
			return err
		},
		"a search over stands": func() error {
			_, err := s.store.searchSandboxes(ctx, "https://example", "lost")
			return err
		},
		"one stand": func() error {
			_, err := s.store.SandboxByID(ctx, sandbox.ID, "https://example")
			return err
		},
		"profiles": func() error {
			_, err := s.store.Profiles(ctx)
			return err
		},
		"payers": func() error {
			_, err := s.store.Accounts(ctx)
			return err
		},
		"one payer": func() error {
			_, err := s.store.AccountByID(ctx, sandbox.AccountID)
			return err
		},
		"the payer of a stand": func() error {
			_, err := s.store.SandboxAccountID(ctx, sandbox.ID)
			return err
		},
		"balance history": func() error {
			_, err := s.store.BalanceHistory(ctx, sandbox.ID, 10)
			return err
		},
		"orders": func() error {
			_, err := s.store.Orders(ctx, "", 10)
			return err
		},
		"the orders of a stand": func() error {
			_, err := s.store.OrdersForSandbox(ctx, sandbox.ID, 10)
			return err
		},
		"one order": func() error {
			_, err := s.store.OrderByID(ctx, 1)
			return err
		},
		"payments": func() error {
			_, err := s.store.Transactions(ctx, transactionFilter{}, pageRequest{Limit: 10})
			return err
		},
		"one payment": func() error {
			_, err := s.store.TransactionByID(ctx, 1)
			return err
		},
		"the payments screen": func() error {
			_, err := s.store.Payments(ctx, paymentFilter{}, pageRequest{Limit: 10})
			return err
		},
		"one receipt": func() error {
			_, err := s.store.ReceiptByID(ctx, 1)
			return err
		},
		"rules": func() error {
			_, err := s.store.Rules(ctx, "")
			return err
		},
		"one rule": func() error {
			_, err := s.store.RuleByID(ctx, 1)
			return err
		},
		"the error catalog": func() error {
			_, err := s.store.Errors(ctx)
			return err
		},
		"traffic": func() error {
			_, err := s.store.Traffic(ctx, 10)
			return err
		},
		"traffic in detail": func() error {
			_, err := s.store.TrafficDetails(ctx, trafficFilter{}, pageRequest{Limit: 10})
			return err
		},
		"one logged call": func() error {
			_, err := s.store.TrafficByID(ctx, 1)
			return err
		},
		"the address rules of a stand": func() error {
			_, err := s.store.IPRules(ctx, sandbox.ID)
			return err
		},
		"live payments": func() error {
			_, err := s.store.LiveTransactions(ctx, "", 60, 10)
			return err
		},
		"live cards": func() error {
			_, err := s.store.LiveCards(ctx, "", 60, 10)
			return err
		},
		"the dashboard": func() error {
			_, err := s.store.Dashboard(ctx)
			return err
		},
	}

	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, read(), "a database that went away is not an empty screen")
		})
	}
}

// The same for everything that writes. A write that could not happen must be
// reported: the operator is about to act on the state they think they made.
func TestE2EEveryWriteReportsALostDatabase(t *testing.T) {
	s := newStand(t)
	ctx := context.Background()

	sandbox := s.newSandbox(t, "lost-write", "topup", 100000)
	card := s.riggedCard(t, sandbox, uzcard, "success")

	s.store.pool.Close()

	writes := map[string]func() error{
		"a new stand": func() error {
			return s.store.CreateSandbox(ctx, &sandboxdomain.Sandbox{
				Slug: "gone", Name: "gone", MerchantID: "m", Key: "k", TestKey: "tk", Kind: "topup",
			}, nil, 0)
		},
		"editing a stand": func() error {
			return s.store.UpdateSandbox(ctx, sandbox.ID, "n", nil, "topup", "", "")
		},
		"resetting a stand": func() error { return s.store.ResetSandbox(ctx, sandbox.ID) },
		"deleting a stand":  func() error { return s.store.DeleteSandbox(ctx, sandbox.ID) },
		"editing a payer": func() error {
			return s.store.UpdateAccount(ctx, sandbox.AccountID, "n", "p")
		},
		"deleting a payer": func() error { return s.store.DeleteAccount(ctx, sandbox.AccountID) },
		"moving a balance": func() error {
			_, err := s.store.ChangeBalance(ctx, sandbox.AccountID, "add", 1)
			return err
		},
		"stopping a payer": func() error { return s.store.SetBlocked(ctx, sandbox.AccountID, true) },
		"a new order": func() error {
			return s.store.CreateOrder(ctx, sandbox.AccountID, 1000, "")
		},
		"deleting an order":  func() error { return s.store.DeleteOrder(ctx, 1) },
		"moving a payment":   func() error { return s.store.UpdateTransactionState(ctx, 1, 2) },
		"deleting a payment": func() error { return s.store.DeleteTransaction(ctx, 1) },
		"a new card": func() error {
			_, err := s.store.CreateCard(ctx, newCard{
				SandboxID: sandbox.ID, Number: humo, Expire: "0399", Outcome: "success",
			})
			return err
		},
		"editing a card": func() error {
			return s.store.UpdateCard(ctx, card, newCard{Outcome: "blocked"})
		},
		"stopping a card":  func() error { return s.store.SetCardBlocked(ctx, card, true) },
		"deleting a card":  func() error { return s.store.DeleteCard(ctx, card) },
		"a card's cashbox": func() error { return s.store.SetCardCashbox(ctx, card, sandbox.ID, true) },
		"the provider's test cards": func() error {
			_, err := s.store.SeedPaymeCards(ctx, sandbox.ID)
			return err
		},
		"a new rule": func() error {
			_, err := s.store.CreateRule(ctx, newRule{Service: "merchant", Method: "*", Action: "delay", Probability: 1})
			return err
		},
		"editing a rule": func() error {
			return s.store.UpdateRule(ctx, 1, newRule{Service: "merchant", Method: "*", Action: "delay", Probability: 1})
		},
		"switching a rule":       func() error { return s.store.ToggleRule(ctx, 1) },
		"deleting a rule":        func() error { return s.store.DeleteRule(ctx, 1) },
		"a new address rule":     func() error { return s.store.CreateIPRule(ctx, sandbox.ID, "10.0.0.0/8", "") },
		"deleting an address":    func() error { return s.store.DeleteIPRule(ctx, 1) },
		"deleting a logged call": func() error { return s.store.DeleteTrafficEntry(ctx, 1) },
		"the seeded profiles": func() error {
			_, err := s.store.SeedProfiles(ctx)
			return err
		},
		"the seeded error catalog": func() error {
			_, err := s.store.SeedErrorCatalog(ctx)
			return err
		},
	}

	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, write(), "a write that could not happen must be reported")
		})
	}
}
