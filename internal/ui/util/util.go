// Package util provides utility functions for UI message handling.
package util

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

func CmdHandler(msg tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return msg
	}
}

func ReportError(err error) tea.Cmd {
	return CmdHandler(NewErrorMsg(err))
}

type InfoType int

const (
	InfoTypeInfo InfoType = iota
	InfoTypeSuccess
	InfoTypeWarn
	InfoTypeError
	InfoTypeUpdate
)

func NewInfoMsg(info string) InfoMsg {
	return InfoMsg{
		Type: InfoTypeInfo,
		Msg:  info,
	}
}

func NewWarnMsg(warn string) InfoMsg {
	return InfoMsg{
		Type: InfoTypeWarn,
		Msg:  warn,
	}
}

func NewErrorMsg(err error) InfoMsg {
	return InfoMsg{
		Type: InfoTypeError,
		Msg:  err.Error(),
	}
}

func ReportInfo(info string) tea.Cmd {
	return CmdHandler(NewInfoMsg(info))
}

func ReportWarn(warn string) tea.Cmd {
	return CmdHandler(NewWarnMsg(warn))
}

type (
	InfoMsg struct {
		Type InfoType
		Msg  string
		TTL  time.Duration
	}
	// ClearStatusMsg asks the status line to drop the message it is
	// showing. Seq identifies the message the timer was armed for: a
	// timer belongs to one status line entry, and without the tag an
	// older one wiped a newer message — most visibly an error that
	// replaced a long-lived notice and then vanished after whatever was
	// left of the notice's own TTL.
	ClearStatusMsg struct {
		Seq int
	}
)

// IsEmpty checks if the [InfoMsg] is empty.
func (m InfoMsg) IsEmpty() bool {
	var zero InfoMsg
	return m == zero
}
