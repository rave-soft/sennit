package shell

import (
	"bytes"
	"context"
	"os"
	"sync"
)

// progressWriter wraps an io.Writer and calls onProgress with each write.
// It is safe for concurrent use from multiple goroutines (e.g. stdout and
// stderr writing simultaneously).
type progressWriter struct {
	mu         sync.Mutex
	buf        bytes.Buffer
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
		opts.Env = os.Environ()
	}
	opts.Env = append(opts.Env, ptyColorEnvVars...)

	buf := &progressWriter{onProgress: onProgress}
	opts.Stdout = buf
	opts.Stderr = buf

	runErr := Run(ctx, opts)

	exitCode := 0
	if runErr != nil {
		exitCode = ExitCode(runErr)
	}

	return CaptureResult{
		Output:   buf.String(),
		ExitCode: exitCode,
	}, nil
}
