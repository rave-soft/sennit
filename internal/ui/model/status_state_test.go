package model

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func TestStatusKeepsNewestMessageUntilItsTimerExpires(t *testing.T) {
	t.Parallel()

	status := new(Status)
	firstTimer := status.ShowInfo(util.InfoMsg{Type: util.InfoTypeInfo, Msg: "first", TTL: time.Hour})
	first := status.messageSeq
	secondTimer := status.ShowInfo(util.InfoMsg{Type: util.InfoTypeInfo, Msg: "second", TTL: time.Hour})
	second := status.messageSeq
	require.NotNil(t, firstTimer)
	require.NotNil(t, secondTimer)
	require.Less(t, first, second)

	status.ClearInfoMsg(first)
	require.Equal(t, "second", status.InfoMsg().Msg)

	status.ClearInfoMsg(second)
	require.Empty(t, status.InfoMsg())
}

func TestStatusTTLUsesDefaultForNonPositiveValues(t *testing.T) {
	t.Parallel()

	require.Equal(t, DefaultStatusTTL, statusTTL(0))
	require.Equal(t, DefaultStatusTTL, statusTTL(-time.Second))
}

func TestStatusTTLPreservesPositiveValue(t *testing.T) {
	t.Parallel()

	require.Equal(t, time.Second, statusTTL(time.Second))
}
