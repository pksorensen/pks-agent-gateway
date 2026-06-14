package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/auth"
)

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Remove stored credentials",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := auth.ClearCred(); err != nil {
			return fmt.Errorf("logout: %w", err)
		}
		fmt.Fprintln(os.Stderr, "Logged out.")
		return nil
	},
}
