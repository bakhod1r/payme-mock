-- Seed: an example integration — three cashboxes and the cards a test suite
-- drives against them.
--
-- Running it puts the stand back to the state a rehearsal starts from: the
-- three cashboxes, one payer each, the rigged cards, and nothing else. What an
-- earlier run left — its payments, orders, tokenized cards and traffic — goes,
-- because a console still showing them cannot be read against the next run.
--
-- Adapt it to your own integration: the merchant ids and test keys below are
-- placeholders. Replace them with the ones your backend already carries in its
-- environment and nothing has to be repointed afterwards.
--
-- It is a seed rather than a migration because it is one integration's data,
-- not the stand's shape.
--
--   docker compose exec -T postgres psql -U payme -d paymemock < seed/example_cashboxes.sql
--
BEGIN;

-- The stand is here to rehearse one integration: a backend that holds three
-- cashboxes with the provider — top-up, deposit, dividend — and one set of
-- cards across them.
--
-- Everything the stand accumulated earlier goes: leftover demonstration
-- sandboxes make the console unreadable and make it impossible to tell a
-- rehearsal's own traffic from an older run's.
DELETE FROM control.sandboxes
WHERE merchant_id NOT IN (
    '000000000000000000000001',
    '000000000000000000000002',
    '000000000000000000000003');

-- The three cashboxes. The merchant id and the test key are the ones the
-- backend under test sends, so they are fixed rather than generated. A cashbox
-- already carrying them is renamed and regrouped rather than replaced: its
-- production key is what an operator may have handed out.
INSERT INTO control.sandboxes (slug, name, merchant_id, key, test_key, kind,
                               merchant_group, merchant_name)
VALUES
    ('example-topup', 'Top-up cashbox', '000000000000000000000001',
     md5('example-topup-live'), md5('example-topup-test'), 'topup', 'example', 'Example'),
    ('example-deposit', 'Deposit cashbox', '000000000000000000000002',
     md5('example-deposit-live'), md5('example-deposit-test'), 'deposit', 'example', 'Example'),
    ('example-dividend', 'Dividend cashbox', '000000000000000000000003',
     md5('example-dividend-live'), md5('example-dividend-test'), 'dividend', 'example', 'Example')
ON CONFLICT (merchant_id) DO UPDATE
SET slug           = excluded.slug,
    name           = excluded.name,
    merchant_name  = excluded.merchant_name,
    test_key       = excluded.test_key,
    kind           = excluded.kind,
    merchant_group = excluded.merchant_group,
    archived       = FALSE;

-- Everything a rehearsal produces. Accounts go with the rest: the stand
-- registers a payer of its own for every unknown order it is sent, so a few
-- runs leave a list of them named after order identifiers and nothing else.
-- They cascade to the orders and payments held against them.
DELETE FROM mock.receipts     WHERE sandbox_id IN (SELECT id FROM control.sandboxes);
DELETE FROM mock.transactions WHERE sandbox_id IN (SELECT id FROM control.sandboxes);
DELETE FROM mock.cards        WHERE sandbox_id IN (SELECT id FROM control.sandboxes);
DELETE FROM merchant.accounts WHERE sandbox_id IN (SELECT id FROM control.sandboxes);
DELETE FROM control.request_log;

-- Every cashbox answers the Merchant API out of a payer, and a cashbox without
-- one fails on its first call, so each gets one with a balance large enough
-- that a rehearsal never fails for want of money.
INSERT INTO merchant.accounts (sandbox_id, phone, name, balance)
SELECT s.id, '901234567', s.name || ' payer', 100000000000
FROM control.sandboxes s
WHERE s.merchant_group = 'example';

-- The cards an integration's test suite drives, seeded into the top-up
-- cashbox — the one that tokenizes — and reachable from the other two because
-- all three name the same merchant.
--
-- The first seven are the provider's published sandbox numbers, each standing
-- for one failure an integration has to survive; the last two are plain cards
-- for end-to-end runs that register and pay. Two of the numbers are on no
-- Uzbek network at all: the provider uses them for a card the processing side
-- rejects outright.
--
-- The OTP is not stored. These rows are added the way an operator adds a card,
-- so the stand answers them with its shared code, 666666. Only a card an
-- integration tokenized for itself falls back to its own expiry, where 03/99
-- takes 039999.
--
-- https://developer.help.paycom.uz/integratsiya-s-mobilnym-prilozheniem/testirovanie-v-pesochnitse
INSERT INTO mock.cards (sandbox_id, token, number_full, expire, recurrent, verify,
                        balance, outcome, verify_wait_ms, source, sms_enabled,
                        frozen, delay_ms, phone)
SELECT
    (SELECT id FROM control.sandboxes WHERE merchant_id = '000000000000000000000001'),
    -- A token the provider issues is 64 base64 characters; these are derived
    -- from the number so a reseeded stand hands back the same one.
    encode(decode(md5(c.number) || md5(c.number || 'b') || md5(c.number || 'c'), 'hex'), 'base64'),
    c.number, c.expire, TRUE, TRUE, 1000000000, c.outcome, 60000, 'console',
    c.sms_enabled, FALSE, c.delay_ms, '+998901234567'
FROM (VALUES
    -- Success.
    ('8600069195406311', '03/99', 'success',            TRUE,      0),
    ('8600495473316478', '03/99', 'success',            TRUE,      0),
    -- SMS notification not enabled on the card.
    ('8600060921090842', '03/99', 'success',            FALSE,     0),
    -- Card expired.
    ('3333336415804657', '03/99', 'expired',            TRUE,      0),
    -- Card blocked.
    ('4444445987459073', '03/99', 'blocked',            TRUE,      0),
    -- Unknown system error.
    ('8600143417770323', '03/99', 'system_error',       TRUE,      0),
    -- Ten seconds of processing, then a failure. The pair is the point: a
    -- delay on its own is not what a timeout is written against.
    ('8600134301849596', '03/99', 'system_error',       TRUE, 10000),
    -- Two ordinary cards for end-to-end runs: Uzcard and Humo.
    ('8600123456789012', '12/26', 'success',            TRUE,      0),
    ('9860123456789012', '12/26', 'success',            TRUE,      0)
) AS c(number, expire, outcome, sms_enabled, delay_ms)
WHERE NOT EXISTS (
    SELECT 1 FROM mock.cards existing
    WHERE existing.sandbox_id =
          (SELECT id FROM control.sandboxes WHERE merchant_id = '000000000000000000000001')
      AND existing.number_full = c.number);

-- A token is held per cashbox, so the row the card was seeded with is recorded
-- as the top-up cashbox's. The other two are handed their own the first time
-- they tokenize the card.
INSERT INTO mock.card_tokens (card_id, sandbox_id, token)
SELECT c.id, c.sandbox_id, c.token
FROM mock.cards c
ON CONFLICT DO NOTHING;

COMMIT;
