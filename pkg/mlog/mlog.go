package mlog

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

var programStartTime = time.Now()

// LogLevel represents the logging level
type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

// LogConfig holds logging configuration (mirrors config.LogConfig)
type LogConfig struct {
	Dir        string // Log directory (default: /mnt/mithril-logs)
	Level      string // Log level: debug, info, warn, error
	ToStdout   bool   // Also write to stdout (default: true)
	MaxSizeMB  int    // Max log file size in MB before rotation
	MaxAgeDays int    // Delete logs older than this many days
	MaxBackups int    // Keep up to N old log files
}

type logger struct {
	mu            sync.Mutex
	enableVerbose *atomic.Bool
	level         LogLevel
	toStdout      bool
	writer        io.Writer // combined writer (file + optional stdout)
	bufWriter     *bufio.Writer
	fileWriter    *lumberjack.Logger
	logPath       string
	runID         string
	initialized   bool
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

var Log = logger{enableVerbose: &atomic.Bool{}, toStdout: true}

// DefaultConfig returns sensible defaults for logging
func DefaultConfig() LogConfig {
	return LogConfig{
		Dir:        "/mnt/mithril-logs",
		Level:      "info",
		ToStdout:   true,
		MaxSizeMB:  100,
		MaxAgeDays: 7,
		MaxBackups: 10,
	}
}

// Initialize sets up file logging with the given config and run ID.
// If dir is empty, only stdout logging is used.
func Initialize(cfg LogConfig, runID string) error {
	Log.mu.Lock()
	defer Log.mu.Unlock()

	if Log.initialized {
		return nil // Already initialized
	}

	Log.runID = runID
	Log.toStdout = cfg.ToStdout
	Log.level = parseLevel(cfg.Level)

	// If no dir specified, stderr only (stderr to avoid breaking progress bar cursor positioning)
	if cfg.Dir == "" {
		Log.writer = os.Stderr
		Log.initialized = true
		return nil
	}

	// Create log directory if it doesn't exist
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory %s: %w", cfg.Dir, err)
	}

	// Generate log filename: mithril-YYYYMMDD-HHMMSSZ-<runid>.log
	now := time.Now().UTC()
	shortRunID := runID
	if len(shortRunID) > 8 {
		shortRunID = shortRunID[:8]
	}
	filename := fmt.Sprintf("mithril-%s-%s.log", now.Format("20060102-150405Z"), shortRunID)
	Log.logPath = filepath.Join(cfg.Dir, filename)

	// Set up lumberjack for rotation
	Log.fileWriter = &lumberjack.Logger{
		Filename:   Log.logPath,
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		LocalTime:  false, // Use UTC
		Compress:   true,  // gzip old logs
	}

	// Create multi-writer if console output is enabled (use stderr to share stream with progress bars)
	if cfg.ToStdout {
		Log.writer = io.MultiWriter(os.Stderr, Log.fileWriter)
	} else {
		Log.writer = Log.fileWriter
	}

	// Wrap with buffered writer for performance
	Log.bufWriter = bufio.NewWriterSize(Log.writer, 16*1024) // 16KB buffer

	// Create symlink to latest log
	symlinkPath := filepath.Join(cfg.Dir, "mithril-latest.log")
	os.Remove(symlinkPath) // Ignore error if doesn't exist
	if err := os.Symlink(filename, symlinkPath); err != nil {
		// Non-fatal, just log to stdout
		fmt.Fprintf(os.Stderr, "warning: failed to create symlink %s: %v\n", symlinkPath, err)
	}

	// Start background flush goroutine
	Log.stopCh = make(chan struct{})
	Log.wg.Add(1)
	go Log.flushLoop()

	Log.initialized = true
	return nil
}

// flushLoop periodically flushes the buffer and syncs to disk
func (l *logger) flushLoop() {
	defer l.wg.Done()

	flushTicker := time.NewTicker(2 * time.Second)
	syncTicker := time.NewTicker(30 * time.Second)
	defer flushTicker.Stop()
	defer syncTicker.Stop()

	for {
		select {
		case <-l.stopCh:
			return
		case <-flushTicker.C:
			l.flush()
		case <-syncTicker.C:
			l.flushAndSync()
		}
	}
}

// flush flushes the buffer without syncing
func (l *logger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bufWriter != nil {
		l.bufWriter.Flush()
	}
}

// flushAndSync flushes and syncs to disk
func (l *logger) flushAndSync() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bufWriter != nil {
		l.bufWriter.Flush()
	}
	// lumberjack doesn't expose Sync, but flush is sufficient for most cases
}

// Shutdown flushes all pending writes and closes the log file
func Shutdown() {
	Log.mu.Lock()
	defer Log.mu.Unlock()

	if !Log.initialized {
		return
	}

	// Stop the flush goroutine
	if Log.stopCh != nil {
		close(Log.stopCh)
		Log.mu.Unlock()
		Log.wg.Wait()
		Log.mu.Lock()
	}

	// Final flush
	if Log.bufWriter != nil {
		Log.bufWriter.Flush()
	}

	// Close lumberjack
	if Log.fileWriter != nil {
		Log.fileWriter.Close()
	}

	Log.initialized = false
}

