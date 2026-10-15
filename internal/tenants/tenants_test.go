package tenants

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const doc = `
defaults:
  retention: 30d
  max_global_series: 150000
  rule_sets: [watchdog]
tenants:
  - id: acme
    name: ACME
    rule_sets: [hosts, watchdog]
    max_global_series: 500000
  - id: globex
    name: Globex
    retention: 7d
`

func load(t *testing.T, content string) *File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tenants.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDefaultsAreInherited(t *testing.T) {
	f := load(t, doc)
	acme, globex := f.Effective(f.Tenants[0]), f.Effective(f.Tenants[1])

	if acme.Retention != "30d" || *acme.MaxGlobalSeries != 500000 {
		t.Errorf("acme = %+v, want retention 30d (default) and 500000 series (own)", acme)
	}
	if globex.Retention != "7d" || *globex.MaxGlobalSeries != 150000 {
		t.Errorf("globex = %+v, want retention 7d (own) and 150000 series (default)", globex)
	}
	if got := f.RuleSets(f.Tenants[0]); strings.Join(got, ",") != "watchdog,hosts" {
		t.Errorf("rule sets = %v, want defaults first and no duplicates", got)
	}
}

func TestLoadRejectsInvalidTenants(t *testing.T) {
	for name, content := range map[string]string{
		"duplicate id":      "tenants: [{id: a, name: A}, {id: a, name: B}]",
		"invalid id":        "tenants: [{id: a/b, name: A}]",
		"missing name":      "tenants: [{id: a}]",
		"bad retention":     "tenants: [{id: a, name: A, retention: 30 days}]",
		"unknown field":     "tenants: [{id: a, name: A, retention_days: 30}]",
		"no tenants at all": "defaults: {retention: 1d}",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tenants.yaml")
			_ = os.WriteFile(path, []byte(content), 0o644)
			if _, err := Load(path); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestRulePlan(t *testing.T) {
	f := load(t, doc)
	rules := t.TempDir()
	for _, p := range []string{"watchdog.yaml", "hosts.yaml", "tenants/acme.yaml"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(rules, p)), 0o755)
		_ = os.WriteFile(filepath.Join(rules, p), []byte("groups: []\n"), 0o644)
	}

	plan, err := f.RulePlan(rules)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range plan {
		got = append(got, e.Tenant+"/"+e.Namespace+"="+e.File)
	}
	want := "acme/watchdog=watchdog.yaml acme/hosts=hosts.yaml acme/custom=tenants/acme.yaml globex/watchdog=watchdog.yaml"
	if strings.Join(got, " ") != want {
		t.Errorf("plan = %v\nwant   %v", got, want)
	}

	_ = os.WriteFile(filepath.Join(rules, "tenants", "initech.yaml"), []byte("groups: []\n"), 0o644)
	if _, err := f.RulePlan(rules); err == nil || !strings.Contains(err.Error(), "initech") {
		t.Errorf("orphan tenant rules file: err = %v", err)
	}
	_ = os.Remove(filepath.Join(rules, "hosts.yaml"))
	if _, err := f.RulePlan(rules); err == nil || !strings.Contains(err.Error(), `"hosts"`) {
		t.Errorf("missing rule set: err = %v", err)
	}
}

func TestGrafanaDatasources(t *testing.T) {
	f := load(t, doc)
	out, err := f.GrafanaDatasources("http://mimir:8080/", "acme")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Datasources []datasource `yaml:"datasources"`
	}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}

	byUID := map[string]datasource{}
	for _, d := range parsed.Datasources {
		byUID[d.UID] = d
	}
	if len(byUID) != 5 {
		t.Fatalf("got %d datasources, want 2 per tenant + 1 federated", len(byUID))
	}
	acme := byUID["mimir-acme"]
	if acme.URL != "http://mimir:8080/prometheus" || acme.SecureJSONData["httpHeaderValue1"] != "acme" || !acme.IsDefault {
		t.Errorf("acme datasource = %+v", acme)
	}
	if got := byUID[AllTenantsUID].SecureJSONData["httpHeaderValue1"]; got != "acme|globex" {
		t.Errorf("federated datasource tenants = %q", got)
	}
}

func TestMimirRuntimeConfig(t *testing.T) {
	out, err := load(t, doc).MimirRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Overrides map[string]map[string]any `yaml:"overrides"`
	}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Overrides["globex"]["compactor_blocks_retention_period"]; got != "7d" {
		t.Errorf("globex retention = %v", got)
	}
	if got := parsed.Overrides["acme"]["max_global_series_per_user"]; got != 500000 {
		t.Errorf("acme series limit = %v", got)
	}
}
