package workspace

import (
	"context"

	"github.com/rave-soft/sennit/internal/stats"
)

// Stats implements [UsageReporter] by forwarding to the App, which owns
// the database connection the aggregation reads.
func (w *AppWorkspace) Stats(ctx context.Context, req stats.Request) (stats.Snapshot, error) {
	return w.app.Stats(ctx, req)
}
