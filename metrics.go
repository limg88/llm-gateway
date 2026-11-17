package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Metrics is a small Prometheus text-format registry: counters, gauges read
// at scrape time, and histograms with fixed buckets.
type Metrics struct {
	mu       sync.Mutex
	help     map[string]string
	counters map[string]map[string]float64 // name -> labels -> value
	hists    map[string]map[string]*histogram
	gauges   map[string]func() float64
}

type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

var durationBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}

func NewMetrics() *Metrics {
	return &Metrics{
		help:     map[string]string{},
		counters: map[string]map[string]float64{},
		hists:    map[string]map[string]*histogram{},
		gauges:   map[string]func() float64{},
	}
}

// labelString renders key/value pairs as a Prometheus label set.
func labelString(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		parts = append(parts, fmt.Sprintf(`%s="%s"`, kv[i], v))
	}
	return strings.Join(parts, ",")
}

func (m *Metrics) Describe(name, help string) {
	m.mu.Lock()
	m.help[name] = help
	m.mu.Unlock()
}

// Add increases a counter. Labels are key/value pairs.
func (m *Metrics) Add(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters[name] == nil {
		m.counters[name] = map[string]float64{}
	}
	m.counters[name][labelString(labels)] += v
}

func (m *Metrics) Inc(name string, labels ...string) { m.Add(name, 1, labels...) }

func (m *Metrics) Observe(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hists[name] == nil {
		m.hists[name] = map[string]*histogram{}
	}
	ls := labelString(labels)
	h := m.hists[name][ls]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(durationBuckets))}
		m.hists[name][ls] = h
	}
	for i, b := range durationBuckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
}

// Gauge registers a value read at every scrape.
func (m *Metrics) Gauge(name, help string, fn func() float64) {
	m.mu.Lock()
	m.gauges[name] = fn
	m.help[name] = help
	m.mu.Unlock()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func series(name, labels string) string {
	if labels == "" {
		return name
	}
	return name + "{" + labels + "}"
}

func (m *Metrics) Render(w io.Writer) {
	m.mu.Lock()
	gauges := make(map[string]func() float64, len(m.gauges))
	for k, v := range m.gauges {
		gauges[k] = v
	}
	m.mu.Unlock()
	// Gauges may take other locks: read them outside ours.
	values := map[string]float64{}
	for name, fn := range gauges {
		values[name] = fn()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	header := func(name, typ string) {
		if h := m.help[name]; h != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", name, h)
		}
		fmt.Fprintf(w, "# TYPE %s %s\n", name, typ)
	}
	for _, name := range sortedKeys(values) {
		header(name, "gauge")
		fmt.Fprintf(w, "%s %g\n", name, values[name])
	}
	for _, name := range sortedKeys(m.counters) {
		header(name, "counter")
		for _, ls := range sortedKeys(m.counters[name]) {
			fmt.Fprintf(w, "%s %g\n", series(name, ls), m.counters[name][ls])
		}
	}
	for _, name := range sortedKeys(m.hists) {
		header(name, "histogram")
		for _, ls := range sortedKeys(m.hists[name]) {
			h := m.hists[name][ls]
			sep := ""
			if ls != "" {
				sep = ","
			}
			for i, b := range durationBuckets {
				fmt.Fprintf(w, "%s_bucket{%s%sle=\"%g\"} %d\n", name, ls, sep, b, h.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", name, ls, sep, h.count)
			fmt.Fprintf(w, "%s %g\n", series(name+"_sum", ls), h.sum)
			fmt.Fprintf(w, "%s %d\n", series(name+"_count", ls), h.count)
		}
	}
}
