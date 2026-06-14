package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/auth"
	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/config"
)

var (
	loginIssuer   string
	loginClientID string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with the gateway via OIDC",
	Long: `Opens a browser window for OIDC login (PKCE Authorization Code flow).
If --issuer and --client-id are not supplied, the values from config are used;
if config is also empty you will be prompted and the values saved for next time.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := resolvedCfg

		issuer := loginIssuer
		if issuer == "" {
			issuer = cfg.OIDCIssuer
		}
		clientID := loginClientID
		if clientID == "" {
			clientID = cfg.OIDCClientID
		}

		// Prompt for missing values.
		if issuer == "" {
			issuer = prompt("OIDC Issuer URL: ")
		}
		if clientID == "" {
			clientID = prompt("OIDC Client ID: ")
		}

		tokens, err := auth.Login(cmd.Context(), issuer, clientID)
		if err != nil {
			return fmt.Errorf("login failed: %w", err)
		}

		// Persist credentials.
		cred := &auth.Cred{
			Sub:          tokens.Sub,
			Email:        tokens.Email,
			RefreshToken: tokens.Refresh,
		}
		if err := auth.SaveCred(cred); err != nil {
			return fmt.Errorf("saving credentials: %w", err)
		}

		// Save OIDC settings to config so future commands (refresh, etc.) work.
		cfg.OIDCIssuer = issuer
		cfg.OIDCClientID = clientID
		if err := config.Save(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save config: %v\n", err)
		}

		name := tokens.Name
		if name == "" {
			name = tokens.Email
		}
		fmt.Fprintf(os.Stderr, "Logged in as %s\n", name)
		return nil
	},
}

func init() {
	loginCmd.Flags().StringVar(&loginIssuer, "issuer", "", "OIDC issuer URL (overrides config)")
	loginCmd.Flags().StringVar(&loginClientID, "client-id", "", "OIDC client ID (overrides config)")
}

// prompt writes msg to stderr and reads a line from stdin.
func prompt(msg string) string {
	fmt.Fprint(os.Stderr, msg)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
}
