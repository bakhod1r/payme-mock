package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	subscribe "github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/domain"
	"github.com/bakhod1r/payme-mock/internal/context/payment/subscribe/infrastructure"
)

// A card the console adds is the counterpart of the one the Subscribe API
// tokenizes: same table, same behaviour, except that an operator picks what it
// does. A stand needs a card that refuses as much as one that pays, and the
// API has no way to ask for one.

// cardRow is a mock card as the screens show it.
type cardRow struct {
	ID        int64
	SandboxID int64
	Sandbox   string
	Token     string
	Number    string
	// Mask is the number as the provider returns it, which is what a list
	// shows: the full number belongs on the card's own page.
	Mask      string
	Expire    string
	System    string
	Outcome   string
	Behaviour string
	// Source says how the card got here: an operator added it, or an
	// integration tokenized it through cards.create. Both are mock cards; a
	// list is only readable if it says which is which.
	Source string
	// Merchant is who holds the card: its registers all charge it. Empty means
	// the stand belongs to no merchant and keeps the card to itself.
	Merchant   string
	Created    string
	Recurrent  bool
	Verify     bool
	Phone      string
	Balance    int64
	Removed    bool
	SMSEnabled bool
	Frozen     bool
	// CodeSentAt is when an OTP was last issued, in protocol milliseconds. A
	// card waiting on a code is the most live thing on a stand, so the moment
	// is carried even though no list shows it as a timestamp.
	CodeSentAt int64
	// Account and Customer are what cards.create was told the card is for.
	// They are the integration's own words about the token, so they are shown
	// rather than interpreted.
	Account  string
	Customer string
	// Delay is the stall in seconds, which is the unit an operator thinks in;
	// the column holds milliseconds.
	Delay float64
	// Fails reports a card rigged to refuse, so a table can mark it without
	// comparing strings in the template.
	Fails bool
	// Added reports a card an operator added, which is the half of the list
	// nobody else could have put there.
	Added bool
	// RegisteredAt is when an integration last tokenized the card, zero if none
	// ever has. A card can be both added and registered: the operator rigged the
	// number, the register then asked for it and got that very card.
	RegisteredAt int64
	// Registered is that fact as a screen reads it.
	Registered bool
	// OTP is the code cards.get_verify_code will send for this card, which is
	// its expiry with the year repeated. It is on the screen so it need not be
	// worked out or read out of a log.
	OTP string
}

// newCard is what the create form submits.
type newCard struct {
	SandboxID  int64
	Number     string
	Expire     string
	Phone      string
	Balance    int64
	Outcome    subscribe.Outcome
	Recurrent  bool
	Verify     bool
	SMSEnabled bool
	Frozen     bool
	// CodeSentAt is when an OTP was last issued, in protocol milliseconds. A
	// card waiting on a code is the most live thing on a stand, so the moment
	// is carried even though no list shows it as a timestamp.
	CodeSentAt int64
	// Account and Customer are what cards.create was told the card is for.
	// They are the integration's own words about the token, so they are shown
	// rather than interpreted.
	Account  string
	Customer string
	DelayMs  int64
}

// behaviourOption is one entry of the card behaviour dropdown.
type behaviourOption struct {
	Value string
	Label string
	// Failing marks the entries that make the card refuse, so the form can set
	// them apart from the one that works.
	Failing bool
}

// cardFilterOutcomes are the behaviours the filter may narrow by, with the
// grouping that catches every refusal at the top.
func cardFilterOutcomes() []behaviourOption {
	return append([]behaviourOption{
		{Value: filterFailing, Label: "anything that refuses", Failing: true},
	}, cardOutcomes()...)
}

// cardOutcomes are the behaviours an operator may give a card.
func cardOutcomes() []behaviourOption {
	out := make([]behaviourOption, 0, len(subscribe.Outcomes))
	for _, outcome := range subscribe.Outcomes {
		out = append(out, behaviourOption{
			Value:   string(outcome),
			Label:   outcome.Label(),
			Failing: outcome != subscribe.OutcomeSuccess,
		})
	}
	return out
}

