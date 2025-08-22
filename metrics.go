// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package gomodfs

import (
	"expvar"
	"reflect"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// RegisterMetrics registers the FS's expvar.Int fields and other stats as
// Prometheus metrics in the provided registry.
func (fs *FS) RegisterMetrics(reg *prometheus.Registry) {
	rv := reflect.ValueOf(fs).Elem()
	t := rv.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		if sf.Type == reflect.TypeFor[expvar.Int]() {
			expvarInt := rv.Field(i).Addr().Interface().(*expvar.Int)
			typ := sf.Tag.Get("type")
			name := sf.Tag.Get("name")
			if typ == "" {
				panic("missing type tag for " + sf.Name)
			}
			if name == "" {
				panic("missing name tag for " + sf.Name)
			}
			help := sf.Tag.Get("help")
			metricName := "gomodfs_" + name

			if tag := sf.Tag.Get("type"); tag != "" {
				if tag == "gauge" {
					reg.MustRegister(singleMetricCollector{&expvarGaugeMetric{
						desc: prometheus.NewDesc(metricName, help, nil, nil),
						v:    expvarInt,
					}})
				} else if tag == "counter" {
					reg.MustRegister(singleMetricCollector{&expvarCounterMetric{
						desc: prometheus.NewDesc(metricName, help, nil, nil),
						v:    expvarInt,
					}})
				}
			}
		}
	}

}

// expvarCounterMetric is a Prometheus counter metric backed by an expvar.Int.
type expvarCounterMetric struct {
	desc *prometheus.Desc
	v    *expvar.Int
}

var _ prometheus.Metric = (*expvarCounterMetric)(nil)

func (m *expvarCounterMetric) Desc() *prometheus.Desc { return m.desc }

func (m *expvarCounterMetric) Write(out *dto.Metric) error {
	val := float64(m.v.Value())
	out.Counter = &dto.Counter{Value: &val}
	return nil
}

// expvarGaugeMetric is a Prometheus gauge metric backed by an expvar.Int.
type expvarGaugeMetric struct {
	desc *prometheus.Desc
	v    *expvar.Int
}

var _ prometheus.Metric = (*expvarGaugeMetric)(nil)

func (m *expvarGaugeMetric) Desc() *prometheus.Desc { return m.desc }

func (m *expvarGaugeMetric) Write(out *dto.Metric) error {
	val := float64(m.v.Value())
	out.Gauge = &dto.Gauge{Value: &val}
	return nil
}

// singleMetricCollector is a Prometheus collector that collects a single metric.
type singleMetricCollector struct {
	metric prometheus.Metric
}

var _ prometheus.Collector = singleMetricCollector{}

func (c singleMetricCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.metric.Desc() }
func (c singleMetricCollector) Collect(ch chan<- prometheus.Metric) { ch <- c.metric }
