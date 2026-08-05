// Package cout provides verbosity-levelled, coloured console output.
//
// All output goes to stderr — stdout is reserved for the extracted schema JSON so
// `tfschemaex plugin ./provider > schema.json` always produces clean JSON.
package cout

import (
	"os"

	c "github.com/gookit/color"
)

// Verbosity levels (ordered from least to most output)
const (
	VerbositySilent = iota
	VerbosityNormal
	VerbosityVerbose
)

// Level controls the output verbosity. Set before any output calls.
var Level = VerbosityNormal

// Printf prints normal progress output with colour support (suppressed in silent mode)
func Printf(format string, args ...any) {
	if Level < VerbosityNormal {
		return
	}
	c.Fprintf(os.Stderr, format, args...)
}

// Println prints normal progress output (suppressed in silent mode)
func Println(args ...any) {
	if Level < VerbosityNormal {
		return
	}
	c.Fprintln(os.Stderr, args...)
}

// Verbosef prints output only shown in verbose mode
func Verbosef(format string, args ...any) {
	if Level < VerbosityVerbose {
		return
	}
	c.Fprintf(os.Stderr, format, args...)
}