// The two ways a card reaches the table. A card an integration tokenized is
// left as the column's default; only the console names itself. The values come
// from the domain so the column and the behaviour it drives cannot drift.
const (
	sourceAPI     = string(subscribe.SourceAPI)
	sourceConsole = string(subscribe.SourceConsole)
)

var cardSelect = `
	SELECT c.id, c.sandbox_id, s.slug, c.token, c.number_full, c.expire,
	       c.recurrent, c.verify, coalesce(c.phone, ''), c.balance, c.removed,
	       c.outcome, c.source, coalesce(s.merchant_group, ''),
	       ` + stamp("c.created_at") + `,
	       c.sms_enabled, c.frozen, c.delay_ms,
	       coalesce(c.account::text, ''), coalesce(c.customer, ''),
	       c.verify_code_sent_at, c.registered_at
	FROM mock.cards c
	JOIN control.sandboxes s ON s.id = c.sandbox_id`

// cardFilter is what the cards screen's filter form submits.
type cardFilter struct {
	Sandbox string
	// Outcome narrows to one behaviour, or to every failing one at once:
	// "which of these refuses" is the question the screen is opened with.
	Outcome string
	Query   string
	Sort    string
}

// filterFailing selects every card rigged to refuse, whatever it refuses with.
const filterFailing = "failing"

// empty reports a filter that narrows nothing.
func (f cardFilter) empty() bool {
	return f.Sandbox == "" && f.Outcome == "" && f.Query == ""
}

// Cards lists one half of the stand's cards: either the ones an operator added
// or the ones an integration tokenized. The screen keeps the two apart because
// they answer different questions — what was rigged, and what the register
// actually holds — and a single list mixes them.
func (s *store) Cards(ctx context.Context, tab string, f cardFilter, page pageRequest) ([]cardRow, error) {
	rows, err := s.pool.Query(ctx, cardSelect+`
		WHERE `+tabScope+`
		  AND ($2 = '' OR s.slug = $2)
		  AND CASE
		        WHEN $3 = '' THEN TRUE
		        WHEN $3 = '`+filterFailing+`' THEN c.outcome <> 'success'
		        ELSE c.outcome = $3
		      END
		  AND ($4 = '' OR c.number_full ILIKE '%' || $4 || '%'
		       OR c.token = $4)
		ORDER BY
		  CASE WHEN $6 = '`+sortLargest+`' THEN c.balance END DESC,
		  CASE WHEN $6 = '`+sortOldest+`' THEN c.id END ASC,
		  c.id DESC
		LIMIT $5 OFFSET $7`, tab, f.Sandbox, f.Outcome, f.Query, page.Limit, f.Sort, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("select cards: %w", err)
	}

	return pgx.CollectRows(rows, scanCardRow)
}

