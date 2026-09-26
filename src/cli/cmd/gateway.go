package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/client"
)

// Subscription-lane verbs over /api/v1 (PRD F1–F25). Output is JSON on stdout
// so agents can pipe it; human hints go to stderr. Secret values are read
// from stdin, never argv.

func apiClient() (*client.Client, error) {
	if resolvedCfg.ServerURL == "" {
		return nil, fmt.Errorf("server URL not set — use --server, GATEWAY_URL, or set serverUrl in config")
	}
	return client.New(strings.TrimRight(resolvedCfg.ServerURL, "/"), resolvedCfg), nil
}

// ownerFlag selects the gateway owner (ADR 0005); default $GATEWAY_OWNER.
var ownerFlag string

// scoped maps an owner-relative management path (/api/v1/apis/…) onto
// /api/v1/owners/{owner}/…; /api/v1/me/… and /api/v1/owners… pass through.
func scoped(path string) (string, error) {
	rest, ok := strings.CutPrefix(path, "/api/v1/")
	if !ok || strings.HasPrefix(rest, "me/") || rest == "owners" || strings.HasPrefix(rest, "owners/") {
		return path, nil
	}
	if ownerFlag == "" {
		return "", fmt.Errorf("owner not set — use --owner or GATEWAY_OWNER (see 'gateway-cli owner list')")
	}
	return "/api/v1/owners/" + esc(ownerFlag) + "/" + rest, nil
}

// doJSON is c.JSON on an owner-scoped path.
func doJSON(cmd *cobra.Command, c *client.Client, method, path string, body, out any) error {
	p, err := scoped(path)
	if err != nil {
		return err
	}
	return c.JSON(cmd.Context(), method, p, body, out)
}

// call runs one request and prints the JSON response.
func call(cmd *cobra.Command, method, path string, body any) error {
	c, err := apiClient()
	if err != nil {
		return err
	}
	var out json.RawMessage
	if err := doJSON(cmd, c, method, path, body, &out); err != nil {
		return err
	}
	if len(out) > 0 {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return nil
}

func esc(s string) string { return url.PathEscape(s) }

func run(method string, path func(args []string) string, body func(cmd *cobra.Command, args []string) (any, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		var b any
		if body != nil {
			var err error
			if b, err = body(cmd, args); err != nil {
				return err
			}
		}
		return call(cmd, method, path(args), b)
	}
}

func fixed(p string) func([]string) string { return func([]string) string { return p } }

func group(use, short string, children ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short}
	c.AddCommand(children...)
	return c
}

func leaf(use, short string, nargs int, fn func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.ExactArgs(nargs), RunE: fn}
}

func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readSecretStdin(what string) (string, error) {
	if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintf(os.Stderr, "Enter %s (end with newline): ", what)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	v := strings.TrimRight(line, "\r\n")
	if v == "" {
		return "", fmt.Errorf("empty %s on stdin", what)
	}
	return v, nil
}

// parseOp turns "id=METHOD /path" into an operation.
func parseOp(s string) (map[string]any, error) {
	id, rest, ok := strings.Cut(s, "=")
	method, path, ok2 := strings.Cut(strings.TrimSpace(rest), " ")
	if !ok || !ok2 {
		return nil, fmt.Errorf("--op %q: want id=METHOD /path", s)
	}
	return map[string]any{"id": id, "method": strings.ToUpper(method), "path": strings.TrimSpace(path)}, nil
}

// --- api / op ---

