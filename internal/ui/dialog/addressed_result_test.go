package dialog

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAddressedResultReachesItsOwnDialog pins the delivery that keeps a
// dialog from wedging. Async results used to go to whichever dialog was on
// top, so a permission prompt raised over a form mid-submit swallowed the
// result the form was waiting for — and a submitting form also refuses to
// close, so the only way out was to restart the app.
func TestAddressedResultReachesItsOwnDialog(t *testing.T) {
	t.Parallel()

	form := &stubDialog{id: ProviderFormID}
	onTop := &stubDialog{id: "something-else"}

	o := NewOverlay()
	o.OpenDialog(form)
	o.OpenDialog(onTop)

	o.Update(ActionCustomProviderResult{ProviderID: "acme"})

	require.Len(t, form.received, 1, "the result must reach the dialog that started it")
	require.Empty(t, onTop.received, "and not the one that happens to be on top")
}

// TestUnaddressedMessageStillGoesToTheTopDialog keeps the ordinary path
// intact: anything not addressed belongs to whatever is on top.
func TestUnaddressedMessageStillGoesToTheTopDialog(t *testing.T) {
	t.Parallel()

	behind := &stubDialog{id: "behind"}
	onTop := &stubDialog{id: "on-top"}

	o := NewOverlay()
	o.OpenDialog(behind)
	o.OpenDialog(onTop)

	o.Update(keyMsg('x'))

	require.Empty(t, behind.received)
	require.Len(t, onTop.received, 1)
}
