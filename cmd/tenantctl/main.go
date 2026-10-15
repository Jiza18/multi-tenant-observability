// Command tenantctl manages tenant configuration:
//
//	tenantctl token -name acme-agents -tenant acme-prod -scopes metrics:write
//	    Generate a new random token and print its token-file entry.
//	tenantctl hash < token.txt
//	    Print the SHA-256 of an existing token.
//	tenantctl check-tokens tokens.yaml
//	    Validate a gateway token file before storing it.
//	tenantctl render
//	    Render Mimir overrides, Grafana datasources and the rule sync plan
//	    from tenants/tenants.yaml.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Jiza18/multi-tenant-observability/internal/auth"
	"github.com/Jiza18/multi-tenant-observability/internal/tenants"
)

// tokenPrefix makes tokens recognisable to secret scanners and humans.
const tokenPrefix = "mto_"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "token":
		err = cmdToken(args)
	case "hash":
		err = cmdHash()
	case "check-tokens":
		err = cmdCheckTokens(args)
	case "render":
		err = cmdRender(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tenantctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tenantctl <token|hash|check-tokens|render> [flags]")
	os.Exit(2)
}

func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	name := fs.String("name", "", "token name, shown in logs and metrics (required)")
	tenant := fs.String("tenant", "", "comma-separated tenants the token may use, or * (required)")
	scopes := fs.String("scopes", string(auth.ScopeMetricsWrite), "comma-separated scopes")
	_ = fs.Parse(args)
	if *name == "" || *tenant == "" {
		return errors.New("token: -name and -tenant are required")
	}
	// Validate now: a bad entry would make the gateway reject the whole file.
	for _, t := range splitList(*tenant) {
		if t != auth.AnyTenant && !auth.ValidTenantID(t) {
			return fmt.Errorf("token: invalid tenant %q", t)
		}
	}
	for _, s := range splitList(*scopes) {
		if !auth.Scope(s).Valid() {
			return fmt.Errorf("token: unknown scope %q (separate scopes with commas)", s)
		}
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)

	fmt.Fprintf(os.Stderr, "Token (shown once, hand it to the tenant through a secret store):\n\n  %s\n\n", token)
	fmt.Fprintln(os.Stderr, "Add this entry to the gateway token file:")
	fmt.Printf("  - name: %s\n    sha256: %s\n    tenants: [%s]\n    scopes: [%s]\n",
		*name, auth.Digest(token), quoteList(*tenant), quoteList(*scopes))
	return nil
}

func cmdHash() error {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("hash: read the token from stdin")
	}
	fmt.Println(auth.Digest(strings.TrimSpace(line)))
	return nil
}

func cmdCheckTokens(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: tenantctl check-tokens <tokens.yaml>")
	}
	set, _, err := auth.LoadFile(args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: %d valid tokens\n", args[0], set.Len())
	return nil
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	tenantsFile := fs.String("tenants", "tenants/tenants.yaml", "tenants file")
	rulesDir := fs.String("rules", "rules", "rules directory; receives sync-plan.tsv")
	outDir := fs.String("out", "deploy/kubernetes/base/generated", "directory for Mimir and Grafana configuration")
	mimirURL := fs.String("mimir-url", "http://mimir:8080", "Mimir URL as seen from Grafana")
	defaultTenant := fs.String("default-tenant", "platform", "tenant of Grafana's default datasource")
	_ = fs.Parse(args)

	f, err := tenants.Load(*tenantsFile)
	if err != nil {
		return err
	}
	plan, err := f.RulePlan(*rulesDir)
	if err != nil {
		return err
	}
	runtime, err := f.MimirRuntimeConfig()
	if err != nil {
		return err
	}
	datasources, err := f.GrafanaDatasources(*mimirURL, *defaultTenant)
	if err != nil {
		return err
	}

	outputs := map[string][]byte{
		filepath.Join(*outDir, "mimir-runtime.yaml"):       runtime,
		filepath.Join(*outDir, "grafana-datasources.yaml"): datasources,
		filepath.Join(*rulesDir, "sync-plan.tsv"):          tenants.RulePlanTSV(plan),
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil { //nolint:gosec // committed to Git, must be world-readable
		return err
	}
	for path, data := range outputs {
		if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // committed to Git, must be world-readable
			return err
		}
		fmt.Fprintln(os.Stderr, "wrote", path)
	}
	return nil
}

func splitList(csv string) []string {
	parts := strings.Split(csv, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// quoteList turns "a,b" into `"a", "b"` so wildcards stay valid YAML.
func quoteList(csv string) string {
	parts := splitList(csv)
	for i, p := range parts {
		parts[i] = fmt.Sprintf("%q", p)
	}
	return strings.Join(parts, ", ")
}
