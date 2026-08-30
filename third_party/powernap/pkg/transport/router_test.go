package transport

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sourcegraph/jsonrpc2"
)

// TestRoute_NilParams verifies a request or notification with no "params"
// field (req.Params == nil) is routed without panicking, and that the
// handler observes nil params rather than a dereferenced zero value.
func TestRoute_NilParams(t *testing.T) {
	r := NewRouter()

	var gotParams json.RawMessage
	var sawParams bool
	r.Handle("workspace/workspaceFolders", func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		gotParams = params
		sawParams = true
		return "ok", nil
	})

	req := &jsonrpc2.Request{
		Method: "workspace/workspaceFolders",
		ID:     jsonrpc2.ID{Num: 0},
		Notif:  false,
		Params: nil,
	}

	result, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("Route returned %v, want %q", result, "ok")
	}
	if !sawParams {
		t.Fatal("handler was not invoked")
	}
	if gotParams != nil {
		t.Fatalf("got params %v, want nil", gotParams)
	}
}

// TestRoute_RequestWithZeroID verifies a request whose numeric id is 0 is
// still treated as a request (dispatched to Handle, not
// HandleNotification) because notification detection must key off
// req.Notif, not a zero-valued ID.
func TestRoute_RequestWithZeroID(t *testing.T) {
	r := NewRouter()

	handlerCalled := false
	r.Handle("workspace/configuration", func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		handlerCalled = true
		return "settings", nil
	})

	notifCalled := false
	r.HandleNotification("workspace/configuration", func(_ context.Context, _ string, _ json.RawMessage) {
		notifCalled = true
	})

	req := &jsonrpc2.Request{
		Method: "workspace/configuration",
		ID:     jsonrpc2.ID{Num: 0},
		Notif:  false,
	}

	result, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if result != "settings" {
		t.Fatalf("Route returned %v, want %q", result, "settings")
	}
	if !handlerCalled {
		t.Fatal("request handler was not called for id 0")
	}
	if notifCalled {
		t.Fatal("notification handler should not have been called for a request with id 0")
	}
}

// TestRoute_Notification verifies an actual notification (req.Notif ==
// true) is still dispatched to the notification handler and never to a
// request handler.
func TestRoute_Notification(t *testing.T) {
	r := NewRouter()

	notifCalled := false
	r.HandleNotification("textDocument/didOpen", func(_ context.Context, _ string, _ json.RawMessage) {
		notifCalled = true
	})

	req := &jsonrpc2.Request{
		Method: "textDocument/didOpen",
		Notif:  true,
	}

	result, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if result != nil {
		t.Fatalf("Route returned %v for a notification, want nil", result)
	}
	if !notifCalled {
		t.Fatal("notification handler was not called")
	}
}
