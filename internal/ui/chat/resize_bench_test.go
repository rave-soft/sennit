package chat

import (
	"context"
	"os"
	"testing"

	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// BenchmarkResizeSession reproduces the resize re-render path over a real
// session's messages. Point BRAID_BENCH_SESSION at a full session id and
// BRAID_BENCH_DATADIR at the braid data dir (defaults to ./.braid).
//
//	BRAID_BENCH_SESSION=e6368d820207a406 go test ./internal/ui/chat/ \
//	  -run x -bench BenchmarkResizeSession -benchtime 20x -cpuprofile /tmp/cpu.out
func BenchmarkResizeSession(b *testing.B) {
	sessionID := os.Getenv("BRAID_BENCH_SESSION")
	if sessionID == "" {
		b.Skip("set BRAID_BENCH_SESSION to a full session id")
	}
	dataDir := os.Getenv("BRAID_BENCH_DATADIR")
	if dataDir == "" {
		dataDir = ".braid"
	}

	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir, db.WithDataDirLock(false))
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	// Note: intentionally not closing conn. db.Connect pools connections by
	// path, and the testing framework may invoke this function more than
	// once; closing would break the shared pooled *sql.DB on re-entry.

	svc := message.NewService(db.New(conn))
	msgs, err := svc.List(ctx, sessionID)
	if err != nil {
		b.Fatalf("list messages: %v", err)
	}
	if len(msgs) == 0 {
		b.Fatalf("no messages for session %s", sessionID)
	}
	b.Logf("loaded %d messages", len(msgs))

	ptrs := make([]*message.Message, len(msgs))
	for i := range msgs {
		ptrs[i] = &msgs[i]
	}
	toolResults := BuildToolResultMap(ptrs)

	sty := styles.CharmtonePantera()
	var items []list.Item
	for _, m := range ptrs {
		for _, it := range ExtractMessageItems(&sty, m, toolResults) {
			items = append(items, it)
		}
	}
	b.Logf("built %d items", len(items))

	l := list.NewList(items...)

	b.ResetTimer()
	// Alternate widths so every iteration is a genuine width change, which
	// is what invalidates the caches and forces a full re-render — exactly
	// what a resize drag does.
	widths := []int{100, 99}
	i := 0
	for b.Loop() {
		l.SetSize(widths[i%2], 40)
		_ = l.TotalHeight()
		i++
	}
}
