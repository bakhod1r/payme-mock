package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The window is what makes the screen live. A hand-edited one is not worth an
// error screen, so anything unreadable falls back to the default.
func TestLiveWindow(t *testing.T) {
	tests := []struct {
		query string
		want  int
	}{
		{"", defaultWindowMinutes},
		{"?window=5", 5},
		{"?window=60", 60},
		{"?window=0", defaultWindowMinutes},
		{"?window=-3", defaultWindowMinutes},
		{"?window=abc", defaultWindowMinutes},
		// A window wider than a day is capped rather than run as asked: the
		// screen is a tail of what just happened, not an archive.
		{"?window=100000", 24 * 60},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got := liveWindow(httptest.NewRequest(http.MethodGet, "/live"+tt.query, nil))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHumanAgo(t *testing.T) {
	assert.Equal(t, "just now", humanAgo(-1))
	assert.Equal(t, "0s ago", humanAgo(0))
	assert.Equal(t, "59s ago", humanAgo(59))
	assert.Equal(t, "1m ago", humanAgo(60))
	assert.Equal(t, "59m ago", humanAgo(3599))
	assert.Equal(t, "1h ago", humanAgo(3600))
	assert.Equal(t, "25h ago", humanAgo(90000))
}

// A timestamp the screen cannot read says nothing rather than lying about it.
func TestAgoSinceRejectsJunk(t *testing.T) {
	assert.Empty(t, agoSince("not-a-timestamp"))
	assert.NotEmpty(t, agoSince("2020-01-01 00:00:00"))
}

// Every window the screen offers has to be one the parser accepts, or a tab
// would silently draw a different window from the one it names.
func TestWindowChoicesAreAccepted(t *testing.T) {
	for _, minutes := range windowChoices() {
		r := httptest.NewRequest(http.MethodGet, "/live?window="+strconv.Itoa(minutes), nil)
		assert.Equal(t, minutes, liveWindow(r))
	}
}
