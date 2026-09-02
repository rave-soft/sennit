package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRuntimeSnapshotDoesNotMixConcurrentPublication(t *testing.T) {
	modelA := SelectedModel{Provider: "provider-a", Model: "model-a"}
	modelB := SelectedModel{Provider: "provider-b", Model: "model-b"}
	store := NewStore(StoreOptions{
		Config:      &Config{Model: modelA, Options: &Options{}},
		WorkingDir:  "/workspace-a",
		LoadedPaths: []string{"config-a.json"},
	})
	store.OverridePreferredModel(modelA)

	store.staleness.mu.Lock()
	snapshotDone := make(chan RuntimeSnapshot, 1)
	go func() {
		snapshotDone <- store.RuntimeSnapshot()
	}()

	require.Eventually(t, func() bool {
		if !store.writeMu.TryLock() {
			return true
		}
		store.writeMu.Unlock()
		return false
	}, time.Second, time.Millisecond)

	publicationStarted := make(chan struct{})
	publicationDone := make(chan struct{})
	go func() {
		close(publicationStarted)
		store.OverridePreferredModel(modelB)
		close(publicationDone)
	}()
	<-publicationStarted
	select {
	case <-publicationDone:
		t.Fatal("publication completed while runtime snapshot held the publication lock")
	default:
	}

	store.staleness.mu.Unlock()
	snapshot := <-snapshotDone
	<-publicationDone

	require.Equal(t, modelA, snapshot.Config.Model)
	require.NotNil(t, snapshot.Overrides.Model)
	require.Equal(t, modelA, *snapshot.Overrides.Model)
	require.Equal(t, "/workspace-a", snapshot.WorkingDir)
	require.Equal(t, []string{"config-a.json"}, snapshot.LoadedPaths)
	require.False(t, snapshot.Staleness.Dirty)
	require.Equal(t, modelB, store.Config().Model)
	require.Equal(t, modelB, *store.Overrides().Model)
}