func apiCommands() *cobra.Command {
	create := leaf("create <id>", "Register an upstream API", 1, nil)
	upstream := create.Flags().String("upstream", "", "upstream: https://… or runner://<connector>/<slot>[/base] (required)")
	cred := create.Flags().String("credential", "", "credential id injected upstream")
	name := create.Flags().String("name", "", "display name")
	kind := create.Flags().String("kind", "http", "http | llm")
	desc := create.Flags().String("description", "", "description")
	ops := create.Flags().StringArray("op", nil, `operation "id=METHOD /path" (repeatable)`)
	_ = create.MarkFlagRequired("upstream")
	create.RunE = run("POST", fixed("/api/v1/apis"), func(_ *cobra.Command, a []string) (any, error) {
		body := map[string]any{"id": a[0], "upstream": *upstream, "credential": *cred, "name": *name, "kind": *kind, "description": *desc}
		var list []map[string]any
		for _, s := range *ops {
			op, err := parseOp(s)
			if err != nil {
				return nil, err
			}
			list = append(list, op)
		}
		body["operations"] = list
		return body, nil
	})

	set := leaf("set <id>", "Change an API's upstream, credential, name or description", 1, nil)
	sUp := set.Flags().String("upstream", "", "")
	sCred := set.Flags().String("credential", "", "")
	sName := set.Flags().String("name", "", "")
	sDesc := set.Flags().String("description", "", "")
	set.RunE = run("PATCH", func(a []string) string { return "/api/v1/apis/" + esc(a[0]) }, func(cmd *cobra.Command, _ []string) (any, error) {
		p := map[string]any{}
		for flag, v := range map[string]*string{"upstream": sUp, "credential": sCred, "name": sName, "description": sDesc} {
			if cmd.Flags().Changed(flag) {
				p[flag] = *v
			}
		}
		return p, nil
	})

	imp := leaf("import <id> <openapi.json>", "Replace an API's operations from an OpenAPI 3 JSON file", 2, func(cmd *cobra.Command, a []string) error {
		b, err := os.ReadFile(a[1])
		if err != nil {
			return err
		}
		if !json.Valid(b) {
			return fmt.Errorf("%s is not JSON (convert YAML first)", a[1])
		}
		return call(cmd, "POST", "/api/v1/apis/"+esc(a[0])+"/openapi", json.RawMessage(b))
	})

	return group("api", "Manage upstream APIs and their operations",
		leaf("list", "List APIs", 0, run("GET", fixed("/api/v1/apis"), nil)),
		leaf("get <id>", "Show an API", 1, run("GET", func(a []string) string { return "/api/v1/apis/" + esc(a[0]) }, nil)),
		create, set, imp,
		leaf("delete <id>", "Delete an API", 1, run("DELETE", func(a []string) string { return "/api/v1/apis/" + esc(a[0]) }, nil)),
	)
}

func opCommands() *cobra.Command {
	put := leaf("put <api> <op-id> <METHOD> <path>", "Declare or replace an operation", 4, nil)
	summary := put.Flags().String("summary", "", "summary (becomes the MCP tool description)")
	schema := put.Flags().String("schema", "", "JSON Schema file for the tool input")
	put.RunE = run("PUT", func(a []string) string { return "/api/v1/apis/" + esc(a[0]) + "/operations/" + esc(a[1]) },
		func(_ *cobra.Command, a []string) (any, error) {
			body := map[string]any{"method": strings.ToUpper(a[2]), "path": a[3], "summary": *summary}
			if *schema != "" {
				b, err := os.ReadFile(*schema)
				if err != nil {
					return nil, err
				}
				body["inputSchema"] = json.RawMessage(b)
			}
			return body, nil
		})
	return group("op", "Manage an API's declared operations (undeclared routes are refused)",
		put,
		leaf("rm <api> <op-id>", "Remove an operation", 2, run("DELETE", func(a []string) string {
			return "/api/v1/apis/" + esc(a[0]) + "/operations/" + esc(a[1])
		}, nil)),
	)
}

// --- mcp ---

func mcpCommands() *cobra.Command {
	create := leaf("create <id>", "Expose an API's operations as an MCP server, or front a remote MCP server", 1, nil)
	api := create.Flags().String("api", "", "API whose operations become tools")
	ops := create.Flags().String("ops", "*", `operation ids to expose, comma-separated, or "*"`)
	remote := create.Flags().String("remote", "", "remote Streamable-HTTP MCP URL (instead of --api)")
	cred := create.Flags().String("credential", "", "credential for the remote server")
	allow := create.Flags().String("allow", "", "remote tools allowed, comma-separated (empty = all)")
	desc := create.Flags().String("description", "", "server instructions / description")
	create.RunE = run("POST", fixed("/api/v1/mcp-servers"), func(_ *cobra.Command, a []string) (any, error) {
		body := map[string]any{"id": a[0], "description": *desc}
		switch {
		case *remote != "" && *api != "":
			return nil, fmt.Errorf("use --api or --remote, not both")
		case *remote != "":
			body["source"], body["remoteUrl"], body["credential"], body["allowTools"] = "remote", *remote, *cred, csv(*allow)
		case *api != "":
			body["source"], body["api"], body["operations"] = "api", *api, csv(*ops)
		default:
			return nil, fmt.Errorf("--api or --remote is required")
		}
		return body, nil
	})
	return group("mcp", "Manage MCP servers",
		leaf("list", "List MCP servers", 0, run("GET", fixed("/api/v1/mcp-servers"), nil)),
		leaf("get <id>", "Show an MCP server", 1, run("GET", func(a []string) string { return "/api/v1/mcp-servers/" + esc(a[0]) }, nil)),
		create,
		leaf("delete <id>", "Delete an MCP server", 1, run("DELETE", func(a []string) string { return "/api/v1/mcp-servers/" + esc(a[0]) }, nil)),
	)
}

