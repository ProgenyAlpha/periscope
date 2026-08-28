package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// logLevelEnvVar overrides the [logging] level key in config.toml.
	logLevelEnvVar = "PERISCOPE_LOG_LEVEL"

	// defaultMaxLogBytes is the size at which periscope.log is rotated.
	defaultMaxLogBytes = 5 * 1024 * 1024

	// rotatedSuffix names the single kept-back generation of the log.
	rotatedSuffix = ".1"

	// logRotateInterval is how often a serving process re-checks the log size
	// independently of its own writes.
	logRotateInterval = time.Minute
)

// parseLogLevel maps a human-written level name onto a slog level.
func parseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// resolveLogLevel picks the log level: PERISCOPE_LOG_LEVEL wins over the
// config.toml value, and anything unset or unparseable falls back to Info.
// Debug is opt-in — it was 72% of log volume when it was the hardcoded level.
func resolveLogLevel(configLevel string) slog.Level {
	level := slog.LevelInfo
	if strings.TrimSpace(configLevel) != "" {
		if lvl, ok := parseLogLevel(configLevel); ok {
			level = lvl
		} else {
			slog.Warn("unknown log level in config, using info", "level", configLevel)
		}
	}
	if env := os.Getenv(logLevelEnvVar); strings.TrimSpace(env) != "" {
		if lvl, ok := parseLogLevel(env); ok {
			level = lvl
		} else {
			slog.Warn("unknown log level in env var, ignoring", "var", logLevelEnvVar, "level", env)
		}
	}
	return level
}

// rotatingWriter appends to a log file and rotates it once it grows past
// maxSize. The size is checked on every write, so a long-running `serve`
// actually rotates — the previous startup-only check was unreachable because
// a healthy daemon never restarts.
//
// Rotation renames rather than truncates. Truncating the live path destroys
// the log of any other process that still holds the file open; after a rename
// that process keeps appending to the rotated file and loses nothing.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxSize  int64
	f        *os.File
	size     int64
	lastFail string // suppresses repeated identical rotation complaints
}

func newRotatingWriter(path string, maxSize int64) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, maxSize: maxSize}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	if err := w.rotateIfNeededLocked(); err != nil {
		// A rotation failure is not fatal: keep the handle we have.
		w.reportLocked(err)
	}
	return w, nil
}

func (w *rotatingWriter) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", w.path, err)
	}
	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	w.f, w.size = f, size
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil { // closed during shutdown; drop the line rather than reopen
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	if rerr := w.rotateIfNeededLocked(); rerr != nil {
		// Never fail the log line itself over a rotation problem.
		w.reportLocked(rerr)
	}
	return n, nil
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

func (w *rotatingWriter) rotateIfNeededLocked() error {
	if w.f == nil || w.maxSize <= 0 || w.size <= w.maxSize {
		return nil
	}
	if err := w.f.Close(); err != nil {
		// Re-open so the writer stays usable, then report.
		if oerr := w.openLocked(); oerr != nil {
			return oerr
		}
		return fmt.Errorf("close log before rotation: %w", err)
	}
	backup := w.path + rotatedSuffix
	if err := os.Rename(w.path, backup); err != nil {
		// The old log stays where it is; carry on appending to it.
		if oerr := w.openLocked(); oerr != nil {
			return oerr
		}
		return fmt.Errorf("rotate %s to %s: %w", w.path, backup, err)
	}
	return w.openLocked()
}

// checkRotate re-stats the log so growth this writer did not perform (another
// periscope process appending to the same file) is still caught.
func (w *rotatingWriter) checkRotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, err := os.Stat(w.path); err == nil && st.Size() > w.size {
		w.size = st.Size()
	}
	if err := w.rotateIfNeededLocked(); err != nil {
		w.reportLocked(err)
		return err
	}
	return nil
}

// watch re-checks the log size every interval until ctx is cancelled.
func (w *rotatingWriter) watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = w.checkRotate()
		}
	}
}

// reportLocked writes a rotation failure to stderr once per distinct message,
// so a persistent failure cannot itself flood the log.
func (w *rotatingWriter) reportLocked(err error) {
	if err == nil || err.Error() == w.lastFail {
		return
	}
	w.lastFail = err.Error()
	fmt.Fprintf(os.Stderr, "periscope: log rotation failed: %v\n", err)
}

// setupLogging points the default logger at stderr plus a self-rotating log
// file. It returns the writer so the caller can run watch() and close it; nil
// means the file could not be opened and only stderr is in play.
func setupLogging(logPath string, level slog.Level) *rotatingWriter {
	opts := &slog.HandlerOptions{Level: level}

	w, err := newRotatingWriter(logPath, defaultMaxLogBytes)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
		slog.Warn("cannot open log file, logging to stderr only", "path", logPath, "err", err)
		return nil
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, w), opts)))
	return w
}
