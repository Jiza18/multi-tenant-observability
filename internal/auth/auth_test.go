package auth

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRejectsInvalidFiles(t *testing.T) {
	digest := Digest("secret")
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{"missing name", `tokens: [{sha256: ` + digest + `, tenants: [a], scopes: [metrics:read]}]`, "name is required"},
		{"bad digest", `tokens: [{name: x, sha256: abc, tenants: [a], scopes: [metrics:read]}]`, "64 hex"},
		{"no tenants", `tokens: [{name: x, sha256: ` + digest + `, scopes: [metrics:read]}]`, "at least one tenant"},
		{"invalid tenant", `tokens: [{name: x, sha256: ` + digest + `, tenants: ["a b"], scopes: [metrics:read]}]`, "invalid tenant"},
		{"unknown scope", `tokens: [{name: x, sha256: ` + digest + `, tenants: [a], scopes: [admin]}]`, "unknown scope"},
		{"unknown field", `tokens: [{name: x, token: plain, sha256: ` + digest + `, tenants: [a], scopes: [metrics:read]}]`, "field token not found"},
		{"duplicate name", `tokens:
  - {name: x, sha256: ` + digest + `, tenants: [a], scopes: [metrics:read]}
  - {name: x, sha256: ` + Digest("other") + `, tenants: [a], scopes: [metrics:read]}`, "duplicate name"},
		{"duplicate token", `tokens:
  - {name: x, sha256: ` + digest + `, tenants: [a], scopes: [metrics:read]}
  - {name: y, sha256: ` + digest + `, tenants: [a], scopes: [metrics:read]}`, "configured twice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLookupAcceptsUppercaseDigests(t *testing.T) {
	set, err := Parse([]byte(`tokens: [{name: x, sha256: ` + strings.ToUpper(Digest("s3cret")) + `, tenants: [a], scopes: [metrics:read]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := set.Lookup("s3cret"); !ok || p.Name != "x" {
		t.Fatalf("Lookup = %v, %v", p, ok)
	}
	if _, ok := set.Lookup(""); ok {
		t.Fatal("empty token must never match")
	}
}

func TestValidTenantID(t *testing.T) {
	for id, want := range map[string]bool{
		"acme-prod":              true,
		"team_a.eu(1)":           true,
		"":                       false,
		".":                      false,
		"..":                     false,
		"a/b":                    false,
		"a|b":                    false,
		strings.Repeat("x", 150): true,
		strings.Repeat("x", 151): false,
	} {
		if got := ValidTenantID(id); got != want {
			t.Errorf("ValidTenantID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.yaml")
	write := func(doc string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tokenDoc := func(name, token string) string {
		return `tokens: [{name: ` + name + `, sha256: ` + Digest(token) + `, tenants: [a], scopes: [metrics:read]}]`
	}

	write(tokenDoc("first", "one"))
	store, err := NewStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var reloads []error
	store.OnReload = func(_ int, err error) { reloads = append(reloads, err) }

	if changed, err := store.Reload(); changed || err != nil {
		t.Fatalf("unchanged file: changed=%v err=%v", changed, err)
	}

	write(tokenDoc("second", "two"))
	if changed, err := store.Reload(); !changed || err != nil {
		t.Fatalf("new file: changed=%v err=%v", changed, err)
	}
	if _, ok := store.Tokens().Lookup("two"); !ok {
		t.Fatal("new token not active after reload")
	}

	write("tokens: [{name: broken}]")
	if _, err := store.Reload(); err == nil {
		t.Fatal("invalid file was accepted")
	}
	if _, ok := store.Tokens().Lookup("two"); !ok {
		t.Fatal("previous tokens must stay active after a rejected reload")
	}
	_, _ = store.Reload() // same broken content: must not be reported again

	if len(reloads) != 2 || reloads[0] != nil || reloads[1] == nil {
		t.Fatalf("OnReload calls = %v, want [nil, error]", reloads)
	}
}