// CardCounts returns how many cards each half holds, so a tab can say what is
// behind it without loading the list nobody is looking at.
func (s *store) CardCounts(ctx context.Context) (mocks, cashbox int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE source = '`+sourceConsole+`'),
		       count(*) FILTER (WHERE registered_at > 0)
		FROM mock.cards`).Scan(&mocks, &cashbox)
	if err != nil {
		return 0, 0, fmt.Errorf("count cards: %w", err)
	}

	return mocks, cashbox, nil
}

// The tabs the cards screen is split into, and the source each stands for.
const (
	tabMocks   = "mocks"
	tabCashbox = "cashbox"
)

// cardTab reads which half the screen is showing. Anything unrecognized falls
// back to the mocks, which is the half an operator came to the screen for.
func cardTab(r *http.Request) string {
	if r.URL.Query().Get("tab") == tabCashbox {
		return tabCashbox
	}
	return tabMocks
}

// tabScope is what each tab means in SQL.
//
// The mocks are what an operator added. The cashbox is what an integration has
// tokenized, which is not the same as what it created: a card the operator had
// already put on the stand is handed back rather than copied, so the row still
// says 'console' while the register is holding it. Reading the tab off the
// source left that half of the screen empty however many cards were registered.
// A card can be in both, and that is the truth about it.
const tabScope = `
	CASE $1
	  WHEN '` + tabCashbox + `' THEN c.registered_at > 0
	  ELSE c.source = '` + sourceConsole + `'
	END`

// CardByID returns one card, which is what its own page is built from.
func (s *store) CardByID(ctx context.Context, id int64) (cardRow, error) {
	rows, err := s.pool.Query(ctx, cardSelect+` WHERE c.id = $1`, id)
	if err != nil {
		return cardRow{}, fmt.Errorf("select card: %w", err)
	}

	out, err := pgx.CollectOneRow(rows, scanCardRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return cardRow{}, errNoRow
	}
	if err != nil {
		return cardRow{}, fmt.Errorf("select card: %w", err)
	}

	return out, nil
}

func scanCardRow(row pgx.CollectableRow) (cardRow, error) {
	var (
		out     cardRow
		delayMs int64
	)

	if err := row.Scan(&out.ID, &out.SandboxID, &out.Sandbox, &out.Token,
		&out.Number, &out.Expire, &out.Recurrent, &out.Verify, &out.Phone,
		&out.Balance, &out.Removed, &out.Outcome, &out.Source, &out.Merchant,
		&out.Created, &out.SMSEnabled, &out.Frozen, &delayMs,
		&out.Account, &out.Customer, &out.CodeSentAt,
		&out.RegisteredAt); err != nil {
		return cardRow{}, err
	}

	out.Delay = float64(delayMs) / 1000

	out.decorate()

	return out, nil
}

// decorate fills the fields a screen reads but the table does not store.
func (c *cardRow) decorate() {
	outcome := subscribe.Outcome(c.Outcome)

	c.Mask = subscribe.MaskNumber(c.Number)
	c.System = string(subscribe.DetectSystem(c.Number))
	c.Behaviour = outcome.Label()
	c.Fails = outcome != subscribe.OutcomeSuccess
	c.Added = c.Source == sourceConsole
	c.Registered = c.RegisteredAt > 0

	// A card the console added takes the shared code; one an integration
	// tokenized takes its own expiry. The screen says which so nobody types the
	// wrong six digits at a card that will refuse them.
	if c.Added {
		c.OTP = subscribe.DefaultVerifyCode
	} else {
		c.OTP = subscribe.ExpiryCode(c.Expire)
	}
}

// CreateCard adds a card to a stand. The token is generated the way the
// Subscribe API generates one, so nothing downstream can tell the two apart.
func (s *store) CreateCard(ctx context.Context, c newCard) (int64, error) {
	if err := c.validate(); err != nil {
		return 0, err
	}

	// A number nobody on the stand has yet is checked for real.
	//
	// Every number the provider publishes for its own sandbox passes Luhn,
	// including the two on no Uzbek network at all, and so does anything a real
	// terminal would read off a card. A made-up number that fails it is a typo
	// far more often than it is a deliberate case, and it is worth catching here
	// rather than three screens later when a payment nobody expected to fail
	// does. A number already on the stand is left alone: it was accepted once,
	// and whatever it stands for is being rehearsed against it now.
	known, err := s.CardExists(ctx, c.Number)
	if err != nil {
		return 0, err
	}
	if !known && !luhnValid(c.Number) {
		return 0, errNotACardNumber
	}

	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO mock.cards
			(sandbox_id, token, number_full, expire, recurrent, verify, phone,
			 balance, outcome, verify_wait_ms, source, sms_enabled, frozen, delay_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id`,
		c.SandboxID, infrastructure.NewTokens().CardToken(), c.Number,
		subscribe.FormatExpire(c.Expire), c.Recurrent, c.Verify,
		nullableText(c.Phone), c.Balance, string(c.Outcome),
		subscribe.DefaultVerifyWaitMillis, sourceConsole,
		c.SMSEnabled, c.Frozen, c.DelayMs).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return 0, errCardHeld
	}
	if err != nil {
		return 0, fmt.Errorf("insert card: %w", err)
	}

	// The token belongs to a register, not to the card, and a card with no token
	// anywhere could not be charged at all: the Subscribe API finds a card by the
	// token the caller sends.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO mock.card_tokens (card_id, sandbox_id, token)
		SELECT id, sandbox_id, token FROM mock.cards WHERE id = $1`, id); err != nil {
		return 0, fmt.Errorf("issue card token: %w", err)
	}

	return id, nil
}

// CardExists reports a number the stand already holds, on any of its registers
// and whoever put it there.
func (s *store) CardExists(ctx context.Context, number string) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM mock.cards WHERE number_full = $1)`,
		number).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("look up card number: %w", err)
	}

	return found, nil
}

