package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/auth"
)

var (
	envProject string
	envUser    string
	envNoProxy bool
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Print shell export block for routing Claude Code telemetry through the gateway",
	Long: `Prints eval-able shell exports to stdout.

Usage:
  eval $(gateway-cli env)
  eval $(gateway-cli env --project my-project --user alice@example.com)

This command does NOT require authentication — it reads config and flags only,
making it safe for ProjectAdmins to generate env blocks for consultants who
do not have (and do not need) a gateway account.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := resolvedCfg

		// Resolve server URL.
		serverURL := cfg.ServerURL
		if serverURL == "" {
			return fmt.Errorf("server URL not set — use --server, GATEWAY_URL, or set serverUrl in config")
		}

		// Resolve project.
		project := envProject
		if project == "" {
			project = cfg.CurrentProject
		}
		if project == "" {
			return fmt.Errorf("project not set — use --project or run `gateway-cli project select <id>`")
		}

		// Resolve user: flag > stored cred email.
		user := envUser
		if user == "" {
			if cred, err := auth.LoadCred(); err == nil && cred != nil {
				user = cred.Email
			}
		}

		// Emit export block to stdout so eval $(...) works.
		if !envNoProxy {
			fmt.Printf("export ANTHROPIC_BASE_URL=%q\n", serverURL)
		}
		fmt.Printf("export OTEL_EXPORTER_OTLP_ENDPOINT=%q\n", serverURL+"/otel")
		fmt.Printf("export OTEL_EXPORTER_OTLP_PROTOCOL=%q\n", "http/json")

		attrs := "project=" + project
		if user != "" {
			attrs += ",user=" + user
		}
		fmt.Printf("export OTEL_RESOURCE_ATTRIBUTES=%q\n", attrs)

		// Informational hint on stderr — does not interfere with eval.
		fmt.Fprintf(os.Stderr, "# gateway-cli env — project=%s", project)
		if user != "" {
			fmt.Fprintf(os.Stderr, " user=%s", user)
		}
		fmt.Fprintln(os.Stderr)

		return nil
	},
}

func init() {
	envCmd.Flags().StringVar(&envProject, "project", "", "Project ID or name (overrides current project in config)")
	envCmd.Flags().StringVar(&envUser, "user", "", "User identifier to tag in OTEL attributes (defaults to logged-in email)")
	envCmd.Flags().BoolVar(&envNoProxy, "no-proxy", false, "Omit ANTHROPIC_BASE_URL export (keep native Anthropic API, only route telemetry)")
}
