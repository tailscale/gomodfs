// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package stats tracks staticics for the gomodfs file system.
package stats

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// OpStat holds statistics for a single operation type.
//
// All fields are guarded by the Stats.mu mutex.
type OpStat struct {
	Started  int
	Ended    int
	Errs     int // all errors
	CtxErrs  int // subset of Errs where the ctx is done
	TotalDur time.Duration
}

// NumSuccess returns the number of successful operations.
func (os *OpStat) NumSuccess() int {
	return os.Ended - os.Errs
}

// Stats holds the operation statistics for the gomodfs file system.
//
// If nil, no statistics are collected.
type Stats struct {
	MetricOpStarted  *prometheus.CounterVec // used if non-nil
	MetricOpEnded    *prometheus.CounterVec // used if non-nil
	MetricOpDuration *prometheus.CounterVec // used if non-nil

	mu  sync.Mutex
	ops map[string]*OpStat
}

// NewStatsWithRegstry returns a new Stats instance with metrics initialized and
// registered to the provided registry.
func NewStatsWithRegistry(reg *prometheus.Registry) *Stats {
	s := &Stats{}
	s.MetricOpStarted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gomodfs_operation_started",
			Help: "Total number of operations started by gomodfs.",
		},
		[]string{"op"},
	)
	s.MetricOpEnded = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gomodfs_operation_ended",
			Help: "Total number of operations ended by gomodfs.",
		},
		[]string{"op"},
	)
	s.MetricOpDuration = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gomodfs_operation_duration_seconds_total",
			Help: "Total duration of operations performed by gomodfs in seconds.",
		},
		[]string{"op"},
	)
	reg.MustRegister(s.MetricOpStarted)
	reg.MustRegister(s.MetricOpEnded)
	reg.MustRegister(s.MetricOpDuration)
	return s
}

// Clone returns a clone of the current operation statistics,
// keyed by operation name.
//
// If st is nil, it returns nil. Otherwise it returns a non-nil map.
func (st *Stats) Clone() map[string]*OpStat {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	clone := make(map[string]*OpStat, len(st.ops))
	for k, v := range st.ops {
		shallowCopy := *v // shallow copy
		clone[k] = &shallowCopy
	}
	return clone
}

type ActiveSpan struct {
	st    *Stats
	os    *OpStat // nil if Stats is nil
	op    string
	start time.Time
	done  bool
}

// StartSpan starts a new operation span for the given op.
//
// If s is nil, a non-nil ActiveSpan is returned that does
// nothing when its End method is called.
func (st *Stats) StartSpan(op string) *ActiveSpan {
	as := &ActiveSpan{
		st:    st,
		op:    op,
		start: time.Now(),
		done:  false,
	}

	if st != nil {
		st.mu.Lock()
		defer st.mu.Unlock()

		if st.ops == nil {
			st.ops = make(map[string]*OpStat)
		}
		os, ok := st.ops[op]
		if !ok {
			os = &OpStat{}
			st.ops[op] = os
		}
		as.os = os
		os.Started++
		if st.MetricOpStarted != nil {
			st.MetricOpStarted.WithLabelValues(op).Inc()
		}
	}
	return as
}

func (s *ActiveSpan) End(err error) {
	if s.done {
		panic("End called twice on span")
	}
	s.done = true

	st, ost := s.st, s.os
	if (st == nil) != (ost == nil) {
		panic("End called on span with mismatched st/os")
	}
	if ost == nil {
		return
	}

	duration := time.Since(s.start)
	if st.MetricOpEnded != nil {
		st.MetricOpEnded.WithLabelValues(s.op).Inc()
	}
	if st.MetricOpDuration != nil {
		st.MetricOpDuration.WithLabelValues(s.op).Add(duration.Seconds())
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	ost.Ended++
	if err != nil {
		ost.Errs++
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			ost.CtxErrs++
		} else {
			errStr := err.Error()
			if errStr != "cache miss" { // TODO(bradfitz): trashy
				log.Printf("op %q error: %v", s.op, errStr)
			}
		}
	}
	ost.TotalDur += duration
}

func (st *Stats) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if st == nil {
		http.Error(w, "stats not enabled", http.StatusInternalServerError)
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	io.WriteString(w, `<html><body cellpadding=3 border=1><table>
	<tr><th align=left>op</th><th>calls</th><th>pending</th><th>errs</th><th>avg</th><th>total</th></tr>
	`)

	keys := slices.Sorted(maps.Keys(st.ops))
	for _, op := range keys {
		v := st.ops[op]
		ctxErrs := ""
		if v.Errs > 0 {
			if v.CtxErrs > 0 {
				ctxErrs = fmt.Sprintf("%d (%d ctx)", v.Errs, v.CtxErrs)
			} else {
				ctxErrs = fmt.Sprint(v.Errs)
			}
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td align=right>%d</td><td align=right>%d</td><td align=right>%s</td><td align=right>%v</td><td align=right>%v</td></tr>\n",
			html.EscapeString(op),
			v.Ended,
			v.Started-v.Ended, // pending
			ctxErrs,
			(v.TotalDur / time.Duration(cmp.Or(v.Ended, 1))).Round(time.Microsecond),
			v.TotalDur.Round(time.Millisecond))
	}
	io.WriteString(w, `</table></body></html>`)
}
