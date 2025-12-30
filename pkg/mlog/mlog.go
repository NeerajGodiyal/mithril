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

// relativePrefix returns the elapsed time since program start as a prefix
func relativePrefix() string {
	d := time.Since(programStartTime)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	secs := d.Seconds() // includes fractional seconds

	var timeStr string
	if h > 0 {
		timeStr = fmt.Sprintf("%2dh%02dm", h, m)
	} else if m > 0 {
		timeStr = fmt.Sprintf("%2dm%06.3fs", m, secs)
	} else {
		timeStr = fmt.Sprintf("%6.3fs", secs)
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

func (log *logger) Errorf(format string, args ...interface{}) {
	fmt.Printf("%sERROR: %s\n", relativePrefix(), fmt.Sprintf(format, args...))
}

func (log *logger) EnableInfLogging() {
	log.enableVerbose.Store(true)
}

func (log *logger) DisableInfLogging() {
	log.enableVerbose.Store(false)
}
