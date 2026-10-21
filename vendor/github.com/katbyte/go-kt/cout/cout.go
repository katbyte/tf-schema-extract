// Package cout prints for the person running a tool, as clog logs for whoever diagnoses it. Every call carries a least verbosity, so --quiet and
// --verbose are honoured in one place, and colour tags such as <red>...</> render anywhere in the output and strip on a plain terminal.
package cout

import (
	"io"
	"os"

	"github.com/gookit/color"
)

// Verbosity is how much a tool prints, ordered so "Normal and above" is a comparison.
type Verbosity int

// The verbosity levels, from least to most output.
const (
	// VerbositySilent prints nothing, not even errors: only the exit code.
	VerbositySilent Verbosity = iota
	// VerbosityJSON keeps stdout for a JSON document the tool writes itself; errors still go to Err.
	VerbosityJSON
	// VerbosityQuiet prints only the lines a script parses (Quietf, QuietOnlyf) and errors.
	VerbosityQuiet
	// VerbosityNormal is the default: everything a person wants to see.
	VerbosityNormal
	// VerbosityVerbose adds the detail behind Verbosef, -v.
	VerbosityVerbose
)

// String is the level's name, as the flag that selects it is spelled.
func (v Verbosity) String() string {
	switch v {
	case VerbositySilent:
		return "silent"
	case VerbosityJSON:
		return "json"
	case VerbosityQuiet:
		return "quiet"
	case VerbosityNormal:
		return "normal"
	case VerbosityVerbose:
		return "verbose"
	default:
		return "unknown"
	}
}

// Level is the verbosity, set once from the flags before any output.
var Level = VerbosityNormal

// Out is where normal output goes, stdout unless changed. A tool whose stdout carries its real output, a JSON document or an MCP session, points this
// at stderr so progress lines stay out of it.
var Out io.Writer = os.Stdout

// Err is where Errorf writes, stderr, apart from Out so errors stay visible when Out is redirected.
var Err io.Writer = os.Stderr

// printer is the package state read once per call, so tests can run in parallel without touching the globals.
type printer struct {
	level Verbosity
	out   io.Writer
	err   io.Writer
}

func current() printer {
	return printer{level: Level, out: Out, err: Err}
}

// Writer is Out at Normal and above and io.Discard below, for output that streams through a tabwriter or an encoder.
func Writer() io.Writer {
	return current().writer()
}

func (p printer) writer() io.Writer {
	if p.level < VerbosityNormal {
		return io.Discard
	}
	return p.out
}

// Sprintf formats and renders colour tags, for a coloured fragment to pass on or print elsewhere.
func Sprintf(format string, args ...any) string {
	return color.Sprintf(format, args...)
}

// Printf prints normal output, nothing in quiet or silent modes.
func Printf(format string, args ...any) {
	current().printf(VerbosityNormal, format, args...)
}

// Println prints normal output and a newline, nothing in quiet or silent modes.
func Println(args ...any) {
	p := current()
	if p.level < VerbosityNormal {
		return
	}
	color.Fprintln(p.out, args...)
}

// Verbosef prints detail only asked for with -v.
func Verbosef(format string, args ...any) {
	current().printf(VerbosityVerbose, format, args...)
}

// Quietf prints in quiet mode and above: the one line a script parses, which normal output shows too.
func Quietf(format string, args ...any) {
	current().printf(VerbosityQuiet, format, args...)
}

// QuietOnlyf prints only in quiet mode, for a line normal mode prints another way.
func QuietOnlyf(format string, args ...any) {
	p := current()
	if p.level != VerbosityQuiet {
		return
	}
	color.Fprintf(p.out, format, args...)
}

// Errorf prints an error to Err in every mode but silent.
func Errorf(format string, args ...any) {
	p := current()
	if p.level == VerbositySilent {
		return
	}
	color.Fprintf(p.err, format, args...)
}

// printf writes to out when the level is at least minimum.
func (p printer) printf(minimum Verbosity, format string, args ...any) {
	if p.level < minimum {
		return
	}
	color.Fprintf(p.out, format, args...)
}
