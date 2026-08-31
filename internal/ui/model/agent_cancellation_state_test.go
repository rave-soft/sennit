package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentCancellationStateLifecycle(t *testing.T) {
	var state agentCancellationState

	require.False(t, state.isArmed(), "initial state must be unarmed")
	require.False(t, state.confirm(), "an unarmed state must not confirm")

	state.arm()
	require.True(t, state.isArmed(), "arm must begin confirmation")
	require.True(t, state.confirm(), "an armed state must confirm cancellation")
	require.False(t, state.isArmed(), "confirmation must reset the lifecycle")

	state.expire()
	require.False(t, state.isArmed(), "expiry must leave an unarmed state unchanged")

	state.arm()
	require.True(t, state.isArmed())
	state.expire()
	require.False(t, state.isArmed(), "expiry must reset an armed confirmation")
	state.arm()
	require.True(t, state.isArmed(), "the state must support re-arming after expiry")
}
