package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

type Price struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
	CacheHour  *float64 `json:"cache_write_1h"`
}

func (p Price) valid() bool {
	if p.Input == nil || p.Output == nil {
		return false
	}
	for _, v := range []*float64{p.Input, p.Output, p.CacheRead, p.CacheWrite, p.CacheHour} {
		if v != nil && *v < 0 {
			return false
		}
	}
	return true
}

// Input is uncached input, with cached and cache-creation units kept separate.
type Units struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Read   int64 `json:"cache_read"`
	Write  int64 `json:"cache_write_5m"`
	Hour   int64 `json:"cache_write_1h"`
}

func (p Price) cost(u *Units) *float64 {
	if u == nil || !p.valid() {
		return nil
	}
	var sum float64
	for _, term := range []struct {
		n    int64
		rate *float64
	}{{u.Input, p.Input}, {u.Output, p.Output}, {u.Read, p.CacheRead}, {u.Write, p.CacheWrite}, {u.Hour, p.CacheHour}} {
		if term.n < 0 {
			return nil
		}
		if term.n == 0 {
			continue
		}
		if term.rate == nil {
			return nil
		}
		sum += float64(term.n) * *term.rate / 1e6
	}
	return &sum
}

type auditEntry struct {
	BudgetDay    string    `json:"budget_day,omitempty"`
	BudgetDelta  float64   `json:"budget_delta_usd,omitempty"`
	At           time.Time `json:"at"`
	Request      string    `json:"request_id"`
	Project      string    `json:"project"`
	Target       string    `json:"target"`
	Attempt      int       `json:"attempt"`
	Outcome      string    `json:"outcome"`
	Status       int       `json:"upstream_status,omitempty"`
	Units        *Units    `json:"tokens"`
	Cost         *float64  `json:"estimated_cost_usd"`
	PriceVersion string    `json:"price_version,omitempty"`
}

func (s *Server) checkLedger() error {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	lock, err := os.OpenFile(s.cfg.Ledger+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := lockLedger(lock); err != nil {
		lock.Close()
		return err
	}
	if err := s.loadLedger(); err != nil {
		lock.Close()
		return err
	}
	s.ledgerLock = lock
	return nil
}

// Called under ledgerMu. Restore today's debits, including reservations
// made before a crash and attempts for which the provider omitted usage.
func (s *Server) loadLedger() error {
	f, err := os.OpenFile(s.cfg.Ledger, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	today := time.Now().UTC().Format(time.DateOnly)
	spent := map[string]float64{}
	d := json.NewDecoder(f)
	for {
		var e auditEntry
		err := d.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if e.BudgetDay == today {
			spent[e.Project] += e.BudgetDelta
		}
	}
	s.spent = spent
	s.budgetDay = today
	return nil
}
func (s *Server) audit(e auditEntry) error {
	e.At = time.Now().UTC()
	e.PriceVersion = s.cfg.PriceVersion
	e.Cost = s.cfg.Prices[e.Target].cost(e.Units)
	if e.Outcome == "cached" {
		zero := 0.
		e.Cost = &zero
	}
	if s.cfg.Ledger == "" {
		slog.Info("usage", "attempt", e)
		return nil
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	if s.ledgerFailed {
		return errors.New("ledger requires recovery")
	}
	today := e.At.Format(time.DateOnly)
	if s.budgetDay != today {
		if err := s.loadLedger(); err != nil {
			return err
		}
	}
	if b, ok := s.cfg.Budgets[e.Project]; ok && e.Outcome != "cached" {
		if e.Outcome == "started" {
			if s.spent[e.Project]+b.Reserve > b.Daily {
				return errBudget
			}
			e.BudgetDelta = b.Reserve
		} else if e.Cost != nil {
			e.BudgetDelta = *e.Cost - b.Reserve
		}
	}
	f, err := os.OpenFile(s.cfg.Ledger, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(e)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		s.ledgerFailed = true
		return err
	}
	if e.BudgetDay == s.budgetDay {
		s.spent[e.Project] += e.BudgetDelta
	}
	slog.Info("usage", "attempt", e)
	return nil
}

// Summarize completed attempt records for this credential's project only.
// An interrupted attempt with missing usage stays visibly unpriced.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	project := keyOf(r).Project
	totals := struct {
		Project  string  `json:"project"`
		Attempts int     `json:"finished_attempts"`
		Open     int     `json:"unsettled_attempts"`
		Hits     int     `json:"cache_hits"`
		Unknown  int     `json:"unpriced_attempts"`
		Cost     float64 `json:"known_estimated_cost_usd"`
		Tokens   Units   `json:"known_tokens"`
	}{Project: project}
	s.ledgerMu.Lock()
	f, err := os.Open(s.cfg.Ledger)
	var size int64
	if err == nil {
		var st os.FileInfo
		st, err = f.Stat()
		if err == nil {
			size = st.Size()
		}
	}
	s.ledgerMu.Unlock()
	if f != nil {
		defer f.Close()
	}
	if err != nil {
		nativeError(w, "usage", 503, "accounting_unavailable")
		return
	}
	dec := json.NewDecoder(io.LimitReader(f, size))
	for {
		var e auditEntry
		err := dec.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			nativeError(w, "usage", 503, "accounting_unavailable")
			return
		}
		if e.Project != project {
			continue
		}
		if e.Outcome == "started" {
			totals.Open++
			continue
		}
		if e.Outcome == "cached" {
			totals.Hits++
			continue
		}
		totals.Open--
		totals.Attempts++
		if e.Cost == nil {
			totals.Unknown++
		} else {
			totals.Cost += *e.Cost
		}
		if u := e.Units; u != nil {
			totals.Tokens.Input += u.Input
			totals.Tokens.Output += u.Output
			totals.Tokens.Read += u.Read
			totals.Tokens.Write += u.Write
			totals.Tokens.Hour += u.Hour
		}
	}
	writeJSON(w, 200, totals)
}
