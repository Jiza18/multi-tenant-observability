// Package gateway implements the authenticating reverse proxy that sits in
// front of Mimir. For every request it:
//
//  1. matches the method and path against an allow-list of routes,
//  2. resolves the bearer (or basic auth) token into a principal,
//  3. checks that the principal holds the scope the route requires,
//  4. decides the tenant (X-Scope-OrgID) the request runs as, and
//  5. forwards it to Mimir without the client's credentials.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Jiza18/multi-tenant-observability/internal/auth"
)

// OrgIDHeader is the header Mimir reads the tenant from.
const OrgIDHeader = "X-Scope-OrgID"

// StatusClientClosedRequest is recorded when the client cancels a request
// before Mimir answers (nginx's convention).
const StatusClientClosedRequest = 499

// TokenSource returns the token set currently in force.
type TokenSource interface {
	Tokens() *auth.TokenSet
}

// Config wires a Gateway.
type Config struct {
	Upstream  *url.URL
	Tokens    TokenSource
	Logger    *slog.Logger
	Metrics   *Metrics
	Transport http.RoundTripper // optional; defaults to a tuned http.Transport
}

// Gateway is an http.Handler.
type Gateway struct {
	tokens  TokenSource
	logger  *slog.Logger
	metrics *Metrics
	proxy   *httputil.ReverseProxy
}

type tenantKey struct{}

// New builds a Gateway from cfg.
func New(cfg Config) *Gateway {
	transport := cfg.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConnsPerHost = 100
		t.IdleConnTimeout = 30 * time.Second
		transport = t
	}

	g := &Gateway{tokens: cfg.Tokens, logger: cfg.Logger, metrics: cfg.Metrics}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.Upstream)
			pr.SetXForwarded()
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set(OrgIDHeader, pr.In.Context().Value(tenantKey{}).(string))
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The client went away: not an upstream failure, keep it out of
			// the 5xx error rate.
			if errors.Is(err, context.Canceled) {
				g.logger.Debug("client canceled request", "path", r.URL.Path)
				w.WriteHeader(StatusClientClosedRequest)
				return
			}
			g.logger.Error("upstream request failed", "path", r.URL.Path, "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
	}
	return g
}

// denial is a request the gateway refuses to forward.
type denial struct {
	status int
	reason string // low-cardinality label for metrics
	msg    string // returned to the client
}

func deny(status int, reason, format string, args ...any) *denial {
	return &denial{status: status, reason: reason, msg: fmt.Sprintf(format, args...)}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	routeName, tokenName := "none", "anonymous"
	defer func() {
		g.metrics.observe(routeName, tokenName, rec.status, time.Since(start))
	}()

	rt, p, tenant, d := g.authorize(r)
	if rt != nil {
		routeName = rt.name
	}
	if p != nil {
		tokenName = p.Name
	}
	if d != nil {
		g.metrics.denied.WithLabelValues(d.reason).Inc()
		g.logger.Info("request denied",
			"reason", d.reason, "method", r.Method, "path", r.URL.Path,
			"token", tokenName, "remote", r.RemoteAddr)
		if d.status == http.StatusUnauthorized {
			rec.Header().Set("WWW-Authenticate", `Bearer realm="mimir"`)
		}
		http.Error(rec, d.msg, d.status)
		return
	}

	ctx := context.WithValue(r.Context(), tenantKey{}, tenant)
	g.proxy.ServeHTTP(rec, r.WithContext(ctx))
}

// authorize returns the matched route, the principal and the tenant for r, or
// the reason it must be refused.
func (g *Gateway) authorize(r *http.Request) (*route, *auth.Principal, string, *denial) {
	// Reject "..", "//" and similar so the allow-list cannot be sidestepped.
	if path.Clean(r.URL.Path) != r.URL.Path {
		return nil, nil, "", deny(http.StatusBadRequest, "bad_path", "non-canonical path")
	}

	rt, pathAllowed := matchRoute(r.Method, r.URL.Path)
	if rt == nil {
		if pathAllowed {
			return nil, nil, "", deny(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
		return nil, nil, "", deny(http.StatusNotFound, "route_not_allowed", "not found")
	}

	token, ok := credential(r)
	if !ok {
		return rt, nil, "", deny(http.StatusUnauthorized, "missing_credentials", "missing bearer token")
	}
	p, ok := g.tokens.Tokens().Lookup(token)
	if !ok {
		return rt, nil, "", deny(http.StatusUnauthorized, "invalid_token", "invalid token")
	}
	if !p.HasScope(rt.scope) {
		return rt, p, "", deny(http.StatusForbidden, "missing_scope", "token lacks scope %s", rt.scope)
	}

	tenant, d := resolveTenant(p, rt, r.Header.Get(OrgIDHeader))
	return rt, p, tenant, d
}

// credential extracts the token from "Authorization: Bearer <token>" or, for
// clients that only speak basic auth, from the basic auth password.
func credential(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if scheme, token, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
		token = strings.TrimSpace(token)
		return token, token != ""
	}
	if _, password, ok := r.BasicAuth(); ok && password != "" {
		return password, true
	}
	return "", false
}

// resolveTenant decides which tenant(s) the request runs as.
//
// Tokens bound to a single tenant may omit X-Scope-OrgID. Otherwise the header
// is required and every tenant it names must be allowed for the token. Several
// tenants ("a|b") are only accepted on federated (read) routes.
func resolveTenant(p *auth.Principal, rt *route, requested string) (string, *denial) {
	if requested == "" {
		if t, ok := p.DefaultTenant(); ok {
			return t, nil
		}
		return "", deny(http.StatusBadRequest, "missing_tenant",
			"%s header is required: this token can access more than one tenant", OrgIDHeader)
	}

	tenants := strings.Split(requested, "|")
	if len(tenants) > 1 && !rt.federated {
		return "", deny(http.StatusBadRequest, "federation_not_allowed",
			"multi-tenant requests are only supported on query endpoints")
	}
	for _, t := range tenants {
		if !auth.ValidTenantID(t) {
			return "", deny(http.StatusBadRequest, "invalid_tenant", "invalid tenant ID %q", t)
		}
		if !p.CanAccess(t) {
			return "", deny(http.StatusForbidden, "tenant_not_allowed", "token is not allowed to access tenant %q", t)
		}
	}
	return requested, nil
}

// statusRecorder captures the response code for metrics. Unwrap lets
// http.ResponseController reach the underlying writer, so streaming
// responses are still flushed.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
