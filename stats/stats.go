// Package stats tracks staticics for the gomodfs file system.
package stats

import (
	"cmp"
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

// OpStat holds statistics for a single operation type.
//
// All fields are guarded by the Stats.mu mutex.
type OpStat struct {
	Started  int
	Ended    int
	Errs     int
	TotalDur time.Duration
}

type Stats struct {
	mu  sync.Mutex
	ops map[string]*OpStat
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
	}
	return as
}

func (s *ActiveSpan) End(err error) {
	if s.done {
		panic("End called twice on span")
	}
	s.done = true

	st, os := s.st, s.os
	if (st == nil) != (os == nil) {
		panic("End called on span with mismatched st/os")
	}
	if os == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	os.Ended++
	if err != nil {
		os.Errs++
	}
	d := time.Since(s.start)
	os.TotalDur += d
}

func (st *Stats) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if st == nil {
		http.Error(w, "stats not enabled", http.StatusInternalServerError)
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	io.WriteString(w, `<html><body cellpadding=3 border=1><table>
	<tr><th>op</th><th>calls</th><th>pending</th><th>errs</th><th>avg</th><th>total</th></tr>
	`)

	keys := slices.Sorted(maps.Keys(st.ops))
	for _, op := range keys {
		v := st.ops[op]
		fmt.Fprintf(w, "<tr><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%v</td><td>%v</td></tr>\n",
			html.EscapeString(op),
			v.Ended,
			v.Started-v.Ended, // pending
			v.Errs,
			(v.TotalDur / time.Duration(cmp.Or(v.Ended, 1))).Round(time.Microsecond),
			v.TotalDur.Round(time.Millisecond))
	}
	io.WriteString(w, `</table></body></html>`)
}
