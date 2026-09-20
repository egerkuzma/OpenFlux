package utils

import (
	"fmt"
	"io"
	"log"
	"os"
)

var (
	debugLog *log.Logger
	verbose  bool
	output   io.Writer = os.Stderr
)

// SetOutput redirects all debug and standard log output to w.
// Used by the mobile bridge to pipe logs into the app UI.
func SetOutput(w io.Writer) {
	output = w
	log.SetOutput(w)
	if debugLog != nil {
		debugLog.SetOutput(w)
	}
}

func EnableDebug() {
	verbose = true
	debugLog = log.New(output, "", log.LstdFlags|log.Lmicroseconds)
	log.SetOutput(output)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
}

func Debugf(format string, args ...interface{}) {
	if verbose {
		debugLog.Output(2, fmt.Sprintf(format, args...))
	}
}

// SetDebug toggles verbose logging at runtime (off = Debugf becomes a no-op).
func SetDebug(on bool) {
	if on {
		EnableDebug()
		return
	}
	verbose = false
}

func IsVerbose() bool {
	return verbose
}

// SafeGo runs fn in a new goroutine, recovering from any panic so a crash in
// one worker cannot take down the whole process (critical when this code runs
// embedded as a library inside a mobile app).
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// A recovered panic is a defect, not noise. Behind --debug it
				// was invisible, and the goroutine died silently: a session
				// renewer or a reader could stop for good and the only symptom
				// would be a link that quietly went still.
				Infof("[PANIC] recovered in %s: %v", name, r)
			}
		}()
		fn()
	}()
}

// Infof logs something an operator would want to see without turning on
// --debug: a connection died, a reconnect failed, a link came back. These are
// rare enough to say out loud, unlike the per-packet detail in Debugf, which
// would bury them. Output honours SetOutput, so it still reaches the app's log
// window.
func Infof(format string, args ...interface{}) {
	log.Printf(format, args...)
}
