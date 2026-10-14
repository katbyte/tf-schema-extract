// Package version is the version of the tool that imports it: stamped at release, else the module version from a go install, else "dev". Every tool
// stamps the same two variables:
//
//	-X github.com/katbyte/go-kt/version.Version={{ .Tag }}
//	-X github.com/katbyte/go-kt/version.GitCommit={{ .ShortCommit }}
//
// Build info reports the main module's version, so a tool built on go-kt reports its own tag.
package version

import "runtime/debug"

// Version is the release version, never blank: an -X flag given an empty value has happened before.
var Version = "dev"

// GitCommit is the short commit the binary was built from, when stamped.
var GitCommit string

// init settles Version once, so it reads as a plain variable.
func init() {
	// build info is nil for a build without modules, which reads as unknown
	info, _ := debug.ReadBuildInfo()
	Version = resolve(Version, info)
}

// resolve picks the version: stamped, then the main module's, then "dev"; apart from init so the rules can be tested.
func resolve(stamped string, info *debug.BuildInfo) string {
	// an empty -X value and "dev" are both unset
	if stamped != "" && stamped != "dev" {
		return stamped
	}

	if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	return "dev"
}
