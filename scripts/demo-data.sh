#!/usr/bin/env bash
#
# Drive enough traffic through a running stand that every screen has something
# to show — which is what the published snapshot is made from, and what makes a
# first look at the console worth taking.
#
# It is deliberately not a fixture loaded straight into the database: the rows
# are produced by real calls, so the traffic log, the transaction events and the
# merchant's own books all agree the way they would after a rehearsal.
#
#   docker compose up -d
#   docker compose exec -T postgres psql -q -U payme -d paymemock < seed/example_cashboxes.sql
#   scripts/demo-data.sh
#
set -euo pipefail

API=${API:-http://localhost:8082/api}
PSQL="docker compose exec -T postgres psql -qtA -U payme -d paymemock"

rpc() { curl -s -X POST "$API" -H "X-Auth: $1" -H 'Content-Type: application/json' -d "$2"; }

# jqp reads a value out of a JSON-RPC answer by path — `jqp result card token`.
# It prints nothing when any step is missing, which is what the callers test
# for: an error answer has no `result` at all.
jqp() {
  python3 -c '
import sys, json
try:
    node = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for key in sys.argv[1:]:
    if not isinstance(node, dict) or key not in node:
        sys.exit(0)
    node = node[key]
print(node)
' "$@"
}

merchant_of() { $PSQL -c "select merchant_id from control.sandboxes where slug='$1'" | tr -d '\r\n '; }
key_of()      { $PSQL -c "select test_key from control.sandboxes where slug='$1'" | tr -d '\r\n '; }

# new_order prints the id of a fresh order on the named cashbox.
new_order() {
  $PSQL -c "INSERT INTO merchant.orders (sandbox_id, account_id, amount, description)
            SELECT a.sandbox_id, a.id, $2, '$3'
            FROM merchant.accounts a
            JOIN control.sandboxes s ON s.id = a.sandbox_id
            WHERE s.slug = '$1' RETURNING id" | head -1 | tr -d '\r\n '
}

# tokenize registers a card and verifies it, printing the token.
tokenize() {
  local m=$1 number=$2 expire=$3 token
  token=$(rpc "$m" "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"cards.create\",\"params\":{\"card\":{\"number\":\"$number\",\"expire\":\"$expire\"},\"save\":true}}" \
          | jqp result card token)
  [ -z "$token" ] && return 0
  rpc "$m" "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"cards.get_verify_code\",\"params\":{\"token\":\"$token\"}}" >/dev/null
  rpc "$m" "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"cards.verify\",\"params\":{\"token\":\"$token\",\"code\":\"666666\"}}" >/dev/null
  echo "$token"
}

# pay runs one whole payment: an order, a card, a receipt and a settlement.
# Whatever the card is rigged to do is what happens, so the failures below are
# real refusals rather than rows written to look like them.
pay() {
  local box=$1 number=$2 amount=$3 label=$4
  local m k oid token rid out
  m=$(merchant_of "$box"); k=$(key_of "$box")
  oid=$(new_order "$box" "$amount" "$label")
  token=$(tokenize "$m" "$number" "0399")
  [ -z "$token" ] && { printf '  %-22s card refused at registration\n' "$label"; return 0; }
  rid=$(rpc "$m:$k" "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"receipts.create\",\"params\":{\"amount\":$amount,\"account\":{\"order_id\":\"$oid\"}}}" \
        | jqp result receipt _id)
  [ -z "$rid" ] && { printf '  %-22s receipt refused\n' "$label"; return 0; }
  out=$(rpc "$m:$k" "{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"receipts.pay\",\"params\":{\"id\":\"$rid\",\"token\":\"$token\"}}")
  printf '  %-22s %s\n' "$label" "$(echo "$out" | python3 -c "
import sys,json
d=json.load(sys.stdin)
r=d.get('result',{}).get('receipt')
print('settled, state '+str(r['state'])) if r else print('refused, '+str(d.get('error',{}).get('code')))")"
}

# payout hands money back to a saved card, which is the other direction a
# cashbox moves money in. It asks nothing of a merchant — there is no order
# behind it — so it is opened and settled entirely on the provider's side.
payout() {
  local box=$1 number=$2 amount=$3 label=$4 settle=$5
  local m k token rid
  m=$(merchant_of "$box"); k=$(key_of "$box")
  token=$(tokenize "$m" "$number" "0399")
  [ -z "$token" ] && { printf '  %-22s card refused\n' "$label"; return 0; }
  rid=$(rpc "$m:$k" "{\"jsonrpc\":\"2.0\",\"id\":12,\"method\":\"transactions.create\",\"params\":{\"amount\":$amount,\"token\":\"$token\",\"account\":{\"order_id\":\"payout-$RANDOM\"}}}" \
        | jqp result receipt _id)
  [ -z "$rid" ] && { printf '  %-22s payout refused\n' "$label"; return 0; }
  if [ "$settle" != "yes" ]; then
    printf '  %-22s opened, left unsettled\n' "$label"
    return 0
  fi
  rpc "$m:$k" "{\"jsonrpc\":\"2.0\",\"id\":13,\"method\":\"transactions.complete\",\"params\":{\"id\":\"$rid\",\"amount\":$amount,\"token\":\"$token\",\"account\":{\"order_id\":\"payout\"}}}" >/dev/null
  printf '  %-22s settled\n' "$label"
}

# leave_open creates a receipt and walks away, so the screens have payments that
# are neither settled nor cancelled.
leave_open() {
  local box=$1 amount=$2 label=$3 m k oid
  m=$(merchant_of "$box"); k=$(key_of "$box")
  oid=$(new_order "$box" "$amount" "$label")
  rpc "$m:$k" "{\"jsonrpc\":\"2.0\",\"id\":6,\"method\":\"receipts.create\",\"params\":{\"amount\":$amount,\"account\":{\"order_id\":\"$oid\"}}}" >/dev/null
  printf '  %-22s left in progress\n' "$label"
}

# cancel_one creates a receipt and cancels it.
cancel_one() {
  local box=$1 amount=$2 label=$3 m k oid rid
  m=$(merchant_of "$box"); k=$(key_of "$box")
  oid=$(new_order "$box" "$amount" "$label")
  rid=$(rpc "$m:$k" "{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"receipts.create\",\"params\":{\"amount\":$amount,\"account\":{\"order_id\":\"$oid\"}}}" \
        | jqp result receipt _id)
  [ -z "$rid" ] && return 0
  rpc "$m:$k" "{\"jsonrpc\":\"2.0\",\"id\":8,\"method\":\"receipts.cancel\",\"params\":{\"id\":\"$rid\"}}" >/dev/null
  printf '  %-22s cancelled\n' "$label"
}

echo "Settled payments"
pay example-topup    8600123456789012  4500000 "uzcard, large"
pay example-topup    9860123456789012  1250000 "humo"
pay example-topup    8600069195406311   340000 "sandbox card"
pay example-topup    8600495473316478   890000 "sandbox card, second"
pay example-deposit  8600123456789012  2100000 "deposit cashbox"
pay example-deposit  9860123456789012   675000 "deposit, humo"
pay example-dividend 8600123456789012  3300000 "dividend cashbox"

echo "Rigged refusals — the point of the stand"
pay example-topup    4444445987459073   200000 "blocked card"
pay example-topup    3333336415804657   780000 "expired card"
pay example-topup    8600143417770323   960000 "system error"
pay example-topup    8600134301849596   410000 "ten seconds, then fail"
pay example-topup    8600060921090842   150000 "no SMS on the card"

echo "Withdrawals — money back onto a card"
payout example-dividend 8600123456789012 1800000 "dividend payout"   yes
payout example-dividend 9860123456789012  640000 "dividend, humo"    yes
payout example-deposit  8600123456789012  950000 "deposit payout"    yes
payout example-dividend 8600495473316478  300000 "opened, not paid"  no

echo "Neither settled nor cancelled"
leave_open example-topup    520000 "abandoned at checkout"
leave_open example-deposit  980000 "waiting on the payer"

echo "Cancelled"
cancel_one example-topup    410000 "cancelled by the merchant"
cancel_one example-dividend 260000 "cancelled, dividend"

echo "Calls that were refused outright"
m=$(merchant_of example-topup)
rpc "$m:deadbeefdeadbeefdeadbeefdeadbeef" \
  '{"jsonrpc":"2.0","id":9,"method":"receipts.create","params":{"amount":1,"account":{"order_id":"999999"}}}' >/dev/null
rpc "$m:deadbeefdeadbeefdeadbeefdeadbeef" \
  '{"jsonrpc":"2.0","id":10,"method":"receipts.get_all","params":{"count":10,"from":0,"to":0,"offset":0}}' >/dev/null
rpc "$m" '{"jsonrpc":"2.0","id":11,"method":"cards.create","params":{"card":{"number":"0000000000000000","expire":"0399"}}}' >/dev/null
echo "  three refusals logged"

echo "Fault rules"
$PSQL <<'SQL'
DELETE FROM control.fault_rules;

INSERT INTO control.fault_rules (sandbox_id, name, enabled, priority, service, method, action, delay_ms, note)
SELECT id, 'Slow CheckPerformTransaction', TRUE, 10, 'merchant', 'CheckPerformTransaction', 'delay', 3000,
       'Rehearses a merchant that answers just inside the timeout.'
FROM control.sandboxes WHERE slug = 'example-topup';

INSERT INTO control.fault_rules (sandbox_id, name, enabled, priority, service, method, action, error_code, error_message, probability, note)
SELECT id, 'One pay in four fails', TRUE, 20, 'paymemock', 'receipts.pay', 'rpc_error', -31008,
       '{"ru":"Не удалось выполнить операцию","uz":"Amalni bajarib bo''lmadi","en":"Could not perform the operation"}'::jsonb,
       0.25, 'Intermittent by design: a retry path is only tested by a failure that comes and goes.'
FROM control.sandboxes WHERE slug = 'example-topup';

INSERT INTO control.fault_rules (sandbox_id, name, enabled, priority, service, method, action, http_status, times_left, note)
SELECT id, 'Two 502s, then behave', FALSE, 30, 'merchant', '*', 'http_status', 502, 2,
       'Disabled until someone wants it. A budget of two is what a retry has to survive.'
FROM control.sandboxes WHERE slug = 'example-deposit';

INSERT INTO control.fault_rules (sandbox_id, name, enabled, priority, service, method, action, note)
SELECT id, 'Drop every GetStatement', TRUE, 40, 'merchant', 'GetStatement', 'drop',
       'The connection closes with nothing written, which is not the same as an error.'
FROM control.sandboxes WHERE slug = 'example-dividend';

INSERT INTO control.fault_rules (sandbox_id, name, enabled, priority, service, method, action, note)
SELECT id, 'Malformed receipts.get', TRUE, 50, 'paymemock', 'receipts.get', 'malformed',
       'Answers with something no JSON parser accepts.'
FROM control.sandboxes WHERE slug = 'example-topup';

INSERT INTO control.fault_rules (sandbox_id, name, enabled, priority, service, method, action, note)
SELECT id, 'Duplicate PerformTransaction', FALSE, 60, 'merchant', 'PerformTransaction', 'duplicate',
       'Delivers the webhook twice, which is what idempotency is for.'
FROM control.sandboxes WHERE slug = 'example-topup';

DELETE FROM control.ip_rules;
INSERT INTO control.ip_rules (sandbox_id, cidr, note)
SELECT id, '185.8.212.0/24', 'The provider''s published range.' FROM control.sandboxes WHERE slug = 'example-topup';
INSERT INTO control.ip_rules (sandbox_id, cidr, note)
SELECT id, '10.0.0.0/8', 'Our own staging network.' FROM control.sandboxes WHERE slug = 'example-topup';
INSERT INTO control.ip_rules (sandbox_id, cidr, note)
SELECT id, '185.8.212.0/24', 'The provider''s published range.' FROM control.sandboxes WHERE slug = 'example-deposit';
SQL
echo "  six fault rules, three allowlist entries"

echo
echo "Now holding:"
$PSQL -c "select
  (select count(*) from mock.receipts)          || ' receipts, '  ||
  (select count(*) from merchant.transactions)  || ' transactions, ' ||
  (select count(*) from mock.cards)             || ' cards, '     ||
  (select count(*) from control.request_log)    || ' logged calls'"
