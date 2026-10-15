package gateway

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the gateway's own Prometheus metrics. Labels only carry values
// bounded by configuration (route and token names), never tenant IDs taken
// from the request.
type Metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	denied   *prometheus.CounterVec

	tokens        prometheus.Gauge
	reloads       *prometheus.CounterVec
	lastReloadOK  prometheus.Gauge
	reloadHealthy prometheus.Gauge
}

// NewMetrics registers the gateway metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tenant_gateway_requests_total",
			Help: "Requests handled, by route, token name and HTTP status code.",
		}, []string{"route", "token", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tenant_gateway_request_duration_seconds",
			Help:    "Time to serve a request, including the upstream call.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"route"}),
		denied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tenant_gateway_denied_requests_total",
			Help: "Requests refused by the gateway, by reason.",
		}, []string{"reason"}),
		tokens: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tenant_gateway_tokens",
			Help: "Number of tokens in the active configuration.",
		}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tenant_gateway_config_reloads_total",
			Help: "Token file reload attempts, by result.",
		}, []string{"result"}),
		lastReloadOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tenant_gateway_config_last_reload_success_timestamp_seconds",
			Help: "Unix time of the last successful token file load.",
		}),
		reloadHealthy: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tenant_gateway_config_last_reload_successful",
			Help: "Whether the last token file reload succeeded (1) or was rejected (0).",
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.denied, m.tokens, m.reloads, m.lastReloadOK, m.reloadHealthy)
	return m
}

func (m *Metrics) observe(route, token string, code int, d time.Duration) {
	m.requests.WithLabelValues(route, token, strconv.Itoa(code)).Inc()
	m.duration.WithLabelValues(route).Observe(d.Seconds())
}

// ConfigLoaded records the outcome of a token file (re)load.
func (m *Metrics) ConfigLoaded(tokens int, err error) {
	if err != nil {
		m.reloads.WithLabelValues("failure").Inc()
		m.reloadHealthy.Set(0)
		return
	}
	m.reloads.WithLabelValues("success").Inc()
	m.reloadHealthy.Set(1)
	m.tokens.Set(float64(tokens))
	m.lastReloadOK.SetToCurrentTime()
}
