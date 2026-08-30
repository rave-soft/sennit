package chatlist

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// testMessageItem is a minimal chat item used to populate the list without
// pulling in full message rendering machinery. internal/ui/model has its
// own copy for the same reason: a test fixture is not worth an exported
// type on either package, and the two are free to drift apart as each
// package's tests need.
type testMessageItem struct {
	id   string
	text string
}

func (m testMessageItem) ID() string           { return m.id }
func (m testMessageItem) Render(int) string    { return m.text }
func (m testMessageItem) RawRender(int) string { return m.text }
func (m testMessageItem) Version() uint64      { return 0 }
func (m testMessageItem) Finished() bool       { return true }

var _ chat.MessageItem = testMessageItem{}

// newTestChat builds a Chat at the size the draw tests render at. It
// replaces a fixture that built a whole UI to reach its chat field, which
// is what kept these tests in internal/ui/model: they are about this
// component's render cache and nothing else.
func newTestChat(t *testing.T) *Chat {
	t.Helper()

	com := common.DefaultCommon(context.Background(), nil)
	c := NewChat(com, config.ScrollbarDefault)
	c.SetSize(80, 20)
	return c
}
