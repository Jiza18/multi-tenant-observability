package gateway

import (
	"net/http"
	"slices"
	"strings"

	"github.com/Jiza18/multi-tenant-observability/internal/auth"
)

// route is one entry of the allow-list. Anything that does not match a route
// is rejected, which keeps Mimir's operational endpoints (/ingester/*,
// /distributor/*, /compactor/*, /config, /memberlist, ...) off the internet.
type route struct {
	name    string
	methods []string
	path    string
	prefix  bool // match path as a prefix instead of exactly
	scope   auth.Scope

	// federated routes accept several tenants in one request ("a|b"), which
	// Mimir resolves with tenant federation. Only reads support it.
	federated bool
}

var routes = []route{
	// Ingestion.
	{name: "push", methods: post, path: "/api/v1/push", scope: auth.ScopeMetricsWrite},
	{name: "otlp", methods: post, path: "/otlp/v1/metrics", scope: auth.ScopeMetricsWrite},

	// Prometheus-compatible query API (Grafana, promtool, scripts).
	{name: "query", methods: getPost, path: "/prometheus/api/v1/", prefix: true, scope: auth.ScopeMetricsRead, federated: true},

	// Ruler configuration, as used by mimirtool.
	{name: "rules_read", methods: get, path: "/prometheus/config/v1/rules", prefix: true, scope: auth.ScopeRulesRead},
	{name: "rules_write", methods: postDelete, path: "/prometheus/config/v1/rules", prefix: true, scope: auth.ScopeRulesWrite},

	// Alertmanager: tenants' own Prometheus servers push alerts here.
	{name: "alerts_push", methods: post, path: "/alertmanager/api/v2/alerts", scope: auth.ScopeAlertsWrite},
	{name: "alerts_read", methods: get, path: "/alertmanager/api/v2/", prefix: true, scope: auth.ScopeAlertsRead},
	{name: "alertmanager_config", methods: getPostDelete, path: "/api/v1/alerts", scope: auth.ScopeAlertmanagerConfig},
}

var (
	get           = []string{http.MethodGet}
	post          = []string{http.MethodPost}
	getPost       = []string{http.MethodGet, http.MethodPost}
	postDelete    = []string{http.MethodPost, http.MethodDelete}
	getPostDelete = []string{http.MethodGet, http.MethodPost, http.MethodDelete}
)

func (r *route) matches(path string) bool {
	if r.prefix {
		return strings.HasPrefix(path, r.path)
	}
	return path == r.path
}

// matchRoute returns the route for a request. When it returns nil,
// pathAllowed tells apart an unknown path (404) from a known path called with
// the wrong method (405).
func matchRoute(method, path string) (r *route, pathAllowed bool) {
	for i := range routes {
		rt := &routes[i]
		if !rt.matches(path) {
			continue
		}
		pathAllowed = true
		if slices.Contains(rt.methods, method) {
			return rt, true
		}
	}
	return nil, pathAllowed
}
