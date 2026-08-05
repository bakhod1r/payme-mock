// Package infrastructure implements the traffic log against PostgreSQL.
package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bakhod1r/payme-mock/internal/context/simulation/traffic/domain"
	"github.com/bakhod1r/payme-mock/internal/kernel/postgres"
)

// Recorder stores traffic entries.
type Recorder struct {
	pool *postgres.Pool
}

// NewRecorder wires the recorder to a pool.
func NewRecorder(pool *postgres.Pool) *Recorder {
	return &Recorder{pool: pool}
}

// Record appends one call to the log.
//
// The insert runs against the pool rather than any open transaction: a request
// that was rolled back still happened, and its record is the only evidence of
// why it failed.
func (r *Recorder) Record(ctx context.Context, e domain.Entry) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO control.request_log
			(sandbox_id, service, direction, method, rpc_id, http_status,
			 request_body, response_body, duration_ms, fault_rule_id,
			 error_code, remote_addr, request_headers, response_headers)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		e.SandboxID, e.Service, string(e.Direction), nullableText(e.Method),
		nullableText(e.RPCID), nullableInt(e.HTTPStatus), jsonOrNil(e.RequestBody),
		jsonOrNil(e.ResponseBody), e.DurationMS, e.FaultRuleID, e.ErrorCode,
		nullableText(e.RemoteAddr), headersOrNil(e.RequestHeaders),
		headersOrNil(e.ResponseHeaders))
	if err != nil {
		return fmt.Errorf("insert traffic entry: %w", err)
	}

	return nil
}

// headersOrNil keeps an empty header set out of the column, so a record with
// nothing to say about headers reads as unset rather than as an empty object.
func headersOrNil(headers map[string]string) any {
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// jsonOrNil keeps a body that is not valid JSON out of a jsonb column.
//
// Malformed bodies are exactly what this stand exists to produce, so one must
// never cost the record that explains it: the raw text is stored as a JSON
// string instead, which is still readable in the console.
func jsonOrNil(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}

	if json.Valid(body) {
		return body
	}

	// Marshalling a string cannot fail, so the raw text always survives.
	quoted, _ := json.Marshal(string(body))

	return quoted
}

func nullableText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func nullableInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}
