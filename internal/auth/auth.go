// Package auth loads gateway tokens and resolves a presented credential into
// a principal: which tenants it may act on and with which scopes.
//
// Tokens are never stored in plain text. The token file holds the SHA-256 of
// each token; an incoming credential is hashed and looked up by its digest, so
// the comparison never touches the secret itself.
package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Scope is a permission granted to a token.
type Scope string

// Scopes, named <resource>:<action>. See internal/gateway/routes.go for the
// endpoints each one unlocks.
const (
	ScopeMetricsWrite       Scope = "metrics:write"
	ScopeMetricsRead        Scope = "metrics:read"
	ScopeRulesRead          Scope = "rules:read"
	ScopeRulesWrite         Scope = "rules:write"
	ScopeAlertsWrite        Scope = "alerts:write"
	ScopeAlertsRead         Scope = "alerts:read"
	ScopeAlertmanagerConfig Scope = "alertmanager:config"
)

var knownScopes = []Scope{
	ScopeMetricsWrite, ScopeMetricsRead,
	ScopeRulesRead, ScopeRulesWrite,
	ScopeAlertsWrite, ScopeAlertsRead,
	ScopeAlertmanagerConfig,
}

// Valid reports whether s is a scope the gateway knows.
func (s Scope) Valid() bool { return slices.Contains(knownScopes, s) }

// AnyTenant in a token's tenant list lets the caller choose any tenant
// through the X-Scope-OrgID header. Reserve it for operators and CI.
const AnyTenant = "*"

// Principal is the identity behind a valid token.
type Principal struct {
	Name    string   `yaml:"name"`
	SHA256  string   `yaml:"sha256"`
	Tenants []string `yaml:"tenants"`
	Scopes  []Scope  `yaml:"scopes"`
}

// HasScope reports whether the principal was granted s.
func (p *Principal) HasScope(s Scope) bool { return slices.Contains(p.Scopes, s) }

// CanAccess reports whether the principal may act on tenant.
func (p *Principal) CanAccess(tenant string) bool {
	return slices.Contains(p.Tenants, AnyTenant) || slices.Contains(p.Tenants, tenant)
}

// DefaultTenant returns the tenant to use when the caller did not ask for one.
// It only exists for tokens bound to exactly one concrete tenant.
func (p *Principal) DefaultTenant() (string, bool) {
	if len(p.Tenants) == 1 && p.Tenants[0] != AnyTenant {
		return p.Tenants[0], true
	}
	return "", false
}

// TokenSet is an immutable, validated set of principals indexed by digest.
type TokenSet struct {
	byDigest map[string]*Principal
}

// Lookup returns the principal for a plain-text token.
func (s *TokenSet) Lookup(token string) (*Principal, bool) {
	if token == "" {
		return nil, false
	}
	p, ok := s.byDigest[Digest(token)]
	return p, ok
}

// Len returns the number of configured tokens.
func (s *TokenSet) Len() int { return len(s.byDigest) }

// Digest returns the hex-encoded SHA-256 of a token, as stored in the token file.
func Digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type tokenFile struct {
	Tokens []Principal `yaml:"tokens"`
}

// LoadFile reads and validates a token file.
func LoadFile(path string) (*TokenSet, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	set, err := Parse(data)
	return set, data, err
}

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Parse validates a token file document. Any error rejects the whole file, so
// a bad edit never leaves the gateway with a partially applied configuration.
func Parse(data []byte) (*TokenSet, error) {
	var f tokenFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}

	set := &TokenSet{byDigest: make(map[string]*Principal, len(f.Tokens))}
	names := make(map[string]bool, len(f.Tokens))
	var errs []error

	for i := range f.Tokens {
		p := &f.Tokens[i]
		where := fmt.Sprintf("tokens[%d] (%s)", i, p.Name)

		switch {
		case p.Name == "":
			errs = append(errs, fmt.Errorf("tokens[%d]: name is required", i))
		case names[p.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate name", where))
		}
		names[p.Name] = true

		p.SHA256 = strings.ToLower(p.SHA256)
		if !digestRE.MatchString(p.SHA256) {
			errs = append(errs, fmt.Errorf("%s: sha256 must be 64 hex characters", where))
		} else if _, dup := set.byDigest[p.SHA256]; dup {
			errs = append(errs, fmt.Errorf("%s: the same token is configured twice", where))
		}

		if len(p.Tenants) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one tenant is required", where))
		}
		for _, t := range p.Tenants {
			if t != AnyTenant && !ValidTenantID(t) {
				errs = append(errs, fmt.Errorf("%s: invalid tenant %q", where, t))
			}
		}

		if len(p.Scopes) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one scope is required", where))
		}
		for _, s := range p.Scopes {
			if !s.Valid() {
				errs = append(errs, fmt.Errorf("%s: unknown scope %q", where, s))
			}
		}

		set.byDigest[p.SHA256] = p
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return set, nil
}

// ValidTenantID applies Mimir's tenant ID rules: 1-150 characters from
// [a-zA-Z0-9!-_.*'()], excluding "." and "..".
func ValidTenantID(id string) bool {
	if id == "" || len(id) > 150 || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!-_.*'()", r):
		default:
			return false
		}
	}
	return true
}
