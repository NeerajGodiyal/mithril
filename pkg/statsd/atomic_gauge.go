package statsd

import (
	"errors"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// atomicGaugeFamily keeps one-hot gauge families coherent within a scrape.
type atomicGaugeFamily struct {
	mu     sync.RWMutex
	desc   *prometheus.Desc
	values map[string]float64
}

func newAtomicGaugeFamily(name, help, labelName string) *atomicGaugeFamily {
	return &atomicGaugeFamily{
		desc:   prometheus.NewDesc(name, help, []string{labelName}, nil),
		values: make(map[string]float64),
	}
}

func (g *atomicGaugeFamily) Describe(out chan<- *prometheus.Desc) { out <- g.desc }

func (g *atomicGaugeFamily) Collect(out chan<- prometheus.Metric) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	labels := make([]string, 0, len(g.values))
	for label := range g.values {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		out <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, g.values[label], label)
	}
}

func (g *atomicGaugeFamily) replace(values map[string]float64) {
	next := make(map[string]float64, len(values))
	for label, value := range values {
		next[label] = value
	}
	g.mu.Lock()
	g.values = next
	g.mu.Unlock()
}

// ReplaceGaugeFamily atomically replaces one fixed one-label gauge family.
func ReplaceGaugeFamily(metric Metric, values map[string]float64) error {
	family := metricsCollection.atomicGauges[metric]
	if family == nil {
		return errors.New("metric is not an atomic gauge family")
	}
	family.replace(values)
	return nil
}
