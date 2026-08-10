package tools

import (
	"context"
	"time"
)

// StrandCreateArgs mirrors internal/strand.CreateArgs. It is declared here,
// rather than imported, to keep this package free of a dependency on
// internal/strand: internal/strand imports internal/app, which imports
// internal/agent, which imports this package — importing internal/strand
// from here would close that cycle. internal/strand/agenttool.go adapts
// *strand.Manager to the [StrandManager] interface below, converting
// between the two packages' otherwise-identical types at the seam.
type StrandCreateArgs struct {
	Name        string
	Goal        string
	BaseBranch  string
	MergePolicy string
}

// StrandInfo mirrors internal/strand.Strand, for the same reason.
type StrandInfo struct {
	ID            string
	Name          string
	Goal          string
	BaseBranch    string
	Branch        string
	WorktreePath  string
	SessionID     string
	Status        string
	MergePolicy   string
	ResultSummary string
	Error         string
	CreatedAt     int64
	UpdatedAt     int64
	CompletedAt   int64
}

// StrandManager is the subset of internal/strand.Manager's API the strand_*
// tools need. The coordinator is only ever given one for the main agent of
// a main (non-strand) workspace; it is nil everywhere else, and the
// strand_* tools are omitted entirely when it is nil.
type StrandManager interface {
	Create(ctx context.Context, args StrandCreateArgs) (StrandInfo, error)
	List(ctx context.Context) ([]StrandInfo, error)
	Get(ctx context.Context, idOrName string) (StrandInfo, error)
	Send(ctx context.Context, idOrName, message string) error
	Wait(ctx context.Context, ids []string, timeout time.Duration) error
	Merge(ctx context.Context, idOrName string) error
	Remove(ctx context.Context, idOrName string, force, deleteBranch bool) error
}
