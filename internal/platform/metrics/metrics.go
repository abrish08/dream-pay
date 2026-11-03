// Package metrics exposes Prometheus-format metrics without dependencies:
// counters, histograms and gauges read at scrape time (e.g. "payments stuck
// in processing" from the database). Safe for concurrent use.
//
//	metrics.Counter("paygate_payments_created_total", "Payments created", "environment", "method").Inc("live", "telebirr")
//	metrics.Gauge("paygate_outbox_pending", "Unpublished events", map[string]string{"module": "payment"}, fn)
package metrics

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type registry struct {
	mu         sync.Mutex
	counters   map[string]*CounterVec
	histograms map[string]*HistogramVec
	gauges     map[string]*gaugeFamily
}

var std = &registry{counters: map[string]*CounterVec{}, histograms: map[string]*HistogramVec{}, gauges: map[string]*gaugeFamily{}}

// ---- Counters ----

type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64
}

// Counter returns the counter family name (created on first use).
func Counter(name, help string, labels ...string) *CounterVec {
	std.mu.Lock()
	defer std.mu.Unlock()
	if c, ok := std.counters[name]; ok {
		return c
	}
	c := &CounterVec{name: name, help: help, labels: labels, values: map[string]float64{}}
	std.counters[name] = c
	return c
}

func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

func (c *CounterVec) Add(v float64, labelValues ...string) {
	k := key(labelValues)
	c.mu.Lock()
	c.values[k] += v
	c.mu.Unlock()
}

// ---- Histograms ----

// DefBuckets (seconds) fit HTTP and provider calls.
var DefBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type HistogramVec struct {
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*histo
}

type histo struct {
	counts []uint64
	sum    float64
	count  uint64
}

func Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	std.mu.Lock()
	defer std.mu.Unlock()
	if h, ok := std.histograms[name]; ok {
		return h
	}
	h := &HistogramVec{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*histo{}}
	std.histograms[name] = h
	return h
}

func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	k := key(labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[k]
	if s == nil {
		s = &histo{counts: make([]uint64, len(h.buckets))}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

// Since observes the seconds elapsed since t.
func (h *HistogramVec) Since(t time.Time, labelValues ...string) {
	h.Observe(time.Since(t).Seconds(), labelValues...)
}

// ---- Gauges read at scrape time ----

type gaugeFamily struct {
	name, help string
	fns        map[string]gaugeFn // by label set; registering again replaces
}

type gaugeFn struct {
	labels map[string]string
	fn     func(ctx context.Context) ([]Sample, error)
}

// Sample is one value of a gauge, with extra labels (e.g. status).
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Gauge registers fn as the source of a gauge for the given fixed labels.
// fn runs on every scrape (keep it cheap: an indexed COUNT).
func Gauge(name, help string, labels map[string]string, fn func(ctx context.Context) ([]Sample, error)) {
	std.mu.Lock()
	defer std.mu.Unlock()
	g := std.gauges[name]
	if g == nil {
		g = &gaugeFamily{name: name, help: help, fns: map[string]gaugeFn{}}
		std.gauges[name] = g
	}
	g.fns[fmtLabels(labels)] = gaugeFn{labels: labels, fn: fn}
}

// ---- Exposition ----

// Handler serves /metrics. With a token, scrapers must send it as
// "Authorization: Bearer <token>".
func Handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		Write(ctx, w)
	})
}

// Write renders every metric in the Prometheus text format.
func Write(ctx context.Context, w io.Writer) {
	std.mu.Lock()
	counters := sortedValues(std.counters)
	histograms := sortedValues(std.histograms)
	gauges := sortedValues(std.gauges)
	std.mu.Unlock()

	for _, c := range counters {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		c.mu.Lock()
		for _, k := range sortedKeys(c.values) {
			fmt.Fprintf(w, "%s%s %s\n", c.name, labelString(c.labels, k, nil), num(c.values[k]))
		}
		c.mu.Unlock()
	}
	for _, h := range histograms {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
		h.mu.Lock()
		for _, k := range sortedKeys(h.series) {
			s := h.series[k]
			for i, b := range h.buckets {
				fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labels, k, map[string]string{"le": num(b)}), s.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, labelString(h.labels, k, map[string]string{"le": "+Inf"}), s.count)
			fmt.Fprintf(w, "%s_sum%s %s\n%s_count%s %d\n", h.name, labelString(h.labels, k, nil), num(s.sum),
				h.name, labelString(h.labels, k, nil), s.count)
		}
		h.mu.Unlock()
	}
	for _, g := range gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		std.mu.Lock()
		fns := make([]gaugeFn, 0, len(g.fns))
		for _, k := range sortedKeys(g.fns) {
			fns = append(fns, g.fns[k])
		}
		std.mu.Unlock()
		for _, f := range fns {
			samples, err := f.fn(ctx)
			if err != nil {
				continue // a failing source is left out, not reported as 0
			}
			for _, s := range samples {
				all := map[string]string{}
				for k, v := range f.labels {
					all[k] = v
				}
				for k, v := range s.Labels {
					all[k] = v
				}
				fmt.Fprintf(w, "%s%s %s\n", g.name, fmtLabels(all), num(s.Value))
			}
		}
	}
}

// ---- helpers ----

func key(values []string) string { return strings.Join(values, "\x00") }

func labelString(names []string, k string, extra map[string]string) string {
	m := map[string]string{}
	if len(names) > 0 {
		vals := strings.Split(k, "\x00")
		for i, n := range names {
			if i < len(vals) {
				m[n] = vals[i]
			}
		}
	}
	for n, v := range extra {
		m[n] = v
	}
	return fmtLabels(m)
}

func fmtLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + `="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(m[n]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedValues[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, m[k])
	}
	return out
}
