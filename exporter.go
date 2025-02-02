package saramaprom

// This code is based on a code of https://github.com/deathowl/go-metrics-prometheus library.

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rcrowley/go-metrics"
)

type labels map[string]string

type exporter struct {
	opt              Options
	registry         MetricsRegistry
	promRegistry     prometheus.Registerer
	gauges           map[string]prometheus.Gauge
	customMetrics    map[string]*customCollector
	histogramBuckets []float64
	timerBuckets     []float64
	mu               sync.RWMutex

	labelsMap      map[string]labels
	metricsNameMap map[string]bool
}

func (c *exporter) sanitizeName(key string) string {
	ret := []byte(key)
	for i := 0; i < len(ret); i++ {
		c := key[i]
		allowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == ':' || (c >= '0' && c <= '9')
		if !allowed {
			ret[i] = '_'
		}
	}
	return string(ret)
}

func (c *exporter) createKey(name string) string {
	return c.opt.Namespace + "_" + c.opt.Subsystem + "_" + name
}

func (c *exporter) gaugeFromNameAndValue(name string, val float64) error {
	shortName, labels, skip := c.metricNameAndLabels(name)
	if skip {
		if c.opt.Debug {
			fmt.Printf("[saramaprom] skip metric %q because there is no broker or topic labels\n", name)
		}
		return nil
	}

	if _, exists := c.gauges[name]; !exists {
		labelNames := make([]string, 0, len(labels))
		for labelName := range labels {
			labelNames = append(labelNames, labelName)
		}

		c.mu.Lock()
		metricName := c.sanitizeName(shortName)
		if c.metricsNameMap[metricName] {
			c.mu.Unlock()
			return nil
		}
		c.metricsNameMap[metricName] = true
		c.mu.Unlock()

		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: c.sanitizeName(c.opt.Namespace),
			Subsystem: c.sanitizeName(c.opt.Subsystem),
			Name:      c.sanitizeName(shortName),
			Help:      shortName,
		}, labelNames)

		if err := c.promRegistry.Register(g); err != nil {
			switch err := err.(type) {
			case prometheus.AlreadyRegisteredError:
				var ok bool
				g, ok = err.ExistingCollector.(*prometheus.GaugeVec)
				if !ok {
					return fmt.Errorf("prometheus collector already registered but it's not *prometheus.GaugeVec: %v", g)
				}
			default:
				return err
			}
		}
		c.gauges[name] = g.With(labels)
	}

	c.gauges[name].Set(val)
	return nil
}

func (c *exporter) metricNameAndLabels(metricName string) (newName string, labels map[string]string, skip bool) {
	newName, broker, topic := parseMetricName(metricName)
	if broker == "" && topic == "" {
		// skip metrics for total
		return newName, labels, true
	}

	var ok bool
	c.mu.RLock()
	labels, ok = c.labelsMap[metricName]
	c.mu.RUnlock()

	if !ok {
		labels = c.opt.Labels
		if broker != "" {
			labels["broker"] = broker
		}
		if topic != "" {
			labels["topic"] = topic
		}

		c.mu.Lock()
		c.labelsMap[metricName] = labels
		c.mu.Unlock()
	}

	return newName, labels, false
}

func parseMetricName(name string) (newName, broker, topic string) {
	if i := strings.Index(name, "-for-broker-"); i >= 0 {
		newName = name[:i]
		broker = name[i+len("-for-broker-"):]
		return
	}
	if i := strings.Index(name, "-for-topic-"); i >= 0 {
		newName = name[:i]
		topic = name[i+len("-for-topic-"):]
		return
	}
	return name, "", ""
}

func (c *exporter) histogramFromNameAndMetric(name string, goMetric interface{}, buckets []float64) error {
	key := c.createKey(name)
	collector, exists := c.customMetrics[key]
	if !exists {
		collector = newCustomCollector(&c.mu)
		c.promRegistry.MustRegister(collector)
		c.customMetrics[key] = collector
	}

	var ps []float64
	var count uint64
	var sum float64
	var typeName string

	switch metric := goMetric.(type) {
	case metrics.Histogram:
		snapshot := metric.Snapshot()
		ps = snapshot.Percentiles(buckets)
		count = uint64(snapshot.Count())
		sum = float64(snapshot.Sum())
		typeName = "histogram"
	case metrics.Timer:
		snapshot := metric.Snapshot()
		ps = snapshot.Percentiles(buckets)
		count = uint64(snapshot.Count())
		sum = float64(snapshot.Sum())
		typeName = "timer"
	default:
		return fmt.Errorf("unexpected metric type %T", goMetric)
	}

	bucketVals := make(map[float64]uint64)
	for ii, bucket := range buckets {
		bucketVals[bucket] = uint64(ps[ii])
	}

	name, labels, skip := c.metricNameAndLabels(name)
	if skip {
		return nil
	}

	c.mu.Lock()
	metricName := c.sanitizeName(name) + "_" + typeName
	if c.metricsNameMap[metricName] {
		c.mu.Unlock()
		return nil
	}
	c.metricsNameMap[metricName] = true
	c.mu.Unlock()

	desc := prometheus.NewDesc(
		prometheus.BuildFQName(
			c.sanitizeName(c.opt.Namespace),
			c.sanitizeName(c.opt.Subsystem),
			metricName,
		),
		c.sanitizeName(name),
		nil,
		labels,
	)

	hist, err := prometheus.NewConstHistogram(desc, count, sum, bucketVals)
	if err != nil {
		return err
	}
	c.mu.Lock()
	collector.metric = hist
	c.mu.Unlock()
	return nil
}

func (c *exporter) update() error {
	if c.opt.Debug {
		fmt.Print("[saramaprom] update()\n")
	}
	var err error
	c.registry.Each(func(name string, i interface{}) {
		switch metric := i.(type) {
		case metrics.Counter:
			err = c.gaugeFromNameAndValue(name, float64(metric.Count()))
		case metrics.Gauge:
			err = c.gaugeFromNameAndValue(name, float64(metric.Value()))
		case metrics.GaugeFloat64:
			err = c.gaugeFromNameAndValue(name, float64(metric.Value()))
		case metrics.Histogram: // sarama
			samples := metric.Snapshot().Sample().Values()
			if len(samples) > 0 {
				lastSample := samples[len(samples)-1]
				err = c.gaugeFromNameAndValue(name, float64(lastSample))
			}
			if err == nil {
				err = c.histogramFromNameAndMetric(name, metric, c.histogramBuckets)
			}
		case metrics.Meter: // sarama
			lastSample := metric.Snapshot().Rate1()
			err = c.gaugeFromNameAndValue(name, float64(lastSample))
		case metrics.Timer:
			lastSample := metric.Snapshot().Rate1()
			err = c.gaugeFromNameAndValue(name, float64(lastSample))
			if err == nil {
				err = c.histogramFromNameAndMetric(name, metric, c.timerBuckets)
			}
		}
	})
	return err
}

// for collecting prometheus.constHistogram objects
type customCollector struct {
	prometheus.Collector

	metric prometheus.Metric
	mu     sync.Locker
}

func newCustomCollector(mu sync.Locker) *customCollector {
	return &customCollector{
		mu: mu,
	}
}

func (c *customCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	if c.metric != nil {
		val := c.metric
		ch <- val
	}
	c.mu.Unlock()
}

func (c *customCollector) Describe(_ chan<- *prometheus.Desc) {
	// empty method to fulfill prometheus.Collector interface
}
