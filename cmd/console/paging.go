package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// A list that stops at two hundred rows is not a list, it is the top of one.
// The lists that grow — the log, the payments, the cards — are paged instead,
// so a stand that has been running all week can still be read to the bottom.

// defaultPageSize is how many rows a page holds unless someone says otherwise:
// a screenful with room to scroll.
const defaultPageSize = 50

// pageSizes are the sizes a reader may pick. Fifty is the middle one; the
// small size is for reading a page carefully and the large ones for scanning a
// stand that has been running all week.
func pageSizes() []int { return []int{25, 50, 100, 200} }

// pageSizeOf reads how many rows were asked for. Anything that is not one of
// the offered sizes is the default rather than a failure — a hand-edited URL is
// not worth an error screen, and an unbounded one would be a way to ask the
// database for the whole log at once.
func pageSizeOf(r *http.Request) int {
	asked, err := strconv.Atoi(r.URL.Query().Get("per"))
	if err != nil {
		return defaultPageSize
	}

	for _, size := range pageSizes() {
		if size == asked {
			return size
		}
	}

	return defaultPageSize
}

// pageRequest is the slice of a list a screen asked for.
type pageRequest struct {
	// Limit is one more than the page holds. The extra row is never shown; it
	// is how the screen knows another page exists without counting the whole
	// table, which on a log of a million rows is the expensive way to ask.
	Limit  int
	Offset int
	Number int
	// Size is how many rows the page holds, which the reader picks.
	Size int
}

// pageView is what the template needs to draw the pager.
type pageView struct {
	Number int
	// HasPrev and HasNext report which way there is more to read.
	HasPrev bool
	HasNext bool
	// PrevURL and NextURL keep every other parameter — the filter, the search,
	// the sort — so paging never quietly widens what is being read.
	PrevURL string
	NextURL string
	// From and To are the row numbers this page covers, which is what tells a
	// reader where they are in something they cannot see the end of.
	From int
	To   int
	// Sizes are the page sizes on offer, each with the address that picks it.
	Sizes []pageSizeOption
	// Rows reports whether the list has anything on it, since a pager under an
	// empty list has nothing to move through.
	Rows bool
}

// pageSizeOption is one entry of the page size picker.
type pageSizeOption struct {
	Value int
	URL   string
	On    bool
}

// pageOf reads which page, and how much of it, a request asked for.
func pageOf(r *http.Request) pageRequest {
	number, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || number < 1 {
		number = 1
	}

	size := pageSizeOf(r)

	return pageRequest{
		Limit:  size + 1,
		Offset: (number - 1) * size,
		Number: number,
		Size:   size,
	}
}

// paginate trims the extra row off a result and describes the pager.
//
// The rows are passed in and back rather than counted by the caller, so a
// screen cannot draw a pager that disagrees with the rows beside it.
func paginate[T any](rows []T, page pageRequest, r *http.Request) ([]T, pageView) {
	view := pageView{
		Number:  page.Number,
		HasPrev: page.Number > 1,
		HasNext: len(rows) > page.Size,
		From:    page.Offset + 1,
	}

	if view.HasNext {
		rows = rows[:page.Size]
	}

	view.To = page.Offset + len(rows)
	if len(rows) == 0 {
		view.From = 0
	}

	if view.HasPrev {
		view.PrevURL = pageURL(r, page.Number-1)
	}
	if view.HasNext {
		view.NextURL = pageURL(r, page.Number+1)
	}

	view.Rows = len(rows) > 0

	// Changing the size starts the list again from the top: page four of a
	// fifty-row list is nowhere in particular once the rows are two hundred.
	for _, size := range pageSizes() {
		view.Sizes = append(view.Sizes, pageSizeOption{
			Value: size,
			URL:   sizeURL(r, size),
			On:    size == page.Size,
		})
	}

	return rows, view
}

// pageURL is this screen's address with the page changed and everything else
// left alone.
func pageURL(r *http.Request, number int) string {
	query := cloneQuery(r.URL.Query())

	if number <= 1 {
		query.Del("page")
	} else {
		query.Set("page", strconv.Itoa(number))
	}

	if len(query) == 0 {
		return r.URL.Path
	}

	return fmt.Sprintf("%s?%s", r.URL.Path, query.Encode())
}

// sizeURL is this screen's address with the page size changed, back at the
// first page and with everything else left alone.
func sizeURL(r *http.Request, size int) string {
	query := cloneQuery(r.URL.Query())
	query.Del("page")

	if size == defaultPageSize {
		query.Del("per")
	} else {
		query.Set("per", strconv.Itoa(size))
	}

	if len(query) == 0 {
		return r.URL.Path
	}

	return fmt.Sprintf("%s?%s", r.URL.Path, query.Encode())
}

// cloneQuery copies the parameters so changing the page cannot alter the
// request the rest of the screen was built from.
func cloneQuery(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}
