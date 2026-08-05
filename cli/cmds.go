// Package cli implements the tfschemaex command line interface: the cobra commands,
// flag and config handling, and the two schema extraction workflows (plugin & src).
package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/katbyte/tfschemaex/lib/cout"
	"github.com/katbyte/tfschemaex/lib/enrich"
	"github.com/katbyte/tfschemaex/lib/plugin"
	"github.com/katbyte/tfschemaex/lib/schemajson"
	"github.com/katbyte/tfschemaex/lib/src"
	"github.com/katbyte/tfschemaex/lib/version"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func Make() (*cobra.Command, error) {
	root := &cobra.Command{
		Use:   "tfschemaex [command]",
		Short: "tfschemaex extracts complete schemas from terraform providers",
		Long: `A small utility to extract as much schema detail as possible from any terraform provider.

plugin mode launches a provider binary exactly like terraform does (go-plugin handshake,
gRPC, protocol 5/6) and dumps everything the plugin protocol exposes.

src mode points at a provider source checkout, compiles a small shim against it, and
dumps the full SDK-level schema including ForceNew, defaults, validators, conflicts and
the source file/line of every resource function - detail that never crosses the wire.

Complete documentation is available at https://github.com/katbyte/tfschemaex`,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			switch {
			case viper.GetBool("silent"):
				cout.Level = cout.VerbositySilent
			case viper.GetBool("verbose"):
				cout.Level = cout.VerbosityVerbose
			}
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Printf("Run \"tfschemaex help\" for more information about available tfschemaex commands.\n")
			return nil
		},
	}

	root.AddCommand(&cobra.Command{
		Use:           "version",
		Short:         "Print the version number of tfschemaex",
		Long:          `Print the version number of tfschemaex`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("tfschemaex " + version.Version)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:     "plugin provider-binary",
		Short:   "extracts everything the plugin protocol exposes from a provider binary",
		Aliases: []string{"p", "bin"},
		Long: `Launches the provider binary the same way terraform does (go-plugin handshake,
AutoMTLS, gRPC protocol 5/6 negotiation), calls GetProviderSchema and dumps resources,
data sources, ephemeral resources, list resources, actions, functions, identity schemas
and server capabilities as JSON.

Note the protocol does not carry ForceNew, defaults, validators or ConflictsWith - use
src mode against the provider's source for those.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			binary := args[0]
			if _, err := os.Stat(binary); err != nil {
				return fmt.Errorf("provider binary %s: %w", binary, err)
			}

			cmd.SilenceUsage = true

			ctx, cancel := context.WithTimeout(cmd.Context(), viper.GetDuration("timeout"))
			defer cancel()

			cout.Printf("extracting schema from <cyan>%s</> via plugin protocol...\n", binary)
			dump, protocol, err := plugin.Extract(ctx, binary)
			if err != nil {
				return err
			}
			cout.Printf("  negotiated protocol <green>%d</>: <yellow>%d</> resources, <yellow>%d</> data sources, <yellow>%d</> ephemeral, <yellow>%d</> functions\n",
				protocol, len(dump.Resources), len(dump.DataSources), len(dump.EphemeralResources), len(dump.Functions))

			out := &schemajson.Dump{
				FormatVersion:   schemajson.FormatVersion,
				Tool:            "tfschemaex",
				ToolVersion:     version.Version,
				Mode:            "plugin",
				Source:          binary,
				ProtocolVersion: protocol,
				Plugin:          dump,
			}

			return writeDump(out)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:     "src provider-repo-path",
		Short:   "extracts the full SDK-level schema from a provider source checkout",
		Aliases: []string{"s", "source"},
		Long: `Locates the provider's entry point (a zero-arg func returning *schema.Provider),
generates a small shim inside the provider module, compiles and runs it with the
provider's own go.mod/vendor, and dumps the complete SDK-level schema: ForceNew,
defaults, validators, ConflictsWith/ExactlyOneOf/AtLeastOneOf/RequiredWith, diff
suppression, state upgraders, timeouts, and the source file:line of every CRUD,
validate and default function.

The entry point is auto-detected; override it with --entry, e.g.
  --entry github.com/hashicorp/terraform-provider-azurerm/internal/provider.AzureProvider

With --ssa, an additional static call-site analysis pass parses the provider source
(and any vendored packages the schema references) to recover the literal arguments
inside validator/default constructors - the 1 and 100 in validation.IntBetween(1, 100),
the string list in StringInSlice - which are closure-captured at runtime and invisible
to reflection. Unique matches gain a "call" object with args and call file:line;
ambiguous matches list "call_candidates" instead.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
				return fmt.Errorf("provider repo path %s is not a directory (%w)", repo, err)
			}

			cmd.SilenceUsage = true

			cout.Printf("extracting schema from <cyan>%s</> via source shim...\n", repo)
			payload, entry, err := src.Extract(cmd.Context(), repo, viper.GetString("entry"), viper.GetBool("keep-shim"))
			if err != nil {
				return err
			}

			ssa := viper.GetBool("ssa")
			if ssa {
				root, modPath, err := src.FindModule(repo)
				if err != nil {
					return err
				}

				cout.Printf("  running --ssa call-site analysis...\n")
				enriched, stats, err := enrich.Payload(payload, root, modPath)
				if err != nil {
					return err
				}
				payload = enriched

				cout.Printf("    <yellow>%d</> constructor-built funcs: args recovered for <green>%d</> <gray>(+%d via identical sites)</>, <yellow>%d</> ambiguous, <red>%d</> unlocated <gray>(%d files scanned)</>\n",
					stats.Refs, stats.Matched, stats.MatchedSame, stats.Ambiguous, stats.Unlocated, stats.FilesScanned)
			}

			out := &schemajson.Dump{
				FormatVersion: schemajson.FormatVersion,
				Tool:          "tfschemaex",
				ToolVersion:   version.Version,
				Mode:          "src",
				Source:        repo,
				Entry:         entry.String(),
				SDK:           "sdkv2",
				SSA:           ssa,
				Src:           payload,
			}

			return writeDump(out)
		},
	})

	if err := configureFlags(root); err != nil {
		return nil, fmt.Errorf("unable to configure flags: %w", err)
	}

	return root, nil
}

func writeDump(d *schemajson.Dump) error {
	out := viper.GetString("out")

	start := time.Now()
	if err := d.WriteFile(out); err != nil {
		return err
	}

	if out != "" && out != "-" {
		if fi, err := os.Stat(out); err == nil {
			cout.Printf("wrote <green>%s</> <gray>(%.1f MB in %s)</>\n", out, float64(fi.Size())/1024/1024, time.Since(start).Round(time.Millisecond))
		}
	}

	return nil
}
