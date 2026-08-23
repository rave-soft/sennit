package db

import (
	"context"
	"testing"
	"time"
)

// TestUpdateMessageStampsUpdatedAtWithoutTheTrigger pins what dropping
// update_messages_updated_at relies on: UpdateMessage sets the column
// itself. If a future query updates a message row without setting it, the
// value silently stops moving — the trigger that used to cover for that is
// gone, deliberately, because it made every streaming flush two writes.
func TestUpdateMessageStampsUpdatedAtWithoutTheTrigger(t *testing.T) {
	conn, err := Connect(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	q := New(conn)
	ctx := context.Background()

	sess, err := q.CreateSession(ctx, CreateSessionParams{ID: "s1", Title: "t", ProjectPath: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := q.CreateMessage(ctx, CreateMessageParams{ID: "m1", SessionID: sess.ID, Role: "assistant", Parts: "[]"})
	if err != nil {
		t.Fatal(err)
	}

	// The column has one-second resolution, so move the clock past it.
	time.Sleep(1100 * time.Millisecond)
	if err := q.UpdateMessage(ctx, UpdateMessageParams{ID: msg.ID, Parts: `[{"x":1}]`}); err != nil {
		t.Fatal(err)
	}

	got, err := q.GetMessage(ctx, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt <= msg.UpdatedAt {
		t.Fatalf("updated_at must move on an update: created %d, updated %d", msg.UpdatedAt, got.UpdatedAt)
	}
}
