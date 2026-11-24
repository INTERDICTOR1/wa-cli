package cmd

import (
	"fmt"

	"github.com/codebyoketch/wa-cli/internal/config"
	"github.com/codebyoketch/wa-cli/internal/update"
	"github.com/codebyoketch/wa-cli/internal/version"
	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the wa-cli version",
	Long:  "Print the wa-cli version, git commit, and build date.",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintln(cmd.OutOrStdout(), version.Get().String())
		if cfg, err := config.Load(); err == nil {
			if v := update.Newer(cmd.Context(), cfg.DataDir, version.Version); v != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Update available: %s. Run: %s\n", v, update.InstallCommand)
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
