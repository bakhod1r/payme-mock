// Package infrastructure implements the Subscribe API ports: the card and
// receipt stores against PostgreSQL, and the merchant client that carries the
// Merchant API chain to the billing side.
//
// Every query is scoped to the sandbox carried in the request context, so one
// stand can never read or write another's cards or receipts.
package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
	"github.com/bakhod1r/payme-mock/internal/kernel/sandboxctx"
)

// CardRepository stores tokenized cards.
type CardRepository struct {
	pool *postgres.Pool
}

// NewCardRepository wires the repository to a pool.
func NewCardRepository(pool *postgres.Pool) *CardRepository {
	return &CardRepository{pool: pool}
}

// cardColumns reads a card as the calling register sees it, which includes the
// token that register was handed: a card carries one per register, and the one
// stored on the row itself is only the first that was issued.
const cardColumns = `
	c.id, c.sandbox_id, coalesce(t.token, c.token), c.number_full, c.expire,
	c.recurrent, c.verify, c.verify_code, c.verify_code_sent_at,
	c.verify_wait_ms, c.phone, c.balance, c.removed, c.outcome, c.source,
	c.sms_enabled, c.frozen, c.delay_ms, c.account, c.customer, c.registered_at`

// cardFrom joins each card to the token this register holds for it, if it has
// been handed one. A register that has never tokenized the card reads the
// card's own, which is what it would be given the moment it asks.
const cardFrom = `
	FROM mock.cards c
	LEFT JOIN mock.card_tokens t ON t.card_id = c.id AND t.sandbox_id = $1`

// cardScope is the set of stands whose cards this one may read: itself, plus
// any stand named as part of the same merchant.
//
// A merchant with several cash registers has one set of cards. The integration
// tokenizes a card through the top-up register and later pays out through the
// dividend one, sending the same token, so a lookup narrowed to the calling
// register alone would answer "no such card" for a card the merchant holds.
// A stand with no merchant named still sees only its own.
const cardScope = `
	c.sandbox_id IN (
		SELECT other.id
		FROM control.sandboxes other, control.sandboxes me
		WHERE me.id = $1
		  AND (other.id = me.id
		       OR (me.merchant_group IS NOT NULL
		           AND other.merchant_group = me.merchant_group)))
	-- A card the merchant stopped taking at this particular register is not
	-- there as far as this register is concerned, which is the same answer it
	-- would give for a card it never held.
	AND NOT EXISTS (
		SELECT 1 FROM mock.card_cashbox_blocks b
		WHERE b.card_id = c.id AND b.sandbox_id = $1)`

// ByToken loads the card a token stands for.
func (r *CardRepository) ByToken(ctx context.Context, token string) (*domain.Card, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	// A token names both a card and the register it was issued to, and the
	// merchant's other registers still honour it: the integration tokenizes at
	// the till it registered the payer on and charges at whichever one the money
	// is moving through. What it must not do is resolve for another merchant.
	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+cardColumns+`
		`+cardFrom+`
		JOIN mock.card_tokens used ON used.card_id = c.id
		WHERE `+cardScope+` AND used.token = $2`, sandboxID, token)

	card, err := scanCard(row)
	if err != nil {
		return nil, err
	}

	// The caller is answered with the token it sent, not with whatever this
	// register holds for the same card.
	card.Token = token

	return card, nil
}

// ByID loads a card by its stored identifier, which a receipt refers to.
func (r *CardRepository) ByID(ctx context.Context, id int64) (*domain.Card, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+cardColumns+`
		`+cardFrom+`
		WHERE `+cardScope+` AND c.id = $2`, sandboxID, id)

	return scanCard(row)
}

// ByNumber loads the card the stand already holds for a number.
//
// A rigged card wins over one the register tokenized earlier: the operator's
// setting is the point of the stand, and a run that tokenized the number first
// must not bury it.
//
// A removed card is not held at all. Handing its token back would answer
// cards.create with a card that then refuses everything asked of it, which is
// what a payer sees after re-adding a card they had deleted.
func (r *CardRepository) ByNumber(ctx context.Context, number string) (*domain.Card, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return nil, err
	}

	row := postgres.From(ctx, r.pool).QueryRow(ctx, `
		SELECT `+cardColumns+`
		`+cardFrom+`
		WHERE `+cardScope+` AND c.number_full = $2 AND NOT c.removed
		ORDER BY (c.source = 'console') DESC, c.id
		LIMIT 1`, sandboxID, number)

	return scanCard(row)
}

