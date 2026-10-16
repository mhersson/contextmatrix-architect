package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-architect/internal/config"
)

// newConfigCmd groups the commands contextmatrix-setup drives: defaults is the
// schema it keeps an installed file's key set equal to, validate answers
// whether serve would start with a file.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Print the default or validate a configuration"}

	cmd.AddCommand(&cobra.Command{
		Use:   "defaults",
		Short: "Print every config key with its default as YAML",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := config.DefaultsYAML()
			if err != nil {
				return err
			}

			_, err = cmd.OutOrStdout().Write(out)

			return err
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "validate <file>",
		Short: "Load <file> exactly as serve would; exit 1 on the first error",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			// Load accepts a missing file as defaults plus env, which serve
			// needs and validate must not: the question is about this file.
			if _, err := os.Stat(path); err != nil {
				return err
			}

			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("invalid config: %w", err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: ok\n", path)

			return err
		},
	})

	return cmd
}
