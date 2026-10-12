// Package metrics is a small, dependency-free Prometheus text exporter.
package metrics

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
)

type metric interface {
	write(w io.Writer)
}

// Registry holds metrics in registration order.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

func (r *Registry) add(m metric) {
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
}

// Write renders every metric in the Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := slices.Clone(r.metrics)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// CounterVec is a counter partitioned by label values.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64
}

func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, values: map[string]float64{}}
	r.add(c)
	return c
}

// Add increments the series identified by values (one per label).
func (c *CounterVec) Add(delta float64, values ...string) {
	k := labelString(c.labels, values)
	c.mu.Lock()
	c.values[k] += delta
	c.mu.Unlock()
}

func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

func (c *CounterVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %s\n", c.name, k, formatFloat(c.values[k]))
	}
}

// GaugeFunc reports values computed at scrape time.
type GaugeFunc struct {
	name, help string
	labels     []string
	fn         func(emit func(v float64, labelValues ...string))
	counter    bool
}

func (r *Registry) NewGaugeFunc(name, help string, labels []string, fn func(emit func(v float64, labelValues ...string))) {
	r.add(&GaugeFunc{name: name, help: help, labels: labels, fn: fn})
}

// NewCounterFunc is like NewGaugeFunc for values that only grow, so
// Prometheus's rate() and increase() apply to them.
func (r *Registry) NewCounterFunc(name, help string, labels []string, fn func(emit func(v float64, labelValues ...string))) {
	r.add(&GaugeFunc{name: name, help: help, labels: labels, fn: fn, counter: true})
}

func (g *GaugeFunc) write(w io.Writer) {
	typ := "gauge"
	if g.counter {
		typ = "counter"
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", g.name, g.help, g.name, typ)
	g.fn(func(v float64, values ...string) {
		fmt.Fprintf(w, "%s%s %s\n", g.name, labelString(g.labels, values), formatFloat(v))
	})
}

// Histogram tracks a distribution in fixed cumulative buckets.
type Histogram struct {
	name, help string
	bounds     []float64
	mu         sync.Mutex
	counts     []uint64
	sum        float64
	count      uint64
}

// LatencyBuckets suit request latencies from a millisecond to several minutes.
var LatencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}

func (r *Registry) NewHistogram(name, help string, bounds []float64) *Histogram {
	h := &Histogram{name: name, help: help, bounds: bounds, counts: make([]uint64, len(bounds))}
	r.add(h)
	return h
}

func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
}

func (h *Histogram) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.bounds {
		fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", h.name, formatFloat(b), h.counts[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %s\n%s_count %d\n",
		h.name, h.count, h.name, formatFloat(h.sum), h.name, h.count)
}

func labelString(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			sb.WriteByte(',')
		}
		v := ""
		if i < len(values) {
			v = values[i]
		}
		fmt.Fprintf(&sb, "%s=%q", n, v)
	}
	sb.WriteByte('}')
	return sb.String()
}

func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strings.TrimSuffix(fmt.Sprintf("%g", v), ".0")
}
