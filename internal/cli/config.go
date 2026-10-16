package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-architect/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect configuration"}

	cmd.AddCommand(&cobra.Command{
		Use:   "defaults",
		Short: "Print every config key with its default",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := config.DefaultsYAML()
			if err != nil {
				return err
			}

			_, err = fmt.Fprint(cmd.OutOrStdout(), string(out))

			return err
		},
	})

	var path string

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Load and validate a config file plus the environment",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			if err := cfg.Validate(); err != nil {
				return err
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), "config ok")

			return err
		},
	}
	validate.Flags().StringVar(&path, "config", "config.yaml", "config file path")
	cmd.AddCommand(validate)

	return cmd
}
