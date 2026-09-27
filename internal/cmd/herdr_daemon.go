package cmd

import "github.com/rave-soft/sennit/internal/herdr"

// herdrClientConstructor is the hook runDaemonTUI's wireHerdr uses to
// obtain the process-wide herdr client. herdr.Init is env-gated (it
// returns nil unless HERDR_ENV=1 names a real pane, see
// internal/herdr/client.go), so this already means "construct one only
// when this process is running inside a herdr pane" in production; tests
// substitute a fake instead of exercising that singleton (which seeds its
// state from HERDR_ENV once per process via sync.Once, so a second test
// wanting the other outcome in the same binary could never get it) --
// mirrors internal/app/bootstrap.go's own herdrClient var/HerdrClient
// option for the in-process path.
var herdrClientConstructor = herdr.Init

// wireHerdr wraps send so every frontend event daemon-mode's own TUI
// receives through Workspace.Subscribe is also translated and handed to
// the herdr client that TUI process drives for its own terminal pane
// (CLIENT-SERVER.md, PR 1.5): the daemon itself never runs one
// (internal/daemon/daemon.go's own doc comment), so each frontend --
// runInteractiveDaemon and attach both funnel through here via
// runDaemonTUI -- must drive one from whatever it sees on its own event
// stream instead. herdr.Client.HandleEvent is nil-safe, so this needs no
// extra branch for "not running inside a herdr pane": herdrClientConstructor
// already returns nil then, and every call below becomes a no-op.
func wireHerdr(send func(any)) func(any) {
	client := herdrClientConstructor()
	return func(msg any) {
		if hev := herdr.TranslateFrontend(msg); hev != nil {
			client.HandleEvent(hev)
		}
		send(msg)
	}
}
