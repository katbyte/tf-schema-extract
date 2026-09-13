package main

import (
	"os"

	c "github.com/gookit/color"
	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/go-kt/cout"
	"github.com/katbyte/tfschemaex/cli"
)

func main() {
	// the log level comes from TFSCHEMAEX_LOG; read it once here, before anything logs
	clog.SetLevelFromEnv("TFSCHEMAEX_LOG")

	// stdout is reserved for the extracted schema JSON, so every progress message goes to stderr
	cout.Out = os.Stderr

	cmd, err := cli.Make()
	if err != nil {
		clog.Log.Error(c.Sprintf("<red>tfschemaex: building cmd</> %v", err))

		os.Exit(1)
	}

	if err := cmd.Execute(); err != nil {
		clog.Log.Error(c.Sprintf("<red>tfschemaex:</> %v", err))

		os.Exit(1)
	}

	os.Exit(0)
}
