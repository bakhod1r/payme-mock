package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pageRequestFor(query string) pageRequest {
	return pageOf(httptest.NewRequest(http.MethodGet, "/traffic"+query, nil))
}

// The page and its size both come off the URL, and neither is trusted: a
// hand-edited one is not worth an error screen, and an unbounded size would be
// a way to ask the database for the whole log at once.
func TestPageOf(t *testing.T) {
	tests := []struct {
		query  string
		number int
		size   int
		offset int
	}{
		{"", 1, 50, 0},
		{"?page=3", 3, 50, 100},
		{"?page=0", 1, 50, 0},
		{"?page=-2", 1, 50, 0},
		{"?page=nonsense", 1, 50, 0},
		{"?per=25", 1, 25, 0},
		{"?per=200&page=2", 2, 200, 200},
		{"?per=9999", 1, 50, 0},
		{"?per=nonsense", 1, 50, 0},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := pageRequestFor(tt.query)

			assert.Equal(t, tt.number, got.Number)
			assert.Equal(t, tt.size, got.Size)
			assert.Equal(t, tt.offset, got.Offset)
			assert.Equal(t, tt.size+1, got.Limit, "one row over the page is how the next one is spotted")
		})
	}
}

// The extra row is how the screen knows there is more; it is never shown.
func TestPaginateTrimsTheProbeRow(t *testing.T) {
	rows := make([]int, 51)
	page := pageRequestFor("")

	got, view := paginate(rows, page, httptest.NewRequest(http.MethodGet, "/traffic", nil))

	require.Len(t, got, 50)
	assert.True(t, view.HasNext)
	assert.False(t, view.HasPrev)
	assert.Equal(t, 1, view.From)
	assert.Equal(t, 50, view.To)
}

func TestPaginateOnTheLastPage(t *testing.T) {
	rows := make([]int, 10)
	page := pageRequestFor("?page=2")

	got, view := paginate(rows, page, httptest.NewRequest(http.MethodGet, "/traffic?page=2", nil))

	assert.Len(t, got, 10)
	assert.False(t, view.HasNext)
	assert.True(t, view.HasPrev)
	assert.Equal(t, 51, view.From)
	assert.Equal(t, 60, view.To)
}

// An empty page says so rather than claiming to start at row one.
func TestPaginateWithNoRows(t *testing.T) {
	got, view := paginate([]int{}, pageRequestFor(""),
		httptest.NewRequest(http.MethodGet, "/traffic", nil))

	assert.Empty(t, got)
	assert.False(t, view.Rows)
	assert.Zero(t, view.From)
	assert.Zero(t, view.To)
}

// Paging and resizing keep every other parameter, so neither quietly widens
// what is being read.
func TestPagerURLsKeepTheFilter(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/traffic?service=merchant&q=cards&page=2", nil)
	_, view := paginate(make([]int, 51), pageOf(r), r)

	assert.Contains(t, view.NextURL, "service=merchant")
	assert.Contains(t, view.NextURL, "q=cards")
	assert.Contains(t, view.NextURL, "page=3")

	assert.Contains(t, view.PrevURL, "service=merchant")
	// Page one is the address without a page at all.
	assert.NotContains(t, view.PrevURL, "page=")

	// Changing the size starts again from the top: page four of a fifty-row
	// list is nowhere in particular once the rows are two hundred.
	for _, size := range view.Sizes {
		assert.Contains(t, size.URL, "service=merchant")
		assert.NotContains(t, size.URL, "page=")

		if size.Value == defaultPageSize {
			assert.NotContains(t, size.URL, "per=", "the default size needs no parameter")
			assert.True(t, size.On)
		}
	}
}
