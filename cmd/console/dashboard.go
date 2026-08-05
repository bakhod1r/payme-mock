package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	billing "github.com/bakhod1r/payme-mock/internal/context/payment/billing/domain"
)

// The stand answers one question all day — is the integration behaving — and
// answering it meant opening four screens and counting rows by eye. This is
// that count, done once: what money moved, how payments ended, and what the
// traffic looked like while it happened.

// dashboard is everything the front screen shows.
type dashboard struct {
	Cashboxes []cashboxSummary
	Money     moneySummary
	Endings   endingSummary
	Cards     cardSummary
	Traffic   trafficSummary
	Failures  []failureRow
}

// cashboxSummary is one register with what it holds and what went through it.
type cashboxSummary struct {
	ID           int64
	Slug         string
	Kind         string
	KindMeaning  string
	MerchantName string
	Balance      int64
	Blocked      bool
	// TopUps and Withdrawals are settled receipts, counted per direction, and
	// In is what is still moving.
	TopUps      int
	Withdrawals int
	InFlight    int
}

// moneySummary is what actually moved, which is not the same as what was asked
// for: only a settled receipt moved money.
type moneySummary struct {
	TopUpCount  int
	TopUpSum    int64
	PayoutCount int
	PayoutSum   int64
	// Net is what the registers gained: top-ups in, withdrawals out.
	Net int64
}

// endingSummary is how payments ended, across both sides of the stand.
type endingSummary struct {
	Settled   int
	InFlight  int
	Cancelled int
	// Total is every payment the stand holds, which is what the three are read
	// against.
	Total int
}

// cardSummary counts the cards a stand can pay with, and the ones rigged to
// refuse — the second number is the point of a stand.
type cardSummary struct {
	Total    int
	Rigged   int
	Verified int
	Removed  int
}

// trafficSummary is the last hour of calls: how many, how many failed, and how
// long they took.
type trafficSummary struct {
	Calls int
	// Failed counts a transport failure, Errors a protocol error returned
	// inside a 200. They are different faults and a stand that conflates them
	// hides the commoner one.
	Failed  int
	Errors  int
	AvgMS   int
	SlowMS  int
	Methods []methodCount
}

// methodCount is one method with how often it was called in the window.
type methodCount struct {
	Method string
	Calls  int
}

// failureRow is one recent call that did not come back clean, which is what
// anyone opening the stand is looking for.
type failureRow struct {
	ID        int64
	At        string
	Sandbox   string
	Method    string
	Status    int
	ErrorCode int
}

// The three colours a payment's state is drawn in. They are not the console's
// --ok/--warn/--bad, which are tuned for text on the panel and sit too light to
// read as filled areas on it: every one of them falls outside the OKLCH
// lightness band a dark surface wants. These are the same three meanings
// stepped down into that band, and the trio was checked rather than eyeballed —
// lightness, chroma, contrast against the panel, and separation under
// deuteranopia and protanopia all pass.
//
// The worst adjacent pair still sits near the colour-vision floor, so colour is
// never the only encoding: every segment carries a direct label, and the
// segments are separated by a surface-coloured gap.
const (
	chartSettled   = "#22a074"
	chartInFlight  = "#ad8c00"
	chartCancelled = "#e04566"
)

// chartSegment is one part of a whole, as a proportion bar draws it.
type chartSegment struct {
	Label string
	Count int
	// Pct is the share of the whole, which is the segment's width.
	Pct   float64
	Color string
}

// chartBar is one row of a horizontal bar chart, measured against the largest
// row rather than against a total: the question there is which is biggest, not
// what share each holds.
type chartBar struct {
	Label string
	Count int
	Pct   float64
}

