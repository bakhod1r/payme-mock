// Package application holds the Merchant API use cases: one command or query
// handler per protocol method. Field names and shapes match the documentation
// exactly, so responses are indistinguishable from the real provider's.
package application

import (
	"github.com/bakhod1r/payme-mock/internal/context/payment/merchant/domain"
)

// CheckPerformParams are the parameters of CheckPerformTransaction.
type CheckPerformParams struct {
	Amount  int64             `json:"amount"`
	Account map[string]string `json:"account"`
}

// CheckPerformResult reports whether a payment may proceed.
type CheckPerformResult struct {
	Allow bool `json:"allow"`
	// Detail carries fiscal data when the merchant supplies it.
	Detail any `json:"detail,omitempty"`
	// Additional carries free-form billing data such as a balance.
	Additional map[string]any `json:"additional,omitempty"`
}

// CreateParams are the parameters of CreateTransaction.
type CreateParams struct {
	ID      string            `json:"id"`
	Time    int64             `json:"time"`
	Amount  int64             `json:"amount"`
	Account map[string]string `json:"account"`
}

// CreateResult is the response of CreateTransaction.
type CreateResult struct {
	CreateTime  int64             `json:"create_time"`
	Transaction string            `json:"transaction"`
	State       domain.State      `json:"state"`
	Receivers   []domain.Receiver `json:"receivers,omitempty"`
}

// PerformParams are the parameters of PerformTransaction.
type PerformParams struct {
	ID string `json:"id"`
}

// PerformResult is the response of PerformTransaction.
type PerformResult struct {
	Transaction string       `json:"transaction"`
	PerformTime int64        `json:"perform_time"`
	State       domain.State `json:"state"`
}

// CancelParams are the parameters of CancelTransaction.
type CancelParams struct {
	ID     string        `json:"id"`
	Reason domain.Reason `json:"reason"`
}

// CancelResult is the response of CancelTransaction.
type CancelResult struct {
	Transaction string       `json:"transaction"`
	CancelTime  int64        `json:"cancel_time"`
	State       domain.State `json:"state"`
}

// CheckParams are the parameters of CheckTransaction.
type CheckParams struct {
	ID string `json:"id"`
}

// CheckResult is the response of CheckTransaction. Reason is null rather than
// absent when the transaction was not cancelled, matching the documented example.
type CheckResult struct {
	CreateTime  int64          `json:"create_time"`
	PerformTime int64          `json:"perform_time"`
	CancelTime  int64          `json:"cancel_time"`
	Transaction string         `json:"transaction"`
	State       domain.State   `json:"state"`
	Reason      *domain.Reason `json:"reason"`
}

// StatementParams are the parameters of GetStatement.
type StatementParams struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// StatementResult is the response of GetStatement.
type StatementResult struct {
	Transactions []StatementEntry `json:"transactions"`
}

// StatementEntry is one transaction in a statement, carrying the full record.
type StatementEntry struct {
	ID          string            `json:"id"`
	Time        int64             `json:"time"`
	Amount      int64             `json:"amount"`
	Account     map[string]string `json:"account"`
	CreateTime  int64             `json:"create_time"`
	PerformTime int64             `json:"perform_time"`
	CancelTime  int64             `json:"cancel_time"`
	Transaction string            `json:"transaction"`
	State       domain.State      `json:"state"`
	Reason      *domain.Reason    `json:"reason"`
	Receivers   []domain.Receiver `json:"receivers,omitempty"`
}
