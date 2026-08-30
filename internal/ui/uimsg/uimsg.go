// Package uimsg holds the message-ownership vocabulary the TUI's router
// and its feature packages share.
//
// It exists because a feature that moved out of internal/ui/model still
// has to be able to say who its messages belong to, and the router still
// has to be able to ask. Neither can import the other — the router imports
// the features — so the marker lives on its own.
package uimsg

// MainScreenMsg marks a message that belongs to the main session screen no
// matter which screen is currently on top — the result of a refresh only
// the main screen ever starts. The router delivers these to the main
// screen directly rather than to the active one.
//
// Embed MainScreenOwned in the message type to claim this.
type MainScreenMsg interface{ isMainScreenMsg() }

// MainScreenOwned is the embeddable implementation of MainScreenMsg.
type MainScreenOwned struct{}

func (MainScreenOwned) isMainScreenMsg() {}