// GetLogPath returns the path to the current log file
func GetLogPath() string {
	Log.mu.Lock()
	defer Log.mu.Unlock()
	return Log.logPath
}

// parseLevel converts a string level to LogLevel
func parseLevel(level string) LogLevel {
	switch level {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// relativePrefix returns the elapsed time since program start as a prefix (no milliseconds)
func relativePrefix() string {
	d := time.Since(programStartTime)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := int(d.Seconds())

	var timeStr string
	if h > 0 {
		timeStr = fmt.Sprintf("%2dh%02dm", h, m)
	} else if m > 0 {
		timeStr = fmt.Sprintf("%2dm%02ds", m, s)
	} else {
		timeStr = fmt.Sprintf("%5ds", s)
	}
	return fmt.Sprintf("(+%s) ", timeStr)
}

// relativePrefixPrecise returns the elapsed time with millisecond precision (for block replay)
// Uses fixed 12-char width for times under 10 hours to keep log columns aligned.
func relativePrefixPrecise() string {
	d := time.Since(programStartTime)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	secs := d.Seconds() // includes fractional seconds

	var timeStr string
	if h >= 10 {
		// Double digit hours: allow expansion beyond 12 chars
		timeStr = fmt.Sprintf("%dh%02dm%06.3fs", h, m, secs)
	} else if h > 0 {
		// Single digit hours: 12 chars (e.g., "1h30m45.123s")
		timeStr = fmt.Sprintf("%dh%02dm%06.3fs", h, m, secs)
	} else if m > 0 {
		// Minutes only: pad to 12 chars (e.g., "  30m45.123s")
		timeStr = fmt.Sprintf("  %2dm%06.3fs", m, secs)
	} else {
		// Seconds only: pad to 12 chars (e.g., "     45.123s")
		timeStr = fmt.Sprintf("     %06.3fs", secs)
	}
	return fmt.Sprintf("(+%s) ", timeStr)
}

// write outputs a log message to the configured writer(s)
func (l *logger) write(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.bufWriter != nil {
		l.bufWriter.WriteString(msg)
	} else if l.writer != nil {
		l.writer.Write([]byte(msg))
	} else {
		// Fallback to stderr if not initialized
		fmt.Fprint(os.Stderr, msg)
	}
}

// writeImmediate outputs a log message and immediately flushes (for errors)
func (l *logger) writeImmediate(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.bufWriter != nil {
		l.bufWriter.WriteString(msg)
		l.bufWriter.Flush()
	} else if l.writer != nil {
		l.writer.Write([]byte(msg))
	} else {
		fmt.Fprint(os.Stderr, msg)
	}
}

// writeFileOnly outputs a log message only to the file, not to stdout/stderr.
// Used for diagnostics that should be captured for debugging but not clutter the terminal.
func (l *logger) writeFileOnly(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Write only to file writer, bypassing the multi-writer
	if l.fileWriter != nil {
		l.fileWriter.Write([]byte(msg))
	}
	// If no file writer, silently discard (file-only logging disabled)
}

func (l *logger) Debugf(format string, args ...interface{}) {
	if l.level > LevelDebug && !l.enableVerbose.Load() {
		return
	}
	msg := fmt.Sprintf("%s%s\n", relativePrefix(), fmt.Sprintf(format, args...))
	l.write(msg)
}

func (l *logger) Infof(format string, args ...interface{}) {
	if l.level > LevelInfo {
		return
	}
	msg := fmt.Sprintf("%s%s\n", relativePrefix(), fmt.Sprintf(format, args...))
	l.write(msg)
}

// FileOnlyf logs a message only to the log file, not to the terminal.
// Used for diagnostics that should be captured for debugging but not clutter the terminal.
func (l *logger) FileOnlyf(format string, args ...interface{}) {
	if l.level > LevelInfo {
		return
	}
	msg := fmt.Sprintf("%s%s\n", relativePrefix(), fmt.Sprintf(format, args...))
	l.writeFileOnly(msg)
}

// InfofPrecise logs with millisecond precision timing (for block replay)
// Flushes immediately to ensure real-time output during replay
func (l *logger) InfofPrecise(format string, args ...interface{}) {
	if l.level > LevelInfo {
		return
	}
	msg := fmt.Sprintf("%s%s\n", relativePrefixPrecise(), fmt.Sprintf(format, args...))
	l.writeImmediate(msg)
}

func (l *logger) Warnf(format string, args ...interface{}) {
	if l.level > LevelWarn {
		return
	}
	msg := fmt.Sprintf("%sWARN: %s\n", relativePrefix(), fmt.Sprintf(format, args...))
	l.writeImmediate(msg) // Warnings flush immediately
}

func (l *logger) Errorf(format string, args ...interface{}) {
	msg := fmt.Sprintf("%sERROR: %s\n", relativePrefix(), fmt.Sprintf(format, args...))
	l.writeImmediate(msg) // Errors always flush immediately
}

func (l *logger) EnableInfLogging() {
	l.enableVerbose.Store(true)
}

func (l *logger) DisableInfLogging() {
	l.enableVerbose.Store(false)
}
