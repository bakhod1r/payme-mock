package main

import (
	"context"
	"fmt"

	subscribe "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// The provider publishes a fixed set of sandbox cards, each standing for one
// failure an integration has to survive. Rehearsing against the same numbers
// means the people who wrote the integration recognise what they are testing,
// so the set is reproduced here rather than approximated.
//
// Two of the numbers are on no Uzbek network at all — the provider uses them
// to stand for a card the processing side rejects outright — which is why the
// console accepts any sixteen digits rather than only 8600 and 9860.
//
// https://developer.help.paycom.uz/integratsiya-s-mobilnym-prilozheniem/testirovanie-v-pesochnitse
func paymeTestCards() []newCard {
	const expire = "0399"

	return []newCard{
		{
			Number: "8600069195406311", Expire: expire,
			Outcome: subscribe.OutcomeSuccess, SMSEnabled: true,
		},
		{
			Number: "8600495473316478", Expire: expire,
			Outcome: subscribe.OutcomeSuccess, SMSEnabled: true,
		},
		{
			// SMS-информирование не подключено.
			Number: "8600060921090842", Expire: expire,
			Outcome: subscribe.OutcomeSuccess, SMSEnabled: false,
		},
		{
			// Срок действия карты истёк.
			Number: "3333336415804657", Expire: expire,
			Outcome: subscribe.OutcomeExpired, SMSEnabled: true,
		},
		{
			// Карта заблокирована.
			Number: "4444445987459073", Expire: expire,
			Outcome: subscribe.OutcomeBlocked, SMSEnabled: true,
		},
		{
			// Неизвестная системная ошибка.
			Number: "8600143417770323", Expire: expire,
			Outcome: subscribe.OutcomeSystemError, SMSEnabled: true,
		},
		{
			// Ten seconds of processing, then a failure. The pair is the point:
			// a delay alone is not what a timeout is written against.
			Number: "8600134301849596", Expire: expire,
			Outcome: subscribe.OutcomeSystemError, SMSEnabled: true, DelayMs: 10_000,
		},
	}
}

// paymeCardBalance is what a seeded card starts with: enough that a run of
// rehearsal payments never fails for the wrong reason.
const paymeCardBalance int64 = 1_000_000_000

// SeedPaymeCards adds the provider's documented cards to a stand and reports
// how many were new.
//
// A number already on the stand is left alone rather than reset: an operator
// who tuned one and pressed the button again meant to fill the gaps, not to
// undo their own work.
func (s *store) SeedPaymeCards(ctx context.Context, sandboxID int64) (int, error) {
	var added int

	err := postgres.WithTx(ctx, s.pool, func(inner context.Context) error {
		var exists bool
		if err := postgres.From(inner, s.pool).QueryRow(inner,
			`SELECT EXISTS (SELECT 1 FROM control.sandboxes WHERE id = $1)`,
			sandboxID).Scan(&exists); err != nil {
			return fmt.Errorf("check sandbox: %w", err)
		}
		if !exists {
			return errNotFound
		}

		added = 0

		for _, card := range paymeTestCards() {
			tag, err := postgres.From(inner, s.pool).Exec(inner, `
				INSERT INTO mock.cards
					(sandbox_id, token, number_full, expire, recurrent, verify,
					 balance, outcome, verify_wait_ms, source, sms_enabled,
					 frozen, delay_ms)
				SELECT $1, $2, $3, $4, TRUE, TRUE, $5, $6, $7, $8, $9, FALSE, $10
				-- A card is one per merchant, not one per stand, so a number a
				-- sibling register already holds is left alone rather than
				-- inserted into a unique index that would refuse it.
				WHERE NOT EXISTS (
					SELECT 1 FROM mock.cards existing
					WHERE existing.number_full = $3
					  AND existing.merchant_key = mock.card_merchant_key($1))`,
				sandboxID, infrastructure.NewTokens().CardToken(), card.Number,
				subscribe.FormatExpire(card.Expire), paymeCardBalance,
				string(card.Outcome), subscribe.DefaultVerifyWaitMillis,
				sourceConsole, card.SMSEnabled, card.DelayMs)
			if err != nil {
				return fmt.Errorf("insert test card: %w", err)
			}

			added += int(tag.RowsAffected())
		}

		// Each card is recorded as tokenized by the register it was added to.
		// A token belongs to a register, and a card no register holds a token
		// for could not be charged at all.
		if _, err := postgres.From(inner, s.pool).Exec(inner, `
			INSERT INTO mock.card_tokens (card_id, sandbox_id, token)
			SELECT c.id, c.sandbox_id, c.token
			FROM mock.cards c
			WHERE c.sandbox_id = $1
			ON CONFLICT DO NOTHING`, sandboxID); err != nil {
			return fmt.Errorf("issue test card tokens: %w", err)
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	return added, nil
}
