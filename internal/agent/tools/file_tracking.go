package tools

import (
	"context"
	"time"

	"github.com/rave-soft/sennit/internal/filetracker/coverage"
)

// FileTracking records and retrieves the file-read state required by tools.
// Storage implementations stay outside the tools package.
type FileTracking interface {
	RecordRead(ctx context.Context, sessionID, path string)
	RecordPartialRead(ctx context.Context, sessionID, path string, start, end int)
	RecordEdit(ctx context.Context, sessionID, path string, start, end, newEnd int)
	ReadCoverage(ctx context.Context, sessionID, path string) FileCoverage
	LastReadTime(ctx context.Context, sessionID, path string) time.Time
}

// FileCoverage and FileLineRange alias the shared leaf type in
// internal/filetracker/coverage: the tools package must not import
// internal/filetracker itself (it pulls in internal/db), but the interval
// arithmetic — Covers, Empty, Add, Shift — has no reason to be duplicated
// just for that. Keeping these names, rather than switching call sites to
// coverage.Coverage/coverage.LineRange, keeps this diff small.
type FileCoverage = coverage.Coverage

type FileLineRange = coverage.LineRange