// luhnValid reports a number whose check digit adds up: double every second
// digit from the right, subtract nine from anything over nine, and the total is
// a multiple of ten. It is what a card issuer computes and what a terminal
// checks before it dials anywhere.
func luhnValid(number string) bool {
	sum := 0
	double := false

	for i := len(number) - 1; i >= 0; i-- {
		digit := int(number[i] - '0')
		if digit < 0 || digit > 9 {
			return false
		}

		if double {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}

		sum += digit
		double = !double
	}

	return sum%10 == 0
}

// errNotACardNumber reports sixteen digits that no card could carry.
var errNotACardNumber = errors.New(
	"that number fails the Luhn check, so no card carries it — check the digits, " +
		"or use the generator, which only makes numbers that check out")

// errCardHeld reports a number the merchant already has.
//
// A card is one card: one balance, one behaviour, one verification. Adding the
// same number to a second register of the same merchant would split all three
// between two rows that stand for the same piece of plastic, so the console
// sends the operator to the card that is already there instead.
var errCardHeld = errors.New("this merchant already holds that card; open it to change how it behaves")

// errNotAMock reports an edit aimed at a card the register tokenized for
// itself. Those are the integration's own: rewriting one's balance or expiry
// would be the console lying to the system under test about what it holds.
// Blocking one is the exception — refusing a card is something a real bank
// does, so the console may do it too.
var errNotAMock = errors.New("that card belongs to the register; it can only be blocked")