// --- bundle ---

func bundleCommands() *cobra.Command {
	create := leaf("create <id>", "Create a bundle of APIs and MCP servers", 1, nil)
	apis := create.Flags().String("apis", "", "API ids, comma-separated")
	mcps := create.Flags().String("mcp", "", "MCP server ids, comma-separated")
	name := create.Flags().String("name", "", "display name")
	desc := create.Flags().String("description", "", "description")
	hidden := create.Flags().Bool("hidden", false, "hide from the catalog")
	create.RunE = run("POST", fixed("/api/v1/bundles"), func(_ *cobra.Command, a []string) (any, error) {
		return map[string]any{"id": a[0], "apis": csv(*apis), "mcpServers": csv(*mcps), "name": *name, "description": *desc, "hidden": *hidden}, nil
	})
	item := func(method string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, a []string) error {
			var kind string
			switch a[1] {
			case "api":
				kind = "apis"
			case "mcp":
				kind = "mcp-servers"
			default:
				return fmt.Errorf("kind must be api or mcp")
			}
			return call(cmd, method, "/api/v1/bundles/"+esc(a[0])+"/"+kind+"/"+esc(a[2]), nil)
		}
	}
	vis := func(hidden bool) func(*cobra.Command, []string) error {
		return run("POST", func(a []string) string { return "/api/v1/bundles/" + esc(a[0]) + "/visibility" },
			func(*cobra.Command, []string) (any, error) { return map[string]bool{"hidden": hidden}, nil })
	}
	return group("bundle", "Manage bundles (what a subscription grants)",
		leaf("list", "List bundles", 0, run("GET", fixed("/api/v1/bundles"), nil)),
		leaf("get <id>", "Show a bundle", 1, run("GET", func(a []string) string { return "/api/v1/bundles/" + esc(a[0]) }, nil)),
		create,
		leaf("add <bundle> api|mcp <id>", "Add an API or MCP server", 3, item("PUT")),
		leaf("remove <bundle> api|mcp <id>", "Remove an API or MCP server", 3, item("DELETE")),
		leaf("hide <id>", "Hide from the catalog", 1, vis(true)),
		leaf("show <id>", "Show in the catalog", 1, vis(false)),
		leaf("delete <id>", "Delete a bundle", 1, run("DELETE", func(a []string) string { return "/api/v1/bundles/" + esc(a[0]) }, nil)),
	)
}

// --- principal ---

func principalCommands() *cobra.Command {
	create := leaf("create <id>", "Register a person or service", 1, nil)
	sub := create.Flags().String("sub", "", "OIDC subject (needed for MCP OAuth and self-service)")
	email := create.Flags().String("email", "", "email")
	name := create.Flags().String("name", "", "display name")
	kind := create.Flags().String("kind", "human", "human | service")
	create.RunE = run("POST", fixed("/api/v1/principals"), func(_ *cobra.Command, a []string) (any, error) {
		return map[string]any{"id": a[0], "sub": *sub, "email": *email, "name": *name, "kind": *kind}, nil
	})
	p := func(verb string) func([]string) string {
		return func(a []string) string { return "/api/v1/principals/" + esc(a[0]) + "/" + verb }
	}
	return group("principal", "Manage principals (who can hold subscriptions)",
		leaf("list", "List principals", 0, run("GET", fixed("/api/v1/principals"), nil)),
		leaf("get <id>", "Show a principal", 1, run("GET", func(a []string) string { return "/api/v1/principals/" + esc(a[0]) }, nil)),
		create,
		leaf("disable <id>", "Disable (all their subscriptions stop working)", 1, run("POST", p("disable"), nil)),
		leaf("enable <id>", "Re-enable", 1, run("POST", p("enable"), nil)),
	)
}

// --- sub ---

type issuedKeys struct {
	Subscription struct {
		ID     string `json:"id"`
		Bundle string `json:"bundle"`
	} `json:"subscription"`
	PrimaryKey   string `json:"primaryKey"`
	SecondaryKey string `json:"secondaryKey"`
}

