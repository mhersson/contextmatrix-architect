package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the architect backend",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("serve: not wired yet")
		},
	}
	cmd.Flags().StringVar(&path, "config", "config.yaml", "config file path")

	return cmd
}
