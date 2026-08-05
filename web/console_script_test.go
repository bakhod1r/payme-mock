package web_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bakhod1r/payme-mock/web"
)

// The console's page script is not exercised by any browser here, so the rules
// that keep a form from being destroyed while it is being filled in are held in
// place by reading them. Both were regressions an operator hit: a filter dialog
// that closed the moment a <select> option was chosen, and one that vanished on
// its own because the page reloaded on a timer underneath it.
//
// The assertions are deliberately about the mechanism rather than the wording.
// Anyone removing the press-target check or the busy() guard has to remove a
// test that says why they are there.
func TestLayoutKeepsADialogFromClosingItself(t *testing.T) {
	layout := layoutHTML(t)

	t.Run("an outside click is judged by where the press started", func(t *testing.T) {
		assert.Contains(t, layout, "document.addEventListener('mousedown'",
			"the press target is what tells an outside click from a <select> option")
		assert.Contains(t, layout, "if (pressedOn !== event.target) { return; }",
			"a click whose press landed inside the dialog must not close it")
	})

	t.Run("the auto refresh holds off while the page is in use", func(t *testing.T) {
		require.Contains(t, layout, "function busy()",
			"one place decides when a reload would destroy work")

		for _, guard := range []string{
			"document.hidden",
			"dialog[open]",
			"INPUT|SELECT|TEXTAREA",
			"window.getSelection()",
		} {
			assert.Contains(t, layout, guard,
				"a reload must hold off in this state")
		}

		assert.Contains(t, layout, "if (!box.checked || busy()) { return; }",
			"the reload timer asks busy() before it reloads anything")
	})

	// Reopening a dialog after a reload was tried and taken out again: a submit
	// is a reload too, so the filter dialog came back over the results it had
	// just produced, and every attempt to close it brought it back. Holding the
	// reload off while the dialog is open is the fix; restoring it is not.
	t.Run("nothing reopens a dialog behind the operator", func(t *testing.T) {
		assert.NotContains(t, layout, "pageshow",
			"a dialog reopened by script is a dialog that will not close")
	})
}

// A request body is one line with a 400-character token in it and nothing to
// break at. A grid track sized to that content drags the panel, and then the
// page, wider than the window: the console scrolls sideways and the row an
// operator is reading walks off the screen.
//
// The fix is that the boxes holding a body may shrink below their content and
// scroll on their own, which is a single declaration and exactly the kind that
// gets tidied away by someone who cannot see what it is for.
func TestLayoutKeepsWideBodiesInsideTheirPanel(t *testing.T) {
	layout := layoutHTML(t)

	assert.Contains(t, layout, ".bodies > * { min-width: 0; }",
		"a grid track must be allowed to be narrower than an unbreakable token")
	assert.Contains(t, layout, "overflow-wrap: anywhere",
		"a token in a fact list has to break rather than push the layout out")
}

// The reload is what makes every one of the above necessary, so a screen that
// switches it on says so with a checkbox the operator can reach.
func TestOnlyScreensWithTheCheckboxReloadThemselves(t *testing.T) {
	entries, err := web.Console.ReadDir("console")
	require.NoError(t, err)

	for _, entry := range entries {
		body, err := web.Console.ReadFile("console/" + entry.Name())
		require.NoError(t, err)

		if !strings.Contains(string(body), "window.location.reload()") {
			continue
		}

		assert.Contains(t, string(body), "live-refresh",
			"%s reloads itself with no way to stop it", entry.Name())
	}
}

func layoutHTML(t *testing.T) string {
	t.Helper()

	body, err := web.Console.ReadFile("console/layout.html")
	require.NoError(t, err)

	return string(body)
}