func subCommands() *cobra.Command {
	list := leaf("list", "List subscriptions", 0, nil)
	lp := list.Flags().String("principal", "", "filter by principal")
	lb := list.Flags().String("bundle", "", "filter by bundle")
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		q := url.Values{}
		if *lp != "" {
			q.Set("principal", *lp)
		}
		if *lb != "" {
			q.Set("bundle", *lb)
		}
		return call(cmd, "GET", "/api/v1/subscriptions?"+q.Encode(), nil)
	}

	create := leaf("create", "Subscribe a principal to a bundle (keys are printed once)", 0, nil)
	bundle := create.Flags().String("bundle", "", "bundle id (required)")
	principal := create.Flags().String("principal", "", "principal id (required)")
	id := create.Flags().String("id", "", "subscription id (default <principal>-<bundle>)")
	expires := create.Flags().String("expires", "", "RFC 3339 expiry")
	asEnv := create.Flags().Bool("env", false, "print the subscriber's env/.mcp.json block instead of JSON")
	_ = create.MarkFlagRequired("bundle")
	_ = create.MarkFlagRequired("principal")
	create.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := apiClient()
		if err != nil {
			return err
		}
		body := map[string]any{"bundle": *bundle, "principal": *principal}
		if *id != "" {
			body["id"] = *id
		}
		if *expires != "" {
			body["expiresAt"] = *expires
		}
		var raw json.RawMessage
		if err := doJSON(cmd, c, "POST", "/api/v1/subscriptions", body, &raw); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "# Keys are shown once and stored only as hashes. Save them now.")
		if !*asEnv {
			fmt.Println(string(raw))
			return nil
		}
		var k issuedKeys
		_ = json.Unmarshal(raw, &k)
		return printSubscriberEnv(cmd, c.BaseURL, ownerFlag, k.PrimaryKey)
	}

	regen := leaf("regenerate <id>", "Replace one key slot (the other keeps working)", 1, nil)
	slot := regen.Flags().String("slot", "secondary", "primary | secondary")
	self := regen.Flags().Bool("self", false, "use the self-service endpoint (your own subscription)")
	regen.RunE = run("POST", func(a []string) string {
		if *self {
			return "/api/v1/me/subscriptions/" + esc(ownerFlag) + "/" + esc(a[0]) + "/regenerate"
		}
		return "/api/v1/subscriptions/" + esc(a[0]) + "/regenerate"
	}, func(*cobra.Command, []string) (any, error) { return map[string]string{"slot": *slot}, nil })

	env := leaf("env", "Print base URL, key and .mcp.json for a subscription key (key from GATEWAY_KEY or stdin; needs --owner, no login)", 0, func(cmd *cobra.Command, _ []string) error {
		if resolvedCfg.ServerURL == "" {
			return fmt.Errorf("server URL not set — use --server or GATEWAY_URL")
		}
		if ownerFlag == "" {
			return fmt.Errorf("owner not set — use --owner or GATEWAY_OWNER")
		}
		key := os.Getenv("GATEWAY_KEY")
		if key == "" {
			var err error
			if key, err = readSecretStdin("subscription key"); err != nil {
				return err
			}
		}
		if _, ok := subIDFromKey(key); !ok {
			return fmt.Errorf("not a gateway key (want gwk_<subscription>_<secret>)")
		}
		return printSubscriberEnv(cmd, strings.TrimRight(resolvedCfg.ServerURL, "/"), ownerFlag, key)
	})

	t := func(verb string) *cobra.Command {
		return leaf(verb+" <id>", strings.ToUpper(verb[:1])+verb[1:]+" a subscription", 1,
			run("POST", func(a []string) string { return "/api/v1/subscriptions/" + esc(a[0]) + "/" + verb }, nil))
	}
	return group("sub", "Manage subscriptions and their keys",
		list,
		leaf("get <id>", "Show a subscription", 1, run("GET", func(a []string) string { return "/api/v1/subscriptions/" + esc(a[0]) }, nil)),
		leaf("mine", "List your own subscriptions", 0, run("GET", fixed("/api/v1/me/subscriptions"), nil)),
		create, t("suspend"), t("reactivate"), t("cancel"), regen, env,
	)
}

func subIDFromKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "gwk_")
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, "_")
	return id, ok && id != ""
}

