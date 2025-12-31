package mlog

import (
	"fmt"
	"sync/atomic"
	"time"
)

var programStartTime = time.Now()

type logger struct {
	enableVerbose *atomic.Bool
}

var Log = logger{&atomic.Bool{}}

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

func (log *logger) Debugf(format string, args ...interface{}) {
	if log.enableVerbose.Load() {
		fmt.Printf("%s%s\n", relativePrefix(), fmt.Sprintf(format, args...))
	}
}

func (log *logger) Infof(format string, args ...interface{}) {
	fmt.Printf("%s%s\n", relativePrefix(), fmt.Sprintf(format, args...))
}

// InfofPrecise logs with millisecond precision timing (for block replay)
func (log *logger) InfofPrecise(format string, args ...interface{}) {
	fmt.Printf("%s%s\n", relativePrefixPrecise(), fmt.Sprintf(format, args...))
}

func (log *logger) Errorf(format string, args ...interface{}) {
	fmt.Printf("%sERROR: %s\n", relativePrefix(), fmt.Sprintf(format, args...))
}

func (log *logger) EnableInfLogging() {
	log.enableVerbose.Store(true)
}

func (log *logger) DisableInfLogging() {
	log.enableVerbose.Store(false)
}
