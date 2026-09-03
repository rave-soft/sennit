package shell

import (
	"context"
	"os"
	"sync"

	sennitenv "github.com/rave-soft/sennit/internal/env"
)

// progressWriter wraps an io.Writer and calls onProgress with each write.
// It is safe for concurrent use from multiple goroutines (e.g. stdout and
// stderr writing simultaneously).
//
// The accumulated copy is a SyncBuffer rather than a bytes.Buffer for the
// reason given there: nothing bounds how much a running command writes, and
// the progress callback has already delivered every chunk by the time this
// copy is read, so the head+tail truncation costs the caller nothing a
// runaway command was going to keep anyway.
type progressWriter struct {
	mu         sync.Mutex
	buf        SyncBuffer
	onProgress func(string)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if n > 0 && w.onProgress != nil {
		w.onProgress(string(p[:n]))
	}
	return n, err
}

// String returns the accumulated output. Locked because interp does not
// wait for background jobs (`cmd &`) before Run returns, so a bgProc
// goroutine may still be calling Write concurrently with this read.
func (w *progressWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// RunAndCaptureStream executes a shell command and streams output chunks
// to onProgress as they arrive. Returns the complete output and exit code.
func RunAndCaptureStream(ctx context.Context, opts RunOptions, onProgress func(string)) (CaptureResult, error) {
	if opts.Env == nil {
		// Strip herdr pane-ownership vars, same as [Shell]: a nested
		// sennit started from bang mode must not attach to the parent
		// pane's agent authority (see [env.WithoutHerdrEnv]).
		opts.Env = sennitenv.WithoutHerdrEnv(os.Environ())
	}
	opts.Env = append(opts.Env, ptyColorEnvVars...)

	buf := &progressWriter{onProgress: onProgress}
	opts.Stdout = buf
	opts.Stderr = buf

	runErr := Run(ctx, opts)

	exitCode, canceled, startErr := classifyCaptureErr(runErr)
	if startErr != nil {
		return CaptureResult{StartErr: startErr}, nil
	}

	return CaptureResult{
		Output:   buf.String(),
		ExitCode: exitCode,
		Canceled: canceled,
	}, nil
}
