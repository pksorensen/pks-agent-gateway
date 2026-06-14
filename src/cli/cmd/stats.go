package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/client"
)

var (
	statsProject string
	statsSince   string
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show aggregated token/cost statistics for a project",
	Long:  `Fetches and displays per-model, per-user token usage and estimated cost.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := resolvedCfg
		if cfg.ServerURL == "" {
			return fmt.Errorf("server URL not set — use --server or set serverUrl in config")
		}

		project := statsProject
		if project == "" {
			project = cfg.CurrentProject
		}
		if project == "" {
			return fmt.Errorf("project not set — use --project or run `gateway-cli project select <id>`")
		}

		// Build query string.
		path := "/api/projects/" + project + "/stats"
		if statsSince != "" {
			// Validate the date loosely.
			if _, err := time.Parse("2006-01-02", statsSince); err != nil {
				return fmt.Errorf("--since must be in YYYY-MM-DD format: %w", err)
			}
			path += "?since=" + statsSince
		}

		c := client.New(cfg.ServerURL, cfg)
		var resp statsResponse
		if err := c.GetJSON(cmd.Context(), path, &resp); err != nil {
			return err
		}

		// Print per-model table.
		if len(resp.ByModel) > 0 {
			fmt.Fprintf(os.Stderr, "\nBy model:\n")
			fmt.Fprintf(os.Stderr, "%-40s  %12s  %12s  %10s\n", "Model", "Input tokens", "Output tokens", "Cost USD")
			fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 80))
			for _, row := range resp.ByModel {
				fmt.Printf("%-40s  %12d  %12d  %10.4f\n", row.Model, row.InputTokens, row.OutputTokens, row.CostUSD)
			}
		}

		// Print per-user table.
		if len(resp.ByUser) > 0 {
			fmt.Fprintf(os.Stderr, "\nBy user:\n")
			fmt.Fprintf(os.Stderr, "%-40s  %12s  %12s  %10s\n", "User", "Input tokens", "Output tokens", "Cost USD")
			fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 80))
			for _, row := range resp.ByUser {
				fmt.Printf("%-40s  %12d  %12d  %10.4f\n", row.User, row.InputTokens, row.OutputTokens, row.CostUSD)
			}
		}

		// Print totals.
		fmt.Fprintf(os.Stderr, "\nTotals:\n")
		fmt.Printf("Input tokens:  %d\n", resp.Totals.InputTokens)
		fmt.Printf("Output tokens: %d\n", resp.Totals.OutputTokens)
		fmt.Printf("Cost USD:      %.4f\n", resp.Totals.CostUSD)

		return nil
	},
}

type statsResponse struct {
	ByModel []modelRow `json:"byModel"`
	ByUser  []userRow  `json:"byUser"`
	Totals  totalsRow  `json:"totals"`
}

type modelRow struct {
	Model        string  `json:"model"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
}

type userRow struct {
	User         string  `json:"user"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
}

type totalsRow struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUsd"`
}

func init() {
	statsCmd.Flags().StringVar(&statsProject, "project", "", "Project ID or name (overrides current project in config)")
	statsCmd.Flags().StringVar(&statsSince, "since", "", "Show stats since this date (YYYY-MM-DD)")
}
