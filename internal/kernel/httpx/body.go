package httpx

import (
	"bytes"
	"io"
	"net/http"
)

// maxReadBytes caps how much of a body the middleware buffers for rule
// matching. Payme's requests are small; anything larger is not worth holding
// in memory to decide whether a rule applies.
const maxReadBytes = 1 << 20 // 1 MiB

// readAndRestoreBody reads a request body for inspection and puts it back, so
// the handler downstream still sees a complete, readable body.
func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReadBytes))
	if err != nil {
		return nil, err
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}