// UpdateCard changes what a mock card holds and what it does. The number and
// the token are not editable: a token an integration already stored must keep
// standing for the same card.
func (s *store) UpdateCard(ctx context.Context, id int64, c newCard) error {
	if !c.Outcome.Valid() {
		return fmt.Errorf("unknown card behaviour %q", c.Outcome)
	}
	if c.Balance < 0 {
		return errNegativeBalance
	}
	if c.DelayMs < 0 {
		return fmt.Errorf("a delay cannot run backwards")
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE mock.cards
		SET balance = $1, verify = $2, recurrent = $3, phone = $4, outcome = $5,
		    sms_enabled = $6, frozen = $7, delay_ms = $8
		WHERE id = $9 AND source = $10`,
		c.Balance, c.Verify, c.Recurrent, nullableText(c.Phone),
		string(c.Outcome), c.SMSEnabled, c.Frozen, c.DelayMs, id, sourceConsole)
	if err != nil {
		return fmt.Errorf("update card: %w", err)
	}

	// Nothing updated means either the row is gone or it is not a mock. The
	// two are told apart rather than reported as one, because "that is gone"
	// sent about a card plainly on the screen reads as a bug.
	if tag.RowsAffected() == 0 {
		return s.missingOrNotAMock(ctx, id)
	}

	return nil
}

// SetCardBlocked blocks or releases a card. It is the one edit a card the
// register tokenized will take, so it is a statement of its own rather than a
// field of the edit form.
func (s *store) SetCardBlocked(ctx context.Context, id int64, blocked bool) error {
	outcome := subscribe.OutcomeSuccess
	if blocked {
		outcome = subscribe.OutcomeBlocked
	}

	// Releasing a rigged mock card would silently throw its behaviour away, so
	// only a blocked card is released; anything else keeps what it was given.
	tag, err := s.pool.Exec(ctx, `
		UPDATE mock.cards SET outcome = $1
		WHERE id = $2 AND ($3 OR outcome = $4)`,
		string(outcome), id, blocked, string(subscribe.OutcomeBlocked))
	if err != nil {
		return fmt.Errorf("block card: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	return nil
}

// missingOrNotAMock says why an update changed nothing.
func (s *store) missingOrNotAMock(ctx context.Context, id int64) error {
	var source string

	err := s.pool.QueryRow(ctx, `SELECT source FROM mock.cards WHERE id = $1`, id).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return fmt.Errorf("read card source: %w", err)
	}

	return errNotAMock
}

// DeleteCard removes a mock card outright. Removing one through the API only marks
// it removed, which is a different thing: the token stays known and answers
// with a refusal, and rehearsing that needs the row to survive.
func (s *store) DeleteCard(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM mock.cards WHERE id = $1 AND source = $2`, id, sourceConsole)
	if err != nil {
		return fmt.Errorf("delete card: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return s.missingOrNotAMock(ctx, id)
	}

	return nil
}

// validate rejects a card the schema would take but the emulator could not
// use: the provider only ever sees sixteen digits and an MMYY expiry.
func (c *newCard) validate() error {
	if c.SandboxID == 0 {
		return fmt.Errorf("pick the sandbox the card belongs to")
	}
	if !validCardNumber(c.Number) {
		return fmt.Errorf("a card number is sixteen digits")
	}
	if !validExpire(c.Expire) {
		return fmt.Errorf("an expiry is four digits, MMYY")
	}
	if c.Balance < 0 {
		return errNegativeBalance
	}
	if !c.Outcome.Valid() {
		return fmt.Errorf("unknown card behaviour %q", c.Outcome)
	}

	return nil
}

// validCardNumber reports a sixteen-digit number.
//
// The network is not checked. The provider's own sandbox hands out numbers on
// no Uzbek network at all to stand for cards the processing side rejects, and
// refusing to enter one would put the documented set out of reach.
func validCardNumber(number string) bool {
	return len(number) == 16 && digits(number)
}

// validExpire reports an MMYY expiry with a month that exists. The year is not
// checked against today: a card that expired last year is exactly what someone
// rehearsing a refusal wants to enter.
func validExpire(expire string) bool {
	if len(expire) != 4 || !digits(expire) {
		return false
	}

	month, err := strconv.Atoi(expire[:2])
	if err != nil {
		return false
	}

	return month >= 1 && month <= 12
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ---------- screens ----------

func (a *app) showCards(w http.ResponseWriter, r *http.Request, user string) {
	a.renderCards(w, r, user, "")
}

// renderCards draws the list together with the form that adds one, so rigging
// a refusal is a single step from the screen that shows what is rigged.
func (a *app) renderCards(w http.ResponseWriter, r *http.Request, user, message string) {
	tab := cardTab(r)

	filter := cardFilter{
		Sandbox: r.URL.Query().Get("sandbox"),
		Outcome: r.URL.Query().Get("outcome"),
		Query:   strings.TrimSpace(r.URL.Query().Get("q")),
		Sort:    sortOrder(r),
	}

	page := pageOf(r)

	cards, err := a.store.Cards(r.Context(), tab, filter, page)
	if err != nil {
		a.fail(w, "list cards", err)
		return
	}

	cards, pager := paginate(cards, page, r)

	mocks, cashbox, err := a.store.CardCounts(r.Context())
	if err != nil {
		a.fail(w, "count cards", err)
		return
	}

	// What has just happened to the stand's cards sits above the list of every
	// card there has ever been, so watching a rehearsal and acting on it are
	// the same screen.
	minutes := liveWindow(r)

	var live []liveCardRow

	// A filtered screen shows no live panel, for the same reason the payments
	// screen does not: a narrowed list is a question about what happened, and
	// a panel moving above it answers a different one.
	if filter.empty() {
		live, err = a.store.LiveCards(r.Context(), filter.Sandbox, minutes, listLimit)
		if err != nil {
			a.fail(w, "list live cards", err)
			return
		}
	}

	sandboxes, err := a.store.Sandboxes(r.Context(), a.cfg.GatewayBaseURL)
	if err != nil {
		a.fail(w, "list sandboxes", err)
		return
	}

	a.render(w, "cards", view{
		Title: "Cards", Nav: "cards", User: user, Error: message, Notice: notice(r),
		Cards: cards, Sandboxes: sandboxes, Outcomes: cardOutcomes(),
		Tab: tab, MockCount: mocks, CashboxCount: cashbox,
		CardFilter: filter, Sorts: listSorts(), FilterOutcomes: cardFilterOutcomes(),
		LiveCards: live, Window: minutes, Windows: windowChoices(),
		LiveOn: filter.empty(), Page: pager,
	})
}

func (a *app) showCard(w http.ResponseWriter, r *http.Request, user string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	card, err := a.store.CardByID(r.Context(), id)
	if errors.Is(err, errNoRow) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, "load card", err)
		return
	}

	activity, err := a.store.CardActivity(r.Context(), id)
	if err != nil {
		a.fail(w, "count card activity", err)
		return
	}

	receipts, err := a.store.ReceiptsForCard(r.Context(), id, cardReceiptLimit)
	if err != nil {
		a.fail(w, "list card receipts", err)
		return
	}

	cashboxes, err := a.store.CardCashboxes(r.Context(), id)
	if err != nil {
		a.fail(w, "list card cashboxes", err)
		return
	}

	a.render(w, "card", view{
		Title: card.Mask, Nav: "cards", User: user, Notice: notice(r),
		Card: card, Outcomes: cardOutcomes(), Activity: activity,
		Receipts: receipts, Cashboxes: cashboxes,
	})
}