// Segments turns the three endings into a proportion bar.
//
// It returns nothing at all when there is nothing to draw. A chart of zero
// payments would be an empty rail that reads as a rendering fault, and the
// screen says "no payments yet" in words instead.
func (e endingSummary) Segments() []chartSegment {
	if e.Total <= 0 {
		return nil
	}

	all := []chartSegment{
		{Label: "settled", Count: e.Settled, Color: chartSettled},
		{Label: "in progress", Count: e.InFlight, Color: chartInFlight},
		{Label: "cancelled", Count: e.Cancelled, Color: chartCancelled},
	}

	// An ending nothing landed on is dropped rather than drawn at zero width:
	// a segment too thin to see still takes a gap and a legend entry, and three
	// of those make a bar that cannot be read.
	out := make([]chartSegment, 0, len(all))
	for _, seg := range all {
		if seg.Count == 0 {
			continue
		}
		seg.Pct = float64(seg.Count) * 100 / float64(e.Total)
		out = append(out, seg)
	}

	return out
}

// Bars turns the method counts into a horizontal bar chart, longest first.
//
// The counts arrive ordered already; what this adds is the width of each bar
// against the busiest method, so the shape of the traffic is readable without
// comparing numbers.
func (t trafficSummary) Bars() []chartBar {
	if len(t.Methods) == 0 {
		return nil
	}

	most := 0
	for _, m := range t.Methods {
		if m.Calls > most {
			most = m.Calls
		}
	}
	if most == 0 {
		return nil
	}

	out := make([]chartBar, 0, len(t.Methods))
	for _, m := range t.Methods {
		out = append(out, chartBar{
			Label: m.Method,
			Count: m.Calls,
			Pct:   float64(m.Calls) * 100 / float64(most),
		})
	}

	return out
}

// Activity is the cashbox's own payments as a proportion bar, so a row in the
// table is comparable at a glance with the row above it.
func (c cashboxSummary) Activity() []chartSegment {
	return endingSummary{
		Settled:  c.TopUps + c.Withdrawals,
		InFlight: c.InFlight,
		Total:    c.TopUps + c.Withdrawals + c.InFlight,
	}.Segments()
}

// dashboardWindowMinutes is how far back the traffic figures look. An hour is
// long enough to cover a rehearsal and short enough that yesterday's run does
// not colour today's.
const dashboardWindowMinutes = 60

// failureLimit caps the recent-failure list. It is a pointer to the log, not a
// replacement for it.
const failureLimit = 8

// Dashboard collects every figure the front screen shows.
func (s *store) Dashboard(ctx context.Context) (dashboard, error) {
	var out dashboard

	cashboxes, err := s.cashboxSummaries(ctx)
	if err != nil {
		return dashboard{}, err
	}
	out.Cashboxes = cashboxes

	if err := s.moneySummary(ctx, &out); err != nil {
		return dashboard{}, err
	}

	if err := s.cardSummary(ctx, &out.Cards); err != nil {
		return dashboard{}, err
	}

	if err := s.trafficSummary(ctx, &out.Traffic); err != nil {
		return dashboard{}, err
	}

	failures, err := s.recentFailures(ctx)
	if err != nil {
		return dashboard{}, err
	}
	out.Failures = failures

	return out, nil
}

func (s *store) cashboxSummaries(ctx context.Context) ([]cashboxSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.slug, s.kind, coalesce(s.merchant_name, ''),
		       coalesce(a.balance, 0), coalesce(a.blocked, FALSE),
		       (SELECT count(*) FROM mock.receipts r
		         WHERE r.sandbox_id = s.id AND r.state = 4 AND NOT r.payout),
		       (SELECT count(*) FROM mock.receipts r
		         WHERE r.sandbox_id = s.id AND r.state = 4 AND r.payout),
		       (SELECT count(*) FROM mock.receipts r
		         WHERE r.sandbox_id = s.id AND r.state NOT IN (4, 50))
		FROM control.sandboxes s
		LEFT JOIN LATERAL (
			SELECT balance, blocked FROM merchant.accounts
			WHERE sandbox_id = s.id ORDER BY id LIMIT 1
		) a ON TRUE
		WHERE NOT s.archived
		ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("summarise registers: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (cashboxSummary, error) {
		var out cashboxSummary
		if err := row.Scan(&out.ID, &out.Slug, &out.Kind, &out.MerchantName,
			&out.Balance, &out.Blocked, &out.TopUps, &out.Withdrawals,
			&out.InFlight); err != nil {
			return cashboxSummary{}, err
		}

		out.KindMeaning = billing.Kind(out.Kind).Describe()

		return out, nil
	})
}

