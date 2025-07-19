// Package stats tracks staticics for the gomodfs file system.
package stats

import (
	"fmt"
	"html"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

type OpStat struct {
	Count int
	Errs  int
	Total time.Duration
}

type Stats struct {
	mu  sync.Mutex
	ops map[string]*OpStat
}

type ActiveSpan struct {
	st    *Stats
	op    string
	start time.Time
	done  bool
}

// StartSpan starts a new operation span for the given op.
//
// If s is nil, a non-nil ActiveSpan is returned that does
// nothing when its End method is called.
func (st *Stats) StartSpan(op string) *ActiveSpan {
	return &ActiveSpan{
		st:    st,
		op:    op,
		start: time.Now(),
		done:  false,
	}
}

func (s *ActiveSpan) End(err error) {
	if s.done {
		panic("End called twice on span")
	}
	s.done = true

	st := s.st
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	op, ok := st.ops[s.op]
	if !ok {
		if st.ops == nil {
			st.ops = make(map[string]*OpStat)
		}
		op = &OpStat{}
		st.ops[s.op] = op
	}
	op.Count++
	if err != nil {
		op.Errs++
	}
	d := time.Since(s.start)
	op.Total += d
}

func (st *Stats) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if st == nil {
		http.Error(w, "stats not enabled", http.StatusInternalServerError)
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	io.WriteString(w, `<html><body cellpadding=3 border=1><table>
	<tr><th>op</th><th>calls</th><th>errs</th><th>avg</th><th>total</th></tr>
	`)

	keys := slices.Sorted(maps.Keys(st.ops))
	for _, op := range keys {
		v := st.ops[op]
		fmt.Fprintf(w, "<tr><td>%s</td><td>%d</td><td>%d</td><td>%v</td><td>%v</td></tr>\n",
			html.EscapeString(op),
			v.Count,
			v.Errs,
			(v.Total / time.Duration(v.Count)).Round(time.Millisecond),
			v.Total.Round(time.Millisecond))
	}
	io.WriteString(w, `</table></body></html>`)
}
