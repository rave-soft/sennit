package tools

import (
	"context"
	"time"
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

// FileCoverage identifies the inclusive 1-based file lines a session has read.
type FileCoverage struct {
	Full   bool
	Ranges []FileLineRange
}

type FileLineRange struct {
	Start int
	End   int
}

func (c FileCoverage) Covers(start, end int) bool {
	if c.Full {
		return true
	}
	if start > end {
		start, end = end, start
	}
	for _, r := range c.Ranges {
		if r.Start > start {
			return false
		}
		if r.End >= end {
			return true
		}
		if r.End >= start {
			return false
		}
	}
	return false
}

func (c FileCoverage) Empty() bool { return !c.Full && len(c.Ranges) == 0 }

func (c FileCoverage) Add(r FileLineRange) FileCoverage {
	if c.Full {
		return c
	}
	if r.Start > r.End {
		r.Start, r.End = r.End, r.Start
	}
	if r.Start < 1 {
		r.Start = 1
	}
	ranges := append(append([]FileLineRange(nil), c.Ranges...), r)
	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			if ranges[j].Start < ranges[i].Start {
				ranges[i], ranges[j] = ranges[j], ranges[i]
			}
		}
	}
	out := ranges[:0]
	for _, next := range ranges {
		if len(out) == 0 || next.Start > out[len(out)-1].End+1 {
			out = append(out, next)
			continue
		}
		if next.End > out[len(out)-1].End {
			out[len(out)-1].End = next.End
		}
	}
	return FileCoverage{Ranges: out}
}

func (c FileCoverage) Shift(start, end, delta int) FileCoverage {
	if c.Full || delta == 0 || len(c.Ranges) == 0 {
		return c
	}
	out := make([]FileLineRange, 0, len(c.Ranges))
	for _, r := range c.Ranges {
		shift := func(line, inside int) int {
			switch {
			case line < start:
				return line
			case line > end:
				return line + delta
			default:
				return inside
			}
		}
		r.Start, r.End = shift(r.Start, start), shift(r.End, end+delta)
		if r.End >= r.Start {
			out = append(out, r)
		}
	}
	return FileCoverage{Ranges: out}
}
