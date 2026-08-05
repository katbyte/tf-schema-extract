package main

import (
	"os"

	c "github.com/gookit/color"
	"github.com/katbyte/tfschemaex/cli"
	"github.com/katbyte/tfschemaex/lib/clog"
)

func main() {
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