func (a *app) createCard(w http.ResponseWriter, r *http.Request, user string) {
	if err := r.ParseForm(); err != nil {
		a.renderCards(w, r, user, "Malformed form.")
		return
	}

	card, err := parseCardForm(r)
	if err != nil {
		a.renderCards(w, r, user, err.Error())
		return
	}

	id, err := a.store.CreateCard(r.Context(), card)
	if err != nil {
		a.renderCards(w, r, user, err.Error())
		return
	}

	a.log.Info("card created", "id", id, "outcome", card.Outcome, "by", user)

	// Redirecting after the write keeps a refresh from adding a second card.
	done(w, r, "/cards", "Card added.")
}

// setCardCashbox takes a card off one register or puts it back. It is the
// merchant declining a card at one till, not the bank stopping it.
func (a *app) setCardCashbox(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderCards)
	if !ok {
		return
	}

	sandboxID, err := strconv.ParseInt(r.PostFormValue("sandbox_id"), 10, 64)
	if err != nil {
		a.renderCards(w, r, user, "Unknown cashbox.")
		return
	}

	blocked := r.PostFormValue("blocked") == "1"

	if err := a.store.SetCardCashbox(r.Context(), id, sandboxID, blocked); err != nil {
		a.fail(w, "set card cashbox", err)
		return
	}

	a.log.Info("card cashbox changed", "card", id, "cashbox", sandboxID,
		"blocked", blocked, "by", user)

	message := "Card is taken back at that cashbox."
	if blocked {
		message = "Card is off that cashbox."
	}

	done(w, r, backTo(r, "/cards/"+strconv.FormatInt(id, 10)), message)
}

func (a *app) editCard(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderCards)
	if !ok {
		return
	}

	card, err := parseCardEdit(r)
	if err != nil {
		a.renderCards(w, r, user, err.Error())
		return
	}

	if err := a.store.UpdateCard(r.Context(), id, card); err != nil {
		if errors.Is(err, errNotAMock) {
			a.renderCards(w, r, user, err.Error())
			return
		}
		a.finish(w, r, user, "/cards", "update card", err, a.renderCards)
		return
	}

	a.log.Info("card updated", "id", id, "outcome", card.Outcome, "by", user)
	done(w, r, backTo(r, "/cards"), "Card updated.")
}

func (a *app) deleteCard(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderCards)
	if !ok {
		return
	}

	if err := a.store.DeleteCard(r.Context(), id); err != nil {
		if errors.Is(err, errNotAMock) {
			a.renderCards(w, r, user, err.Error())
			return
		}
		a.finish(w, r, user, "/cards", "delete card", err, a.renderCards)
		return
	}

	a.log.Info("card deleted", "id", id, "by", user)
	done(w, r, "/cards", "Card deleted.")
}

