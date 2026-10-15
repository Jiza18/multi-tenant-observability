package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Jiza18/multi-tenant-observability/internal/auth"
)

const (
	agentToken   = "acme-agent-token"
	readerToken  = "acme-reader-token"
	consoleToken = "console-token"
	opsToken     = "ops-token"
)

var tokenFile = `
tokens:
  - name: acme-agent
    sha256: ` + auth.Digest(agentToken) + `
    tenants: [acme]
    scopes: [metrics:write, alerts:write]
  - name: acme-reader
    sha256: ` + auth.Digest(readerToken) + `
    tenants: [acme]
    scopes: [metrics:read]
  - name: console
    sha256: ` + auth.Digest(consoleToken) + `
    tenants: [acme, globex]
    scopes: [metrics:read]
  - name: ops
    sha256: ` + auth.Digest(opsToken) + `
    tenants: ["*"]
    scopes: [metrics:read, metrics:write, rules:read, rules:write, alerts:read, alertmanager:config]
`

type staticTokens struct{ set *auth.TokenSet }

func (s staticTokens) Tokens() *auth.TokenSet { return s.set }

// upstreamRecord is what the fake Mimir saw.
type upstreamRecord struct {
	Path          string `json:"path"`
	OrgID         string `json:"org_id"`
	Authorization string `json:"authorization"`
	ForwardedFor  string `json:"forwarded_for"`
}

func newTestGateway(t *testing.T) (*Gateway, *Metrics) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(upstreamRecord{
			Path:          r.URL.Path,
			OrgID:         r.Header.Get(OrgIDHeader),
			Authorization: r.Header.Get("Authorization"),
			ForwardedFor:  r.Header.Get("X-Forwarded-For"),
		})
	}))
	t.Cleanup(upstream.Close)

	set, err := auth.Parse([]byte(tokenFile))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	m := NewMetrics(prometheus.NewRegistry())
	g := New(Config{
		Upstream: u,
		Tokens:   staticTokens{set},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:  m,
	})
	return g, m
}