// moneySummary counts what moved and how payments ended, in one pass over the
// receipts: the two questions read the same rows.
func (s *store) moneySummary(ctx context.Context, out *dashboard) error {
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE state = 4 AND NOT payout),
			coalesce(sum(amount) FILTER (WHERE state = 4 AND NOT payout), 0),
			count(*) FILTER (WHERE state = 4 AND payout),
			coalesce(sum(amount) FILTER (WHERE state = 4 AND payout), 0),
			count(*) FILTER (WHERE state = 4),
			count(*) FILTER (WHERE state NOT IN (4, 50)),
			count(*) FILTER (WHERE state = 50),
			count(*)
		FROM mock.receipts`).
		Scan(&out.Money.TopUpCount, &out.Money.TopUpSum,
			&out.Money.PayoutCount, &out.Money.PayoutSum,
			&out.Endings.Settled, &out.Endings.InFlight,
			&out.Endings.Cancelled, &out.Endings.Total)
	if err != nil {
		return fmt.Errorf("summarise money: %w", err)
	}

	out.Money.Net = out.Money.TopUpSum - out.Money.PayoutSum

	return nil
}

func (s *store) cardSummary(ctx context.Context, out *cardSummary) error {
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE outcome <> 'success'),
		       count(*) FILTER (WHERE verify),
		       count(*) FILTER (WHERE removed)
		FROM mock.cards`).
		Scan(&out.Total, &out.Rigged, &out.Verified, &out.Removed)
	if err != nil {
		return fmt.Errorf("summarise cards: %w", err)
	}

	return nil
}

func (s *store) trafficSummary(ctx context.Context, out *trafficSummary) error {
	window := fmt.Sprintf("%d minutes", dashboardWindowMinutes)

	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE http_status >= 400 OR http_status = 0),
		       count(*) FILTER (WHERE error_code IS NOT NULL),
		       coalesce(round(avg(duration_ms))::int, 0),
		       coalesce(max(duration_ms), 0)
		FROM control.request_log
		WHERE at > now() - $1::interval`, window).
		Scan(&out.Calls, &out.Failed, &out.Errors, &out.AvgMS, &out.SlowMS)
	if err != nil {
		return fmt.Errorf("summarise traffic: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT coalesce(method, '—'), count(*)
		FROM control.request_log
		WHERE at > now() - $1::interval
		GROUP BY 1
		ORDER BY 2 DESC, 1
		LIMIT 6`, window)
	if err != nil {
		return fmt.Errorf("summarise methods: %w", err)
	}

	methods, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (methodCount, error) {
		var out methodCount
		err := row.Scan(&out.Method, &out.Calls)
		return out, err
	})
	if err != nil {
		return fmt.Errorf("read methods: %w", err)
	}

	out.Methods = methods

	return nil
}

// recentFailures lists the last calls that did not come back clean, counting a
// protocol error inside a 200 as a failure — which is how the provider reports
// most of them.
func (s *store) recentFailures(ctx context.Context) ([]failureRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, `+stamp("l.at")+`, coalesce(s.slug, '—'),
		       coalesce(l.method, '—'), l.http_status, coalesce(l.error_code, 0)
		FROM control.request_log l
		LEFT JOIN control.sandboxes s ON s.id = l.sandbox_id
		WHERE l.error_code IS NOT NULL OR l.http_status >= 400 OR l.http_status = 0
		ORDER BY l.id DESC
		LIMIT $1`, failureLimit)
	if err != nil {
		return nil, fmt.Errorf("select recent failures: %w", err)
	}

	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (failureRow, error) {
		var out failureRow
		err := row.Scan(&out.ID, &out.At, &out.Sandbox, &out.Method,
			&out.Status, &out.ErrorCode)
		return out, err
	})
}

func (a *app) showDashboard(w http.ResponseWriter, r *http.Request, user string) {
	data, err := a.store.Dashboard(r.Context())
	if err != nil {
		a.fail(w, "build dashboard", err)
		return
	}

	a.render(w, "dashboard", view{
		Title: "Dashboard", Nav: "dashboard", User: user, Notice: notice(r),
		Dashboard: data, Window: dashboardWindowMinutes,
	})
}
