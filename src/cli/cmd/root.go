package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/config"
)

var (
	// serverFlag is the --server persistent flag value.
	serverFlag string
	// configFlag is the --config persistent flag value (unused yet, reserved).
	configFlag string

	// resolvedCfg is loaded once by PersistentPreRunE and shared by sub-commands.
	resolvedCfg *config.Config
)

// rootCmd is the top-level cobra command.
var rootCmd = &cobra.Command{
	Use:   "gateway-cli",
	Short: "pks-agent-gateway administration CLI",
	Long: `gateway-cli manages projects and token routing for pks-agent-gateway.

Run 'gateway-cli login' to authenticate, then 'gateway-cli env' to generate
the shell exports that point Claude Code telemetry at the gateway.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		// --server flag overrides everything (env already handled inside Load).
		if serverFlag != "" {
			cfg.ServerURL = serverFlag
		}
		resolvedCfg = cfg
		return nil
	},
}

// Execute is the entry point called from main.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&serverFlag, "server", "",
		"Gateway server URL (overrides config and GATEWAY_URL env)")
	rootCmd.PersistentFlags().StringVar(&configFlag, "config", "",
		"Config file path (default ~/.config/gateway-cli/config.json)")

	rootCmd.SetErr(os.Stderr)
	rootCmd.SetOut(os.Stdout)

	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(logoutCmd)
	rootCmd.AddCommand(projectCmd)
	rootCmd.AddCommand(envCmd)
	rootCmd.AddCommand(statsCmd)
}
