-- +goose Up

-- Three schemas, one per side of the rehearsal:
--
--   merchant  what the backend under test owns — its payers, orders and the
--             transactions the provider opens against them
--   mock      what the Payme emulator owns — cards, receipts and its own view
--             of a transaction
--   control   the control plane the console drives — sandboxes, fault rules,
--             the IP allowlist and the traffic log
--
-- They are separate schemas rather than prefixes because the two payment sides
-- hold tables of the same name on purpose: each keeps its own record of a
-- transaction, and a rehearsal is worth running precisely when the two
-- disagree.
CREATE SCHEMA merchant;
CREATE SCHEMA mock;
CREATE SCHEMA control;

-- ---------------------------------------------------------------------------
-- control: the stand itself
-- ---------------------------------------------------------------------------

-- A named bundle of fault rules that can be switched onto a sandbox as a unit,
-- so a scenario is set up once and replayed rather than rebuilt rule by rule.
-- A builtin config ships with the stand and the console refuses to delete it.
CREATE TABLE control.configs (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    settings    jsonb NOT NULL,
    builtin     boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- One sandbox is one cashbox: the merchant id and keys a backend is
-- configured with, plus the endpoint the gateway publishes for it. Everything
-- else in the stand hangs off a sandbox, which is what keeps two integrations
-- sharing one stand from seeing each other's traffic.
--
-- kind names which of the provider's cashbox types this one stands for; the
-- Subscribe API behaves differently for a payout cashbox than for a top-up.
--
-- merchant_group is what makes a card reachable from more than one cashbox:
-- the provider tokenizes a card per cashbox but knows it belongs to one
-- merchant, so cashboxes naming the same group share their cards.
CREATE TABLE control.sandboxes (
    id               bigserial PRIMARY KEY,
    slug             text NOT NULL UNIQUE,
    name             text NOT NULL,
    merchant_id      text NOT NULL UNIQUE,
    key              text NOT NULL,
    test_key         text NOT NULL,
    active_config_id bigint REFERENCES control.configs (id),
    archived         boolean NOT NULL DEFAULT false,
    last_seen_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    kind             text NOT NULL DEFAULT 'topup'
                     CHECK (kind IN ('topup', 'dividend', 'deposit')),
    merchant_group   text,
    merchant_name    text
);

CREATE INDEX sandboxes_merchant_group ON control.sandboxes (merchant_group)
    WHERE merchant_group IS NOT NULL;

-- The protocol's error codes, held as data so the console can show what a code
-- means and a fault rule can be written against a name instead of a number.
-- scope says which API the code belongs to; the same number means different
-- things on the Merchant and Subscribe sides.
CREATE TABLE control.error_catalog (
    code       integer PRIMARY KEY,
    slug       text NOT NULL UNIQUE,
    scope      text NOT NULL CHECK (scope IN ('general', 'merchant', 'subscribe')),
    message    jsonb NOT NULL,
    data_field text,
    methods    text[] NOT NULL DEFAULT '{}',
    builtin    boolean NOT NULL DEFAULT true
);

-- One way the stand can be told to misbehave. A rule matches on the service,
-- the method, the account object, the transaction id and the amount, and then
-- does one thing: hold the call, answer with an error, answer with an HTTP
-- status, return something unparseable, drop the connection, or send the
-- webhook twice.
--
-- priority orders the rules and the first match wins, so a broad rule can sit
-- behind a narrow one. probability and times_left make a rule intermittent,
-- which is what an integration's retry path actually has to survive.
CREATE TABLE control.fault_rules (
    id             bigserial PRIMARY KEY,
    config_id      bigint REFERENCES control.configs (id) ON DELETE CASCADE,
    sandbox_id     bigint REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    name           text NOT NULL,
    enabled        boolean NOT NULL DEFAULT true,
    priority       integer NOT NULL DEFAULT 100,
    service        text NOT NULL DEFAULT '*'
                   CHECK (service IN ('merchant', 'paymemock', 'gateway', '*')),
    method         text NOT NULL DEFAULT '*',
    match_account  jsonb,
    match_payme_id text,
    amount_min     bigint,
    amount_max     bigint,
    action         text NOT NULL
                   CHECK (action IN ('delay', 'rpc_error', 'http_status',
                                     'malformed', 'drop', 'duplicate', 'passthrough')),
    delay_ms       integer NOT NULL DEFAULT 0 CHECK (delay_ms >= 0),
    error_code     integer,
    error_message  jsonb,
    error_data     text,
    http_status    integer,
    probability    real NOT NULL DEFAULT 1.0
                   CHECK (probability >= 0 AND probability <= 1),
    times_left     integer,
    hit_count      bigint NOT NULL DEFAULT 0,
    note           text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX fault_rules_lookup     ON control.fault_rules (config_id, enabled, priority);
CREATE INDEX fault_rules_by_sandbox ON control.fault_rules (sandbox_id, enabled, priority);

-- The provider only calls a merchant from its own addresses, and an
-- integration that never rehearses being refused discovers the allowlist in
-- production. An empty list for a sandbox means no restriction.
CREATE TABLE control.ip_rules (
    id         bigserial PRIMARY KEY,
    sandbox_id bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    cidr       cidr NOT NULL,
    note       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX ip_rules_cidr_per_sandbox ON control.ip_rules (sandbox_id, cidr);

-- Every call in or out, with both bodies and both header sets, which is the
-- whole point of a stand: an integration that fails is read here rather than
-- guessed at. fault_rule_id records that a call was answered by a rule instead
-- of by the protocol, so a deliberate failure is never mistaken for a real one.
CREATE TABLE control.request_log (
    id               bigserial PRIMARY KEY,
    sandbox_id       bigint REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    service          text NOT NULL,
    direction        text NOT NULL CHECK (direction IN ('in', 'out')),
    method           text,
    rpc_id           text,
    http_status      integer,
    request_body     jsonb,
    response_body    jsonb,
    request_headers  jsonb,
    response_headers jsonb,
    duration_ms      integer NOT NULL,
    fault_rule_id    bigint REFERENCES control.fault_rules (id) ON DELETE SET NULL,
    error_code       integer,
    remote_addr      text,
    at               timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX request_log_recent     ON control.request_log (at DESC);
CREATE INDEX request_log_by_sandbox ON control.request_log (sandbox_id, at DESC);
CREATE INDEX request_log_by_method  ON control.request_log (service, method, at DESC);

-- A call the caller has already made. The provider retries a webhook it did
-- not get an answer to, and the second attempt must return the first answer
-- rather than act again. body_hash is kept so a reused request id carrying a
-- different body is caught instead of silently answered from the cache.
CREATE TABLE control.idempotent_calls (
    sandbox_id bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    method     text NOT NULL,
    request_id text NOT NULL,
    body_hash  text NOT NULL,
    response   jsonb NOT NULL,
    at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (sandbox_id, method, request_id)
);

CREATE INDEX idempotent_calls_by_age ON control.idempotent_calls (at DESC);

-- ---------------------------------------------------------------------------
-- merchant: the side the provider calls
-- ---------------------------------------------------------------------------

-- A payer. Either a phone or a login identifies one, depending on what the
-- cashbox is configured to key its account object on, so both are nullable and
-- each is unique only within a sandbox.
CREATE TABLE merchant.accounts (
    id         bigserial PRIMARY KEY,
    sandbox_id bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    phone      text,
    login      text,
    name       text NOT NULL DEFAULT '',
    balance    bigint NOT NULL DEFAULT 0,
    blocked    boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX accounts_phone_per_sandbox ON merchant.accounts (sandbox_id, phone)
    WHERE phone IS NOT NULL;
CREATE UNIQUE INDEX accounts_login_per_sandbox ON merchant.accounts (sandbox_id, login)
    WHERE login IS NOT NULL;

-- What is being paid for. Amounts are in tiyin throughout, as the protocol
-- sends them.
CREATE TABLE merchant.orders (
    id          bigserial PRIMARY KEY,
    sandbox_id  bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    account_id  bigint NOT NULL REFERENCES merchant.accounts (id) ON DELETE CASCADE,
    amount      bigint NOT NULL CHECK (amount > 0),
    status      text NOT NULL DEFAULT 'new'
                CHECK (status IN ('new', 'processing', 'paid', 'cancelled')),
    detail      jsonb,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX orders_by_account ON merchant.orders (sandbox_id, account_id, status);

-- The Merchant API's transaction. state and reason are the protocol's own
-- numbers rather than names, because that is what goes over the wire and a
-- translation here would be one more thing to disbelieve when a rehearsal
-- disagrees with production. The times are the protocol's milliseconds, kept
-- alongside created_at rather than derived from it: a stand that runs its
-- clock forward must not have its wire values recomputed underneath it.
CREATE TABLE merchant.transactions (
    id           bigserial PRIMARY KEY,
    sandbox_id   bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    payme_id     text NOT NULL,
    order_id     bigint REFERENCES merchant.orders (id) ON DELETE SET NULL,
    account_id   bigint NOT NULL REFERENCES merchant.accounts (id) ON DELETE CASCADE,
    account      jsonb NOT NULL,
    amount       bigint NOT NULL CHECK (amount > 0),
    state        smallint NOT NULL CHECK (state IN (1, 2, -1, -2)),
    reason       smallint,
    payme_time   bigint NOT NULL,
    create_time  bigint NOT NULL,
    perform_time bigint NOT NULL DEFAULT 0,
    cancel_time  bigint NOT NULL DEFAULT 0,
    receivers    jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX transactions_payme_id_per_sandbox
    ON merchant.transactions (sandbox_id, payme_id);
CREATE INDEX transactions_by_create_time
    ON merchant.transactions (sandbox_id, create_time);

-- An order may have only one transaction in flight. Two of them is the classic
-- double-charge, and the database is where it has to be refused: the check
-- cannot be done in application code without a race.
CREATE UNIQUE INDEX transactions_one_active_per_order
    ON merchant.transactions (order_id)
    WHERE state IN (1, 2) AND order_id IS NOT NULL;

-- Every state change with the method that caused it, so the console can show
-- how a transaction got where it is. idempotent_hit marks a call that changed
-- nothing because it had already been answered.
CREATE TABLE merchant.transaction_events (
    id             bigserial PRIMARY KEY,
    transaction_id bigint NOT NULL REFERENCES merchant.transactions (id) ON DELETE CASCADE,
    method         text NOT NULL,
    from_state     smallint,
    to_state       smallint,
    idempotent_hit boolean NOT NULL DEFAULT false,
    error_code     integer,
    at             timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX transaction_events_by_transaction
    ON merchant.transaction_events (transaction_id, at);

-- Every balance move, with the balance on both sides of it. A balance that
-- looks wrong is then a question about a row rather than about the whole
-- history, and a console top-up is told apart from a payment by source.
CREATE TABLE merchant.balance_events (
    id             bigserial PRIMARY KEY,
    sandbox_id     bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    account_id     bigint NOT NULL REFERENCES merchant.accounts (id) ON DELETE CASCADE,
    source         text NOT NULL CHECK (source IN ('console', 'payment')),
    delta          bigint NOT NULL,
    balance_before bigint NOT NULL,
    balance_after  bigint NOT NULL,
    transaction_id bigint REFERENCES merchant.transactions (id) ON DELETE SET NULL,
    note           text NOT NULL DEFAULT '',
    at             timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX balance_events_by_sandbox ON merchant.balance_events (sandbox_id, at DESC);
CREATE INDEX balance_events_by_account ON merchant.balance_events (account_id, at DESC);

-- ---------------------------------------------------------------------------
-- mock: the provider side
-- ---------------------------------------------------------------------------

-- A card the stand knows about, either registered through the Subscribe API or
-- added in the console.
--
-- outcome is what makes the stand useful: a card can be rigged to report
-- itself blocked, expired, out of funds, or to fail verification, so an
-- integration can be driven down every path without waiting for a real card to
-- misbehave. delay_ms holds the answer back, sms_enabled withholds the OTP,
-- and frozen keeps a card from ever settling.
--
-- merchant_key is what a card is shared by; it is maintained by the trigger
-- below rather than written by the application, because it has to stay correct
-- when a cashbox is moved between merchant groups.
CREATE TABLE mock.cards (
    id                  bigserial PRIMARY KEY,
    sandbox_id          bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    token               text NOT NULL,
    number_full         text NOT NULL,
    expire              text NOT NULL,
    recurrent           boolean NOT NULL DEFAULT false,
    verify              boolean NOT NULL DEFAULT false,
    verify_code         text,
    verify_code_sent_at bigint NOT NULL DEFAULT 0,
    verify_wait_ms      bigint NOT NULL DEFAULT 0,
    phone               text,
    balance             bigint NOT NULL DEFAULT 100000000,
    removed             boolean NOT NULL DEFAULT false,
    outcome             text NOT NULL DEFAULT 'success'
                        CHECK (outcome IN ('success', 'insufficient_funds', 'blocked',
                                           'expired', 'verify_failed', 'system_error')),
    source              text NOT NULL DEFAULT 'api' CHECK (source IN ('api', 'console')),
    sms_enabled         boolean NOT NULL DEFAULT true,
    frozen              boolean NOT NULL DEFAULT false,
    delay_ms            bigint NOT NULL DEFAULT 0 CHECK (delay_ms >= 0),
    account             jsonb,
    customer            text,
    merchant_key        text NOT NULL,
    registered_at       bigint NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX cards_token_per_sandbox ON mock.cards (sandbox_id, token);

-- +goose StatementBegin
-- The merchant a sandbox belongs to. A sandbox outside any group is its own
-- merchant, so an ungrouped cashbox never shares cards by accident.
CREATE FUNCTION mock.card_merchant_key(sandbox bigint) RETURNS text
    LANGUAGE sql STABLE AS $$
    SELECT coalesce(s.merchant_group, 'sandbox:' || s.id)
    FROM control.sandboxes s
    WHERE s.id = sandbox
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION mock.set_card_merchant_key() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.merchant_key := mock.card_merchant_key(NEW.sandbox_id);
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Moving a cashbox into another group has to move its cards with it, or the
-- uniqueness below would be enforced against a key nothing uses any more.
CREATE FUNCTION mock.refresh_card_merchant_keys() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    UPDATE mock.cards
    SET merchant_key = coalesce(NEW.merchant_group, 'sandbox:' || NEW.id)
    WHERE sandbox_id = NEW.id;

    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER cards_merchant_key
    BEFORE INSERT OR UPDATE OF sandbox_id ON mock.cards
    FOR EACH ROW EXECUTE FUNCTION mock.set_card_merchant_key();

CREATE TRIGGER sandboxes_merchant_group_moved
    AFTER UPDATE OF merchant_group ON control.sandboxes
    FOR EACH ROW WHEN (new.merchant_group IS DISTINCT FROM old.merchant_group)
    EXECUTE FUNCTION mock.refresh_card_merchant_keys();

-- One card is one row per merchant, however many cashboxes that merchant has.
-- The provider holds the card once and hands out a token per cashbox, so a
-- second row for the same number would give it two balances that drift apart.
CREATE UNIQUE INDEX cards_one_per_merchant ON mock.cards (merchant_key, number_full);

-- The token a given cashbox knows a card by. It is a table rather than a
-- column because the same card carries a different token in each cashbox, and
-- a token is globally unique in the provider's own namespace.
CREATE TABLE mock.card_tokens (
    card_id    bigint NOT NULL REFERENCES mock.cards (id) ON DELETE CASCADE,
    sandbox_id bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    token      text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (card_id, sandbox_id)
);

-- A card blocked for one cashbox only. The shared row above means a card
-- removed in one place would otherwise vanish from all of them, which is not
-- what the provider does.
CREATE TABLE mock.card_cashbox_blocks (
    card_id    bigint NOT NULL REFERENCES mock.cards (id) ON DELETE CASCADE,
    sandbox_id bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (card_id, sandbox_id)
);

-- The Subscribe API's receipt: a payment as the provider sees it, from created
-- through paid or cancelled. state is the protocol's number for the same
-- reason the merchant side keeps its own.
--
-- hold and hold_expire carry a two-phase payment, payout marks a receipt that
-- moves money out rather than in, and merchant_txn records which merchant-side
-- transaction it settled — the link an operator needs when the two sides
-- disagree.
CREATE TABLE mock.receipts (
    id            bigserial PRIMARY KEY,
    sandbox_id    bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    receipt_id    text NOT NULL,
    merchant_id   text NOT NULL,
    amount        bigint NOT NULL CHECK (amount > 0),
    currency      integer NOT NULL DEFAULT 860,
    commission    bigint NOT NULL DEFAULT 0,
    state         smallint NOT NULL DEFAULT 0
                  CHECK (state IN (0, 1, 2, 3, 4, 5, 6, 20, 21, 30, 50)),
    type          smallint NOT NULL DEFAULT 0,
    hold          boolean NOT NULL DEFAULT false,
    hold_expire   bigint NOT NULL DEFAULT 0,
    payout        boolean NOT NULL DEFAULT false,
    card_system   text CHECK (card_system IN ('uzcard', 'humo')),
    account       jsonb NOT NULL,
    detail        jsonb,
    description   text NOT NULL DEFAULT '',
    card_id       bigint REFERENCES mock.cards (id) ON DELETE SET NULL,
    payer         jsonb,
    processing_id text,
    merchant_txn  text,
    create_time   bigint NOT NULL,
    pay_time      bigint NOT NULL DEFAULT 0,
    cancel_time   bigint NOT NULL DEFAULT 0,
    meta          jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX receipts_receipt_id_per_sandbox ON mock.receipts (sandbox_id, receipt_id);
CREATE INDEX receipts_by_create_time  ON mock.receipts (sandbox_id, create_time DESC);
CREATE INDEX receipts_by_state        ON mock.receipts (sandbox_id, state);
CREATE INDEX receipts_by_merchant_txn ON mock.receipts (sandbox_id, merchant_txn)
    WHERE merchant_txn IS NOT NULL;

-- The provider's own record of the transaction it opened against a merchant.
-- It duplicates merchant.transactions deliberately: holding one row for both
-- sides would make the two agree by construction, and the disagreements are
-- what a stand exists to reproduce.
CREATE TABLE mock.transactions (
    id           bigserial PRIMARY KEY,
    sandbox_id   bigint NOT NULL REFERENCES control.sandboxes (id) ON DELETE CASCADE,
    payme_id     text NOT NULL,
    receipt_id   text,
    account      jsonb NOT NULL,
    amount       bigint NOT NULL,
    state        smallint NOT NULL,
    reason       smallint,
    "time"       bigint NOT NULL,
    create_time  bigint NOT NULL DEFAULT 0,
    perform_time bigint NOT NULL DEFAULT 0,
    cancel_time  bigint NOT NULL DEFAULT 0,
    merchant_txn text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX mock_transactions_payme_id_per_sandbox
    ON mock.transactions (sandbox_id, payme_id);

-- +goose Down
DROP SCHEMA control CASCADE;
DROP SCHEMA mock CASCADE;
DROP SCHEMA merchant CASCADE;
