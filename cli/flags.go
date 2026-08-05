package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/katbyte/tfschemaex/lib/clog"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type FlagData struct {
	Out      string        `mapstructure:"out"`
	Entry    string        `mapstructure:"entry"`
	KeepShim bool          `mapstructure:"keep-shim"`
	Timeout  time.Duration `mapstructure:"timeout"`
	Silent   bool          `mapstructure:"silent"`
	Verbose  bool          `mapstructure:"verbose"`
}

func configureFlags(root *cobra.Command) error {
	pflags := root.PersistentFlags()

	pflags.StringP("out", "o", "-", "output file for the schema JSON ('-' for stdout)")
	pflags.Bool("silent", false, "suppress all progress output")
	pflags.BoolP("verbose", "v", false, "show detailed progress output")
	pflags.Duration("timeout", 5*time.Minute, "timeout for schema extraction")

	// src mode flags
	pflags.String("entry", "", "provider entry point as import/path.FuncName (src mode, auto-detected by default)")
	pflags.Bool("keep-shim", false, "keep the generated shim package in the provider repo for debugging (src mode)")
	pflags.Bool("ssa", false, "recover literal validator/default arguments via static call-site analysis (src mode)")

	// binding map for viper/pflag -> env
	m := map[string]string{
		"out":       "TFSCHEMAEX_OUT",
		"entry":     "TFSCHEMAEX_ENTRY",
		"keep-shim": "",
		"ssa":       "TFSCHEMAEX_SSA",
		"timeout":   "TFSCHEMAEX_TIMEOUT",
		"silent":    "TFSCHEMAEX_SILENT",
		"verbose":   "",
	}

	for name, env := range m {
		if err := viper.BindPFlag(name, pflags.Lookup(name)); err != nil {
			return fmt.Errorf("error binding '%s' flag: %w", name, err)
		}

		if env != "" {
			if err := viper.BindEnv(name, env); err != nil {
				return fmt.Errorf("error binding '%s' to env '%s' : %w", name, env, err)
			}
		}
	}

	viper.SetConfigName(".tfschemaex")
	viper.SetConfigType("env")
	if home, err := os.UserHomeDir(); err == nil {
		viper.AddConfigPath(home)
	}
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			clog.Log.Errorf("Error reading config file: %v", err)
		}
	}

	return nil
}

// GetFlags returns the fully populated FlagData, unmarshalled from viper so env var and
// config file overrides are applied on top of command line flags.
func GetFlags() *FlagData {
	var f FlagData
	if err := viper.Unmarshal(&f); err != nil {
		clog.Log.Fatalf("failed to unmarshal configuration: %v", err)
	}

	return &f
}
