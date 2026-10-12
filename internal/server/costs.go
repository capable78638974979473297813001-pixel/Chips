package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/costs"
	"github.com/capable78638974979473297813001-pixel/chips/internal/pool"
)

// charge books what a released lease cost to the client who held it.
func (s *Server) charge(rl *requestLog, who string, l *pool.Lease, promptTokens, completionTokens int64) {
	c := l.Cost(promptTokens, completionTokens)
	if c <= 0 {
		return
	}
	rl.cost += c
	s.costs.Add(who, l.Hardware(), c)
	s.clientCost.Add(c, who, l.Hardware())
}

// handleCosts reports spend in dollars per client and hardware:
// ?days=7 (the default) for the last week including today, or
// ?from=2026-10-01&to=2026-10-07 for a range of UTC days.
func (s *Server) handleCosts(w http.ResponseWriter, r *http.Request) {
	s.bookIdle()
	q := r.URL.Query()
	var rep costs.Report
	switch from, to := q.Get("from"), q.Get("to"); {
	case from != "" || to != "":
		if to == "" {
			to = time.Now().UTC().Format(time.DateOnly)
		}
		if _, err := time.Parse(time.DateOnly, from); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "from must be a date like 2026-10-01")
			return
		}
		if _, err := time.Parse(time.DateOnly, to); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "to must be a date like 2026-10-07")
			return
		}
		rep = s.costs.Report(from, to)
	default:
		days := 7
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > costs.Retention {
				writeError(w, http.StatusBadRequest, "invalid_request", "days must be between 1 and "+strconv.Itoa(costs.Retention))
				return
			}
			days = n
		}
		rep = s.costs.LastDays(days)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rep)
}

// bookIdle moves idle backend time accrued since the last call into the
// ledger, under costs.Idle.
func (s *Server) bookIdle() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if s.idleBooked == nil {
		s.idleBooked = map[string]float64{}
	}
	for _, c := range s.pool.IdleCosts() {
		if d := c.Dollars - s.idleBooked[c.Backend]; d > 0 {
			s.costs.Add(costs.Idle, c.Hardware, d)
		}
		s.idleBooked[c.Backend] = c.Dollars
	}
}

// RunCosts books idle time into the cost ledger every minute, so each
// day's idle cost lands on that day.
func (s *Server) RunCosts(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.bookIdle()
		}
	}
}
