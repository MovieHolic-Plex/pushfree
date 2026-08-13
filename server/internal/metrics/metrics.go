// Package metrics defines pushfree's Prometheus collectors and the request
// logging middleware (plan todo 15). The collectors are owned by this
// package; other server workers record observations through the typed methods
// (IncDeliveryAttempts, IncAck, ...) without knowing the underlying metric
// names or label cardinality.
//
// All pushfree_* names and label sets are fixed by the todo-15 SPEC:
//
//	pushfree_sends_total
//	pushfree_messages_received_total
//	pushfree_delivery_attempts_total{channel}
//	pushfree_delivery_failures_total{channel,reason_class}
//	pushfree_ws_clients
//	pushfree_ack_total{status}
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric names (SPEC todo 15). Exported so tests and tooling can reference
// the exact strings.
const (
	NameSends            = "pushfree_sends_total"
	NameMessagesReceived = "pushfree_messages_received_total"
	NameDeliveryAttempts = "pushfree_delivery_attempts_total"
	NameDeliveryFailures = "pushfree_delivery_failures_total"
	NameWSClients        = "pushfree_ws_clients"
	NameAck              = "pushfree_ack_total"
)

// Metrics holds the pushfree Prometheus collectors. The zero value is NOT
// usable; build one with New.
type Metrics struct {
	sends            prometheus.Counter
	messagesReceived prometheus.Counter
	deliveryAttempts *prometheus.CounterVec
	deliveryFailures *prometheus.CounterVec
	wsClients        prometheus.Gauge
	ack              *prometheus.CounterVec
}

// New registers all pushfree_* collectors on reg and returns a Metrics value
// whose methods record observations against them. reg is typically the
// registry built by NewBundle (which also seeds the go_* and process_
// collectors) so the live server and tests share one wiring and never
// pollute prometheus.DefaultRegisterer.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		sends: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameSends,
			Help: "Total accepted message sends (POST /1/messages.json with a 2xx response).",
		}),
		messagesReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: NameMessagesReceived,
			Help: "Total messages received (one per accepted send).",
		}),
		deliveryAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameDeliveryAttempts,
			Help: "Total delivery attempts per channel (ws|sse|fcm|up).",
		}, []string{"channel"}),
		deliveryFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameDeliveryFailures,
			Help: "Total delivery failures per channel and reason class.",
		}, []string{"channel", "reason_class"}),
		wsClients: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: NameWSClients,
			Help: "Current number of connected WebSocket clients.",
		}),
		ack: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: NameAck,
			Help: "Total receipt acknowledgements by status.",
		}, []string{"status"}),
	}
	reg.MustRegister(
		m.sends,
		m.messagesReceived,
		m.deliveryAttempts,
		m.deliveryFailures,
		m.wsClients,
		m.ack,
	)
	return m
}

// IncSends records one accepted send.
func (m *Metrics) IncSends() { m.sends.Inc() }

// IncMessagesReceived records one received message. Each accepted send is
// exactly one received message, so the request-logging middleware calls this
// alongside IncSends on a 2xx POST /1/messages.json.
func (m *Metrics) IncMessagesReceived() { m.messagesReceived.Inc() }

// IncDeliveryAttempts records a delivery attempt for a channel
// (ws|sse|fcm|up). Called by the transport layers when they try to deliver a
// fanned-out message to a recipient.
func (m *Metrics) IncDeliveryAttempts(channel string) {
	m.deliveryAttempts.WithLabelValues(channel).Inc()
}

// IncDeliveryFailures records a delivery failure for a channel and a reason
// class (e.g. "timeout", "auth", "unregistered", "quota").
func (m *Metrics) IncDeliveryFailures(channel, reasonClass string) {
	m.deliveryFailures.WithLabelValues(channel, reasonClass).Inc()
}

// IncWSClients increments the live WebSocket client gauge.
func (m *Metrics) IncWSClients() { m.wsClients.Inc() }

// DecWSClients decrements the live WebSocket client gauge.
func (m *Metrics) DecWSClients() { m.wsClients.Dec() }

// SetWSClients sets the live WebSocket client gauge to n.
func (m *Metrics) SetWSClients(n int) { m.wsClients.Set(float64(n)) }

// IncAck records a receipt acknowledgement by status (e.g. "ok",
// "rejected", "not_found").
func (m *Metrics) IncAck(status string) { m.ack.WithLabelValues(status).Inc() }

// Bundle is the complete metrics surface: the typed collectors plus the
// /metrics HTTP handler and the underlying gatherer (for tests).
type Bundle struct {
	*Metrics
	Handler  http.Handler
	Registry prometheus.Gatherer
}

// NewBundle builds a self-contained registry preloaded with the Go runtime
// and process collectors (go_* and process_*) plus the pushfree_* collectors,
// and returns the typed Metrics alongside the /metrics HTTP handler. It is
// the single wiring point used by both the live server and tests so there is
// exactly one source of truth for what /metrics exposes.
func NewBundle() *Bundle {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return &Bundle{
		Metrics:  New(reg),
		Handler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
		Registry: reg,
	}
}
