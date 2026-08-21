package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
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

		slog.SetDefault(slog.New(slog.NewMultiHandler(handlers...)))
		initialized.Store(true)
	})
}

func Initialized() bool {
	return initialized.Load()
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