// blockCard is the one edit every card takes, whoever put it there.
func (a *app) blockCard(w http.ResponseWriter, r *http.Request, user string) {
	id, ok := a.pathID(w, r, user, a.renderCards)
	if !ok {
		return
	}

	blocked := r.PostFormValue("blocked") != ""

	if err := a.store.SetCardBlocked(r.Context(), id, blocked); err != nil {
		a.finish(w, r, user, "/cards", "block card", err, a.renderCards)
		return
	}

	a.log.Info("card blocked", "id", id, "blocked", blocked, "by", user)

	if blocked {
		done(w, r, backTo(r, "/cards"), "Card blocked. It now refuses every operation.")
		return
	}
	done(w, r, backTo(r, "/cards"), "Card released.")
}

// seedPaymeCards adds the provider's own documented sandbox cards to a stand,
// so an integration can be run against the numbers its authors already know.
func (a *app) seedPaymeCards(w http.ResponseWriter, r *http.Request, user string) {
	if err := r.ParseForm(); err != nil {
		a.renderCards(w, r, user, "Malformed form.")
		return
	}

	sandboxID, err := strconv.ParseInt(r.PostFormValue("sandbox_id"), 10, 64)
	if err != nil {
		a.renderCards(w, r, user, "Pick the sandbox the cards belong to.")
		return
	}

	added, err := a.store.SeedPaymeCards(r.Context(), sandboxID)
	if err != nil {
		a.renderCards(w, r, user, err.Error())
		return
	}

	a.log.Info("payme test cards seeded", "sandbox", sandboxID, "added", added, "by", user)

	if added == 0 {
		done(w, r, "/cards", "That sandbox already has the whole set.")
		return
	}
	done(w, r, "/cards", fmt.Sprintf("Added %d of the provider's test cards.", added))
}

// parseCardForm reads the create form. Spaces are allowed in the number
// because that is how a card is written down.
func parseCardForm(r *http.Request) (newCard, error) {
	sandboxID, err := strconv.ParseInt(r.PostFormValue("sandbox_id"), 10, 64)
	if err != nil {
		return newCard{}, fmt.Errorf("pick the sandbox the card belongs to")
	}

	card, err := parseCardEdit(r)
	if err != nil {
		return newCard{}, err
	}

	card.SandboxID = sandboxID
	card.Number = strings.ReplaceAll(r.PostFormValue("number"), " ", "")
	card.Expire = strings.ReplaceAll(r.PostFormValue("expire"), "/", "")

	return card, card.validate()
}

// parseCardEdit reads the fields both forms share.
func parseCardEdit(r *http.Request) (newCard, error) {
	balance, err := strconv.ParseInt(orZero(r.PostFormValue("balance")), 10, 64)
	if err != nil || balance < 0 {
		return newCard{}, fmt.Errorf("balance must be a whole number of tiyin, zero or more")
	}

	outcome := subscribe.Outcome(r.PostFormValue("outcome"))
	if outcome == "" {
		outcome = subscribe.OutcomeSuccess
	}
	if !outcome.Valid() {
		return newCard{}, fmt.Errorf("unknown card behaviour %q", outcome)
	}

	// The form asks for seconds because that is the unit a stall is described
	// in; the column holds milliseconds.
	delay, err := strconv.ParseFloat(orZero(r.PostFormValue("delay")), 64)
	if err != nil || delay < 0 {
		return newCard{}, fmt.Errorf("a delay is a number of seconds, zero or more")
	}

	return newCard{
		Balance:    balance,
		Outcome:    outcome,
		Phone:      r.PostFormValue("phone"),
		Verify:     r.PostFormValue("verify") != "",
		Recurrent:  r.PostFormValue("recurrent") != "",
		SMSEnabled: r.PostFormValue("sms_enabled") != "",
		Frozen:     r.PostFormValue("frozen") != "",
		DelayMs:    int64(delay * 1000),
	}, nil
}