// Create stores a newly tokenized card.
func (r *CardRepository) Create(ctx context.Context, c *domain.Card) error {
	err := postgres.From(ctx, r.pool).QueryRow(ctx, `
		INSERT INTO mock.cards
			(sandbox_id, token, number_full, expire, recurrent, verify,
			 verify_code, verify_code_sent_at, verify_wait_ms, phone, balance, removed,
			 outcome, sms_enabled, frozen, delay_ms, account, customer,
			 registered_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
		        $17, $18, $19)
		RETURNING id`,
		c.SandboxID, c.Token, c.NumberFull, c.Expire, c.Recurrent, c.Verify,
		nullableText(c.VerifyCode), c.VerifyCodeSentAt, c.VerifyWaitMillis,
		nullableText(c.Phone), c.Balance, c.Removed, outcomeOrDefault(c.Outcome),
		c.SMSEnabled, c.Frozen, c.DelayMillis, nullableAccount(c.Account),
		nullableText(c.Customer), c.RegisteredAt,
	).Scan(&c.ID)
	if err != nil {
		return fmt.Errorf("insert card: %w", err)
	}

	// The token it was created with is this register's token for it. Every other
	// register gets its own the first time it asks.
	if _, err := r.TokenFor(ctx, c.ID, c.Token); err != nil {
		return err
	}

	return nil
}

// TokenFor returns the token this register holds for a card, issuing the one
// offered if it holds none yet.
//
// Two registers tokenizing the same card are handed different strings, which is
// what an integration storing a token per till already assumes. The card behind
// them is one card: one balance, one behaviour, one verification.
func (r *CardRepository) TokenFor(ctx context.Context, cardID int64, offered string) (string, error) {
	sandboxID, err := sandboxctx.From(ctx)
	if err != nil {
		return "", err
	}

	var token string
	err = postgres.From(ctx, r.pool).QueryRow(ctx, `
		WITH issued AS (
			INSERT INTO mock.card_tokens (card_id, sandbox_id, token)
			VALUES ($1, $2, $3)
			ON CONFLICT (card_id, sandbox_id) DO NOTHING
			RETURNING token
		)
		SELECT token FROM issued
		UNION ALL
		SELECT token FROM mock.card_tokens
		WHERE card_id = $1 AND sandbox_id = $2
		LIMIT 1`, cardID, sandboxID, offered).Scan(&token)
	if err != nil {
		return "", fmt.Errorf("issue card token: %w", err)
	}

	return token, nil
}

// Update persists a verification, a charge or a removal.
func (r *CardRepository) Update(ctx context.Context, c *domain.Card) error {
	tag, err := postgres.From(ctx, r.pool).Exec(ctx, `
		UPDATE mock.cards
		SET verify = $1, verify_code = $2, verify_code_sent_at = $3,
		    verify_wait_ms = $4, phone = $5, balance = $6, removed = $7,
		    outcome = $8, recurrent = $9, account = $10, customer = $11,
		    registered_at = $12
		WHERE id = $13`,
		c.Verify, nullableText(c.VerifyCode), c.VerifyCodeSentAt,
		c.VerifyWaitMillis, nullableText(c.Phone), c.Balance, c.Removed,
		outcomeOrDefault(c.Outcome), c.Recurrent, nullableAccount(c.Account),
		nullableText(c.Customer), c.RegisteredAt, c.ID)
	if err != nil {
		return fmt.Errorf("update card: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return nil
}

func scanCard(row scanner) (*domain.Card, error) {
	var (
		card       domain.Card
		verifyCode *string
		phone      *string
		account    map[string]string
		customer   *string
	)

	err := row.Scan(
		&card.ID, &card.SandboxID, &card.Token, &card.NumberFull, &card.Expire,
		&card.Recurrent, &card.Verify, &verifyCode, &card.VerifyCodeSentAt,
		&card.VerifyWaitMillis, &phone, &card.Balance, &card.Removed,
		&card.Outcome, &card.Source, &card.SMSEnabled, &card.Frozen,
		&card.DelayMillis, &account, &customer, &card.RegisteredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan card: %w", err)
	}

	if verifyCode != nil {
		card.VerifyCode = *verifyCode
	}
	if phone != nil {
		card.Phone = *phone
	}
	card.Account = account
	if customer != nil {
		card.Customer = *customer
	}

	return &card, nil
}

// scanner is satisfied by both a single row and a row from a result set.
type scanner interface {
	Scan(dest ...any) error
}

// outcomeOrDefault fills in the ordinary behaviour for a card built without
// one, so a zero value never reaches the column's check constraint.
func outcomeOrDefault(o domain.Outcome) domain.Outcome {
	if o == "" {
		return domain.OutcomeSuccess
	}
	return o
}

// nullableAccount stores an account nobody sent as NULL, so a token saved
// without one reads back as unset rather than as an empty object.
func nullableAccount(account map[string]string) any {
	if len(account) == 0 {
		return nil
	}
	return account
}

// nullableText stores an empty string as NULL, so a column that means "not set
// yet" reads back as unset rather than as a blank value.
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
