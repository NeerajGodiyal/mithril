package mlog

import (
	"fmt"
	"sync/atomic"

	"k8s.io/klog/v2"
)

type logger struct {
	enableVerbose *atomic.Bool
}

var Log = logger{&atomic.Bool{}}

func (log *logger) Debugf(format string, args ...interface{}) {
	if log.enableVerbose.Load() {
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
	log.enableVerbose.Store(true)
}

func (log *logger) DisableInfLogging() {
	log.enableVerbose.Store(false)
}