func TestGateway(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		token     string
		basicAuth bool
		orgID     string

		wantStatus int
		wantOrgID  string // tenant forwarded upstream, for 200 responses
		wantBody   string // substring of the error message, for denials
	}{
		// Routing.
		{name: "operational endpoint is not exposed", method: "POST", path: "/ingester/shutdown", token: opsToken, wantStatus: 404},
		{name: "config endpoint is not exposed", method: "GET", path: "/config", token: opsToken, wantStatus: 404},
		{name: "path traversal is rejected", method: "GET", path: "/prometheus/api/v1/../../../ingester/shutdown", token: opsToken, wantStatus: 400},
		{name: "known path, wrong method", method: "GET", path: "/api/v1/push", token: agentToken, wantStatus: 405},

		// Authentication.
		{name: "missing token", method: "POST", path: "/api/v1/push", wantStatus: 401, wantBody: "missing bearer token"},
		{name: "unknown token", method: "POST", path: "/api/v1/push", token: "nope", wantStatus: 401, wantBody: "invalid token"},
		{name: "basic auth password is accepted as token", method: "POST", path: "/api/v1/push", token: agentToken, basicAuth: true, wantStatus: 200, wantOrgID: "acme"},

		// Authorisation by scope.
		{name: "write token can push", method: "POST", path: "/api/v1/push", token: agentToken, wantStatus: 200, wantOrgID: "acme"},
		{name: "write token can push OTLP", method: "POST", path: "/otlp/v1/metrics", token: agentToken, wantStatus: 200, wantOrgID: "acme"},
		{name: "write token can push alerts", method: "POST", path: "/alertmanager/api/v2/alerts", token: agentToken, wantStatus: 200, wantOrgID: "acme"},
		{name: "write token cannot query", method: "GET", path: "/prometheus/api/v1/query", token: agentToken, wantStatus: 403, wantBody: "metrics:read"},
		{name: "read token cannot push", method: "POST", path: "/api/v1/push", token: readerToken, wantStatus: 403, wantBody: "metrics:write"},
		{name: "read token cannot change rules", method: "POST", path: "/prometheus/config/v1/rules/hosts", token: readerToken, wantStatus: 403},
		{name: "rules can be listed", method: "GET", path: "/prometheus/config/v1/rules", token: opsToken, orgID: "acme", wantStatus: 200, wantOrgID: "acme"},
		{name: "alerts can be listed", method: "GET", path: "/alertmanager/api/v2/alerts", token: opsToken, orgID: "acme", wantStatus: 200, wantOrgID: "acme"},
		{name: "alertmanager config can be uploaded", method: "POST", path: "/api/v1/alerts", token: opsToken, orgID: "acme", wantStatus: 200, wantOrgID: "acme"},
		{name: "read token cannot see alertmanager config", method: "GET", path: "/api/v1/alerts", token: readerToken, wantStatus: 403, wantBody: "alertmanager:config"},

		// Tenant resolution.
		{name: "single-tenant token may send its own tenant", method: "POST", path: "/api/v1/push", token: agentToken, orgID: "acme", wantStatus: 200, wantOrgID: "acme"},
		{name: "single-tenant token cannot pick another tenant", method: "POST", path: "/api/v1/push", token: agentToken, orgID: "globex", wantStatus: 403, wantBody: `tenant "globex"`},
		{name: "multi-tenant token must name a tenant", method: "GET", path: "/prometheus/api/v1/query", token: consoleToken, wantStatus: 400, wantBody: "X-Scope-OrgID"},
		{name: "multi-tenant token can federate its tenants", method: "GET", path: "/prometheus/api/v1/query", token: consoleToken, orgID: "acme|globex", wantStatus: 200, wantOrgID: "acme|globex"},
		{name: "federation cannot include foreign tenants", method: "GET", path: "/prometheus/api/v1/query", token: consoleToken, orgID: "acme|initech", wantStatus: 403},
		{name: "wildcard token can use any tenant", method: "POST", path: "/prometheus/config/v1/rules/hosts", token: opsToken, orgID: "initech", wantStatus: 200, wantOrgID: "initech"},
		{name: "federation is read-only", method: "POST", path: "/api/v1/push", token: opsToken, orgID: "acme|globex", wantStatus: 400, wantBody: "query endpoints"},
		{name: "invalid tenant ID", method: "POST", path: "/api/v1/push", token: opsToken, orgID: "acme/../x", wantStatus: 400, wantBody: "invalid tenant"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newTestGateway(t)
			req := httptest.NewRequest(tc.method, "http://gateway"+tc.path, strings.NewReader("payload"))
			if tc.token != "" {
				if tc.basicAuth {
					req.SetBasicAuth("acme", tc.token)
				} else {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
			}
			if tc.orgID != "" {
				req.Header.Set(OrgIDHeader, tc.orgID)
			}
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				if !strings.Contains(rec.Body.String(), tc.wantBody) {
					t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
				}
				return
			}

			var got upstreamRecord
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode upstream record: %v", err)
			}
			if got.OrgID != tc.wantOrgID {
				t.Errorf("upstream %s = %q, want %q", OrgIDHeader, got.OrgID, tc.wantOrgID)
			}
			if got.Authorization != "" {
				t.Errorf("client credentials leaked upstream: %q", got.Authorization)
			}
			if got.Path != tc.path {
				t.Errorf("upstream path = %q, want %q", got.Path, tc.path)
			}
			if got.ForwardedFor == "" {
				t.Error("X-Forwarded-For was not set")
			}
		})
	}
}

func TestMetricsUseConfiguredNamesOnly(t *testing.T) {
	g, m := newTestGateway(t)

	for _, token := range []string{agentToken, "made-up-token"} {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/api/v1/push", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		g.ServeHTTP(httptest.NewRecorder(), req)
	}

	if got := testutil.ToFloat64(m.requests.WithLabelValues("push", "acme-agent", "200")); got != 1 {
		t.Errorf("accepted requests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("push", "anonymous", "401")); got != 1 {
		t.Errorf("rejected requests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.denied.WithLabelValues("invalid_token")); got != 1 {
		t.Errorf("denied{invalid_token} = %v, want 1", got)
	}
}

func TestUpstreamFailureReturns502(t *testing.T) {
	set, _ := auth.Parse([]byte(tokenFile))
	u, _ := url.Parse("http://127.0.0.1:1") // nothing listens here
	g := New(Config{
		Upstream: u,
		Tokens:   staticTokens{set},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:  NewMetrics(prometheus.NewRegistry()),
	})

	req := httptest.NewRequest(http.MethodPost, "http://gateway/api/v1/push", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestClientCancellationIsNotAnUpstreamError(t *testing.T) {
	g, m := newTestGateway(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is gone before Mimir answers

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://gateway/api/v1/push", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != StatusClientClosedRequest {
		t.Fatalf("status = %d, want %d", rec.Code, StatusClientClosedRequest)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("push", "acme-agent", "502")); got != 0 {
		t.Errorf("cancellation counted as 502: %v", got)
	}
}
