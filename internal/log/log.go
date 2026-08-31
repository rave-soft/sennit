package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/event"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	initOnce    sync.Once
	initialized atomic.Bool
	// logDir is the directory of the log file passed to Setup, reused by
	// RecoverPanic so panic dumps land next to the regular logs instead of
	// wherever the process happened to be started from.
	logDir atomic.Value // string
)

func Setup(logFile string, debug bool, ws ...io.Writer) {
	initOnce.Do(func() {
		logDir.Store(filepath.Dir(logFile))

		logRotator := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    10,    // Max size in MB
			MaxBackups: 0,     // Number of backups
			MaxAge:     30,    // Days
			Compress:   false, // Enable compression
		}
		// A log file is per process now (see config.GlobalLogFile), and
		// pids are reused: without this, a run that happened to draw the
		// pid of a dead one would append to its log and put two
		// unrelated processes back in one file — the exact thing the
		// split exists to prevent. Rotating an existing file costs one
		// rename and guarantees each run starts on a blank one.
		if info, err := os.Stat(logFile); err == nil && info.Size() > 0 {
			if err := logRotator.Rotate(); err != nil {
				fmt.Fprintf(os.Stderr, "could not rotate the previous log at %s: %v\n", logFile, err)
			}
		}

		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}

		opts := &slog.HandlerOptions{
			Level:     level,
			AddSource: true,
		}

		var handlers []slog.Handler
		handlers = append(handlers, slog.NewJSONHandler(logRotator, opts))

		for _, w := range ws {
			if w == nil {
				continue
			}
			if f, ok := w.(term.File); ok && term.IsTerminal(f.Fd()) {
				handlers = append(handlers, slog.NewTextHandler(w, opts))
			} else {
				handlers = append(handlers, slog.NewJSONHandler(w, opts))
			}
		}

		// pid on every record as well as in the file name: logs get
		// concatenated, pasted into issues, and grepped across the whole
		// directory, and in any of those the file name is gone while the
		// question "was this all one sennit?" is exactly the one being
		// asked.
		slog.SetDefault(slog.New(slog.NewMultiHandler(handlers...)).With("pid", os.Getpid()))
		initialized.Store(true)
		go sweepStaleLogs(filepath.Dir(logFile), logFile)
	})
}

func Initialized() bool {
	return initialized.Load()
}

// staleLogAge is how long a log outlives the process that wrote it. It
// matches lumberjack's own MaxAge above, which is what bounds a single
// file's rotated backups: one file per process only helps if the
// directory does not grow one file per run forever.
const staleLogAge = 30 * 24 * time.Hour

// sweepStaleLogs deletes logs in dir last written more than staleLogAge
// ago, skipping keep (this process's own, which a clock skew could
// otherwise make look ancient).
//
// Best-effort and deliberately quiet: this runs for tidiness, on a
// goroutine, and a log directory that cannot be swept is not a reason to
// report anything to somebody who was trying to start an agent. It is
// also why nothing here uses slog — it runs as slog is being installed.
func sweepStaleLogs(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleLogAge)
	for _, entry := range entries {
		// Panic dumps (RecoverPanic, below) share this directory and are
		// the one thing here nobody would want tidied away on a timer.
		// config.isRunLogName draws the same line for the same files.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") || strings.Contains(entry.Name(), "-panic-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if path == keep {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(path)
	}
}

// panicLogPath resolves the full path for a panic log file named
// filename. It reuses the directory Setup configured for the regular log
// file if available, falling back to the user's cache directory, and as a
// last resort the bare filename in the process's cwd. It never panics.
func panicLogPath(filename string) string {
	if dir, ok := logDir.Load().(string); ok && dir != "" {
		return filepath.Join(dir, filename)
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		slog.Error("Failed to determine user cache dir for panic log", "error", err)
		return filename
	}

	dir := filepath.Join(cacheDir, brand.Slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("Failed to create sennit cache dir for panic log", "dir", dir, "error", err)
		return filename
	}

	return filepath.Join(dir, filename)
}

func RecoverPanic(name string, cleanup func()) {
	if r := recover(); r != nil {
		// Run cleanup unconditionally, even if the panic log file below
		// can't be created or writing it panics itself.
		if cleanup != nil {
			defer cleanup()
		}

		event.Error(r, "panic", true, "name", name)

		stack := debug.Stack()
		slog.Error("Recovered from panic", "name", name, "panic", r, "stack", string(stack))

		// Create a timestamped panic log file
		timestamp := time.Now().Format("20060102-150405")
		filename := fmt.Sprintf(brand.Slug+"-panic-%s-%s.log", name, timestamp)

		file, err := os.Create(panicLogPath(filename))
		if err != nil {
			slog.Error("Failed to create panic log file", "name", name, "error", err)
			return
		}
		defer file.Close()

		// Write panic information and stack trace
		fmt.Fprintf(file, "Panic in %s: %v\n\n", name, r)
		fmt.Fprintf(file, "Time: %s\n\n", time.Now().Format(time.RFC3339))
		fmt.Fprintf(file, "Stack Trace:\n%s\n", stack)
	}
}
