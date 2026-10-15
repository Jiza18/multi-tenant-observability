// Package tenants models tenants/tenants.yaml, the single source of truth for
// every tenant, and renders the configuration other components derive from it.
package tenants

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Jiza18/multi-tenant-observability/internal/auth"
)

// Limits are the per-tenant knobs exposed in tenants.yaml. Unset fields
// inherit from defaults, and if still unset, from Mimir's own defaults.
type Limits struct {
	Retention             string   `yaml:"retention,omitempty"`
	IngestionRate         *float64 `yaml:"ingestion_rate,omitempty"`
	IngestionBurstSize    *int     `yaml:"ingestion_burst_size,omitempty"`
	MaxGlobalSeries       *int     `yaml:"max_global_series,omitempty"`
	RulerMaxRulesPerGroup *int     `yaml:"ruler_max_rules_per_group,omitempty"`
}

// Tenant is one entry of tenants.yaml.
type Tenant struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	RuleSets []string `yaml:"rule_sets,omitempty"`
	Limits   `yaml:",inline"`
}

// File is the whole tenants.yaml document.
type File struct {
	Defaults struct {
		RuleSets []string `yaml:"rule_sets,omitempty"`
		Limits   `yaml:",inline"`
	} `yaml:"defaults"`
	Tenants []Tenant `yaml:"tenants"`
}

// Load reads and validates tenants.yaml.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, f.validate()
}

// Mimir accepts durations such as 30d, 1y or 12h; 0 disables retention.
var durationRE = regexp.MustCompile(`^(0|([0-9]+(ms|s|m|h|d|w|y))+)$`)

func (f *File) validate() error {
	var errs []error
	check := func(where string, l Limits) {
		if l.Retention != "" && !durationRE.MatchString(l.Retention) {
			errs = append(errs, fmt.Errorf("%s: invalid retention %q", where, l.Retention))
		}
	}
	check("defaults", f.Defaults.Limits)

	if len(f.Tenants) == 0 {
		errs = append(errs, errors.New("no tenants defined"))
	}
	seen := map[string]bool{}
	for i, t := range f.Tenants {
		where := fmt.Sprintf("tenants[%d] (%s)", i, t.ID)
		switch {
		case !auth.ValidTenantID(t.ID):
			errs = append(errs, fmt.Errorf("%s: invalid tenant ID", where))
		case seen[t.ID]:
			errs = append(errs, fmt.Errorf("%s: duplicate tenant ID", where))
		}
		seen[t.ID] = true
		if t.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		}
		check(where, t.Limits)
	}
	return errors.Join(errs...)
}

// Effective returns the tenant's limits after applying defaults.
func (f *File) Effective(t Tenant) Limits {
	d := f.Defaults.Limits
	l := t.Limits
	if l.Retention == "" {
		l.Retention = d.Retention
	}
	l.IngestionRate = cmpOr(l.IngestionRate, d.IngestionRate)
	l.IngestionBurstSize = cmpOr(l.IngestionBurstSize, d.IngestionBurstSize)
	l.MaxGlobalSeries = cmpOr(l.MaxGlobalSeries, d.MaxGlobalSeries)
	l.RulerMaxRulesPerGroup = cmpOr(l.RulerMaxRulesPerGroup, d.RulerMaxRulesPerGroup)
	return l
}

// RuleSets returns the rule sets evaluated for t: the defaults followed by
// the tenant's own, without duplicates.
func (f *File) RuleSets(t Tenant) []string {
	var out []string
	for _, s := range append(slices.Clone(f.Defaults.RuleSets), t.RuleSets...) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// RulePlanEntry maps a rule file to the Mimir ruler namespace it is loaded
// into for one tenant.
type RulePlanEntry struct {
	Tenant    string
	Namespace string
	File      string // relative to the rules directory
}

// TenantRulesNamespace holds rules that apply to a single tenant
// (rules/tenants/<id>.yaml).
const TenantRulesNamespace = "custom"

// RulePlan resolves which rule files each tenant gets. Every referenced rule
// set must exist as <rulesDir>/<set>.yaml.
func (f *File) RulePlan(rulesDir string) ([]RulePlanEntry, error) {
	var plan []RulePlanEntry
	var errs []error
	for _, t := range f.Tenants {
		for _, set := range f.RuleSets(t) {
			file := set + ".yaml"
			if _, err := os.Stat(filepath.Join(rulesDir, file)); err != nil {
				errs = append(errs, fmt.Errorf("tenant %s: rule set %q: %w", t.ID, set, err))
				continue
			}
			plan = append(plan, RulePlanEntry{Tenant: t.ID, Namespace: set, File: file})
		}
		custom := filepath.Join("tenants", t.ID+".yaml")
		if _, err := os.Stat(filepath.Join(rulesDir, custom)); err == nil {
			plan = append(plan, RulePlanEntry{Tenant: t.ID, Namespace: TenantRulesNamespace, File: custom})
		}
	}

	// A rules file for a tenant that no longer exists would silently never load.
	orphans, _ := filepath.Glob(filepath.Join(rulesDir, "tenants", "*.yaml"))
	for _, path := range orphans {
		id := strings.TrimSuffix(filepath.Base(path), ".yaml")
		if !slices.ContainsFunc(f.Tenants, func(t Tenant) bool { return t.ID == id }) {
			errs = append(errs, fmt.Errorf("%s: no tenant %q in tenants.yaml", path, id))
		}
	}
	return plan, errors.Join(errs...)
}

func cmpOr[T any](v, fallback *T) *T {
	if v != nil {
		return v
	}
	return fallback
}
