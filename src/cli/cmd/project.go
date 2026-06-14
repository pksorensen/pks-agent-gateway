package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/client"
	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/config"
)

// projectCmd is the parent sub-command group.
var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage gateway projects",
}

// --- project list ---

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := resolvedCfg
		if cfg.ServerURL == "" {
			return fmt.Errorf("server URL not set — use --server or set serverUrl in config")
		}
		c := client.New(cfg.ServerURL, cfg)

		var projects []projectResponse
		if err := c.GetJSON(cmd.Context(), "/api/projects", &projects); err != nil {
			return err
		}

		if len(projects) == 0 {
			fmt.Fprintln(os.Stderr, "No projects found.")
			return nil
		}

		// Print a simple table.
		fmt.Fprintf(os.Stderr, "%-36s  %-30s  %s\n", "ID", "Name", "Created")
		fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 80))
		for _, p := range projects {
			created := p.CreatedAt.Format(time.RFC3339)
			fmt.Printf("%-36s  %-30s  %s\n", p.ID, p.Name, created)
		}
		return nil
	},
}

// --- project create ---

var projectCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := resolvedCfg
		if cfg.ServerURL == "" {
			return fmt.Errorf("server URL not set — use --server or set serverUrl in config")
		}
		c := client.New(cfg.ServerURL, cfg)

		body := map[string]string{"name": args[0]}
		var created projectResponse
		if err := c.PostJSON(cmd.Context(), "/api/projects", body, &created); err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "Created project: %s (ID: %s)\n", created.Name, created.ID)
		return nil
	},
}

// --- project select ---

var projectSelectCmd = &cobra.Command{
	Use:   "select <id-or-name>",
	Short: "Set the current project in config",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := resolvedCfg
		idOrName := args[0]

		// Resolve the name against the server if we have a URL.
		if cfg.ServerURL != "" {
			c := client.New(cfg.ServerURL, cfg)
			var projects []projectResponse
			if err := c.GetJSON(cmd.Context(), "/api/projects", &projects); err == nil {
				for _, p := range projects {
					if p.ID == idOrName || strings.EqualFold(p.Name, idOrName) {
						idOrName = p.ID
						fmt.Fprintf(os.Stderr, "Selected project: %s\n", p.Name)
						goto save
					}
				}
				// Not found on server — store the raw value and warn.
				fmt.Fprintf(os.Stderr, "warning: project %q not found on server; storing as-is\n", idOrName)
			}
		}
		fmt.Fprintf(os.Stderr, "Selected project: %s\n", idOrName)

	save:
		cfg.CurrentProject = idOrName
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		return nil
	},
}

type projectResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

func init() {
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectCreateCmd)
	projectCmd.AddCommand(projectSelectCmd)
}
