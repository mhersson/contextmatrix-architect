// Package cli holds the cobra commands.
package cli

import "github.com/spf13/cobra"

// NewRootCmd builds the architect command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "architect",
		Short:         "ContextMatrix architect backend",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newServeCmd())
	root.AddCommand(newConfigCmd())

	return root
}
