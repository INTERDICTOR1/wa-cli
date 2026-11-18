package cmd

import (
	"github.com/spf13/cobra"

	"github.com/codebyoketch/wa-cli/internal/tui"
)

// migrateCmd is started by the TUI itself, in its own process, the first
// time it runs after an upgrade from the old JSON stores (see
// tui.Migrate for why it isn't done in-process). Hidden: running it by
// hand is harmless but never needed.
var migrateCmd = &cobra.Command{
	Use:    tui.MigrateCommand,
	Short:  "Import the old JSON chat and message stores (run automatically)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return tui.Migrate(a)
	},
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}