// printSubscriberEnv emits what a subscriber needs: shell exports on stdout,
// with the bundle's API base URLs and a ready .mcp.json on stderr. It reads
// the key-authenticated /apis/{owner}/_catalog, so no operator login.
func printSubscriberEnv(cmd *cobra.Command, base, owner, key string) error {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, base+"/apis/"+esc(owner)+"/_catalog", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Api-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("catalog: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var cat struct {
		Subscription string `json:"subscription"`
		Bundle       struct {
			Apis []struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			} `json:"apis"`
			McpServers []struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			} `json:"mcpServers"`
		} `json:"bundle"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	fmt.Printf("export GATEWAY_URL=%q\n", base)
	fmt.Printf("export GATEWAY_KEY=%q\n", key)
	for _, a := range cat.Bundle.Apis {
		fmt.Printf("# api %s: curl -H \"Api-Key: $GATEWAY_KEY\" %s%s/<operation path>\n", a.ID, base, a.Path)
	}
	servers := map[string]any{}
	for _, m := range cat.Bundle.McpServers {
		servers[m.ID] = map[string]any{"type": "http", "url": base + m.Path, "headers": map[string]string{"Api-Key": key}}
	}
	if len(servers) > 0 {
		j, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
		fmt.Fprintln(os.Stderr, "# .mcp.json fragment (contains the key — keep it out of git):")
		fmt.Fprintln(os.Stderr, string(j))
	}
	return nil
}

// --- cred ---

func credCommands() *cobra.Command {
	create := leaf("create <id>", "Register an upstream credential (sealed values are read from stdin)", 1, nil)
	source := create.Flags().String("source", "", "env | sealed | entra | vault (required)")
	header := create.Flags().String("header", "", "inject into this header")
	query := create.Flags().String("query", "", "inject into this query parameter")
	bearer := create.Flags().Bool("bearer", false, "inject as Authorization: Bearer")
	envVar := create.Flags().String("var", "", "env: variable name on the gateway")
	tenant := create.Flags().String("tenant", "", "entra: tenant id")
	clientID := create.Flags().String("client-id", "", "entra: app (client) id")
	clientSecret := create.Flags().String("client-secret", "", "entra: id of the credential holding the client secret")
	scope := create.Flags().String("scope", "", "entra: scope, e.g. https://cognitiveservices.azure.com/.default")
	item := create.Flags().String("item", "", "vault: <owner>/<item>")
	_ = create.MarkFlagRequired("source")
	create.RunE = run("POST", fixed("/api/v1/credentials"), func(_ *cobra.Command, a []string) (any, error) {
		body := map[string]any{"id": a[0], "source": *source,
			"inject": map[string]any{"header": *header, "query": *query, "bearer": *bearer},
			"var":    *envVar, "tenant": *tenant, "clientId": *clientID, "clientSecret": *clientSecret, "scope": *scope, "item": *item}
		if *source == "sealed" {
			v, err := readSecretStdin("secret value")
			if err != nil {
				return nil, err
			}
			body["value"] = v
		}
		return body, nil
	})
	rotate := leaf("rotate <id>", "Replace a sealed value from stdin, or drop a cached token", 1, nil)
	sealed := rotate.Flags().Bool("value", false, "read a new sealed value from stdin")
	rotate.RunE = run("POST", func(a []string) string { return "/api/v1/credentials/" + esc(a[0]) + "/rotate" },
		func(*cobra.Command, []string) (any, error) {
			if !*sealed {
				return map[string]any{}, nil
			}
			v, err := readSecretStdin("new secret value")
			return map[string]any{"value": v}, err
		})
	return group("cred", "Manage upstream credentials (values are never shown)",
		leaf("list", "List credentials", 0, run("GET", fixed("/api/v1/credentials"), nil)),
		leaf("get <id>", "Show credential metadata", 1, run("GET", func(a []string) string { return "/api/v1/credentials/" + esc(a[0]) }, nil)),
		create, rotate,
		leaf("delete <id>", "Delete a credential", 1, run("DELETE", func(a []string) string { return "/api/v1/credentials/" + esc(a[0]) }, nil)),
	)
}

// --- usage / catalog ---

func usageCommand() *cobra.Command {
	cmd := leaf("usage", "Show usage totals (per subscription)", 0, nil)
	flags := map[string]*string{}
	for _, f := range []string{"subscription", "bundle", "principal", "api", "from", "to"} {
		flags[f] = cmd.Flags().String(f, "", "filter by "+f+map[bool]string{true: " (YYYY-MM-DD)"}[f == "from" || f == "to"])
	}
	records := cmd.Flags().Bool("records", false, "include individual records")
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		q := url.Values{}
		for k, v := range flags {
			if *v != "" {
				q.Set(k, *v)
			}
		}
		if *records {
			q.Set("records", "1")
		}
		return call(c, http.MethodGet, "/api/v1/usage?"+q.Encode(), nil)
	}
	return cmd
}

// --- owner ---

func ownerCommands() *cobra.Command {
	return group("owner", "Claim and administer gateway owners (tenants)",
		leaf("list", "List the owners you administer", 0, run("GET", fixed("/api/v1/owners"), nil)),
		leaf("claim <id>", "Claim a new owner; you become its first admin", 1, run("POST", fixed("/api/v1/owners"),
			func(_ *cobra.Command, a []string) (any, error) { return map[string]string{"id": a[0]}, nil })),
		leaf("get <id>", "Show an owner and its admins", 1, run("GET", func(a []string) string { return "/api/v1/owners/" + esc(a[0]) }, nil)),
		leaf("admin-add <id> <sub>", "Add an OIDC subject as admin", 2, run("PUT", func(a []string) string {
			return "/api/v1/owners/" + esc(a[0]) + "/admins/" + esc(a[1])
		}, nil)),
		leaf("admin-rm <id> <sub>", "Remove an admin (the last one stays)", 2, run("DELETE", func(a []string) string {
			return "/api/v1/owners/" + esc(a[0]) + "/admins/" + esc(a[1])
		}, nil)),
	)
}

// --- connector ---

func connectorCommands() *cobra.Command {
	create := leaf("create <id>", "Create a runner connector (tokens are printed once)", 1, nil)
	name := create.Flags().String("name", "", "display name")
	create.RunE = func(cmd *cobra.Command, a []string) error {
		c, err := apiClient()
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := doJSON(cmd, c, "POST", "/api/v1/connectors", map[string]string{"id": a[0], "name": *name}, &raw); err != nil {
			return err
		}
		fmt.Println(string(raw))
		fmt.Fprintln(os.Stderr, "# Tokens are shown once and stored only as hashes. On the runner machine:")
		fmt.Fprintf(os.Stderr, "#   agent-tunnel host --server %s/connect --owner %s --name %s --token <primaryToken> --http <slot>=127.0.0.1:<port>\n",
			wsURL(c.BaseURL), ownerFlag, a[0])
		fmt.Fprintf(os.Stderr, "# then point an API at it: gateway-cli api create <id> --upstream runner://%s/<slot>[/base]\n", a[0])
		return nil
	}
	regen := leaf("regenerate <id>", "Replace one token slot; live sessions must re-register", 1, nil)
	slot := regen.Flags().String("slot", "secondary", "primary | secondary")
	regen.RunE = run("POST", func(a []string) string { return "/api/v1/connectors/" + esc(a[0]) + "/regenerate" },
		func(*cobra.Command, []string) (any, error) { return map[string]string{"slot": *slot}, nil })
	v := func(verb, short string) *cobra.Command {
		return leaf(verb+" <id>", short, 1, run("POST", func(a []string) string { return "/api/v1/connectors/" + esc(a[0]) + "/" + verb }, nil))
	}
	return group("connector", "Manage runner connectors (runner:// upstreams over the tunnel protocol)",
		leaf("list", "List connectors with presence", 0, run("GET", fixed("/api/v1/connectors"), nil)),
		leaf("get <id>", "Show a connector with presence", 1, run("GET", func(a []string) string { return "/api/v1/connectors/" + esc(a[0]) }, nil)),
		create,
		leaf("delete <id>", "Delete a connector (refused while an API uses it)", 1, run("DELETE", func(a []string) string { return "/api/v1/connectors/" + esc(a[0]) }, nil)),
		v("disable", "Disable a connector and drop its sessions"), v("enable", "Enable a connector"),
		regen,
	)
}

func wsURL(base string) string {
	if rest, ok := strings.CutPrefix(base, "https://"); ok {
		return "wss://" + rest
	}
	if rest, ok := strings.CutPrefix(base, "http://"); ok {
		return "ws://" + rest
	}
	return base
}

func init() {
	rootCmd.PersistentFlags().StringVar(&ownerFlag, "owner", os.Getenv("GATEWAY_OWNER"), "gateway owner (tenant); default $GATEWAY_OWNER")
	rootCmd.AddCommand(ownerCommands(), connectorCommands(), apiCommands(), opCommands(), mcpCommands(), bundleCommands(),
		principalCommands(), subCommands(), credCommands(), usageCommand(),
		leaf("catalog", "List the owner's bundles and what they contain", 0, run("GET", fixed("/api/v1/catalog"), nil)))
}
