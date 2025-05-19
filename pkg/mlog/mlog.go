package mlog

import (
	"fmt"

	"k8s.io/klog/v2"
)

type logger struct {
	enableVerbose bool
}

var Log = logger{}

func (log *logger) Debugf(format string, args ...interface{}) {
	if log.enableVerbose {
		klog.InfoDepth(1, fmt.Sprintf(format, args...))
	}
}

func (log *logger) Infof(format string, args ...interface{}) {
	klog.InfoDepth(1, fmt.Sprintf(format, args...))
}

func (log *logger) Errorf(format string, args ...interface{}) {
	klog.ErrorDepth(1, fmt.Sprintf(format, args...))
}

func (log *logger) EnableInfLogging() {
	log.enableVerbose = true
}

func (log *logger) DisableInfLogging() {
	log.enableVerbose = false
}
