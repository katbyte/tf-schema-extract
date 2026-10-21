// Package clog is the one logger every katbyte tool logs through: stderr, since stdout is the tool's real output, with timestamps, at WARN unless the
// tool's own variable (TCTEST_LOG, KOI_LOG) raises it.
package clog

import (
	"io"
	"os"

	"github.com/sirupsen/logrus"
)

// TimestampFormat is the timestamp layout, for a tool that builds its own formatter to match.
const TimestampFormat = "2006-01-02 15:04:05"

// DefaultLevel is where a logger starts: warnings and errors only.
const DefaultLevel = logrus.WarnLevel

// FallbackLevel is the level a string that does not parse gets: the most verbose, so a typo shows everything rather than hiding what was wanted.
const FallbackLevel = logrus.TraceLevel

// Log is the one logger the whole process uses, so setting its level once sets it for everything.
var Log = New()

// New is a logger set up as Log is, for the rare second logger with a level of its own.
func New() *logrus.Logger {
	return NewWithOutput(os.Stderr)
}

// NewWithOutput is New writing to w, so a test can capture the log.
func NewWithOutput(w io.Writer) *logrus.Logger {
	l := logrus.New()
	l.SetOutput(w)

	formatter := new(logrus.TextFormatter)
	formatter.TimestampFormat = TimestampFormat
	formatter.FullTimestamp = true
	l.SetFormatter(formatter)

	l.SetLevel(DefaultLevel)

	return l
}

// SetLevelFromEnv sets Log's level from a variable such as "TCTEST_LOG": unset leaves DefaultLevel, a value that does not parse gives FallbackLevel
// and logs the typo. Call it once, before any logging that matters.
func SetLevelFromEnv(envVar string) {
	applyLevel(Log, os.Getenv(envVar), envVar)
}

// SetLevel sets Log's level from a string such as "debug", by the same rules as SetLevelFromEnv, so a flag and the variable behave alike.
func SetLevel(level string) {
	applyLevel(Log, level, "")
}

// applyLevel applies level to l; source names the variable it came from for the error, "" when passed directly.
func applyLevel(l *logrus.Logger, level, source string) {
	if level == "" {
		l.SetLevel(DefaultLevel)
		return
	}

	ll, err := logrus.ParseLevel(level)
	if err != nil {
		l.SetLevel(FallbackLevel)
		if source != "" {
			l.Errorf("defaulting to %s: unable to parse `%s` into a valid log level: %v", FallbackLevel, source, err)
		} else {
			l.Errorf("defaulting to %s: unable to parse %q into a valid log level: %v", FallbackLevel, level, err)
		}
		return
	}

	l.SetLevel(ll)
}
