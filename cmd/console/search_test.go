package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A pasted identifier usually arrives with a space on one end, and a search
// that fails on that looks broken rather than empty.
func TestSearchQuery(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"", ""},
		{"?q=acme", "acme"},
		{"?q=%20%20acme%20%20", "acme"},
		{"?q=%20", ""},
		{"?q=8600+0691", "8600 0691"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := searchQuery(httptest.NewRequest(http.MethodGet, "/orders"+tt.query, nil))
			assert.Equal(t, tt.want, got)
		})
	}
}
