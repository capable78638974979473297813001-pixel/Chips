// Package costs keeps what each client's requests cost in dollars, per
// hardware type and UTC day, so a platform team can answer "what did team X
// spend on H100s this week" instead of reading token counts.
package costs

import (
	"encoding/json"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// Idle is the client name idle hardware time is booked under: running cost
// of backends with nothing running on them.
const Idle = "(idle)"

// Retention is how many days of costs are kept.
const Retention = 400

const dayFormat = "2006-01-02"

type key struct {
	day      string
	client   string
	hardware string
}

// Ledger accumulates dollars by day, client and hardware. Safe for concurrent use.
type Ledger struct {
	mu      sync.Mutex
	dollars map[key]float64
	pruned  string // the day pruneLocked last ran
	now     func() time.Time
}

func New() *Ledger { return &Ledger{dollars: map[key]float64{}, now: time.Now} }

// Add books dollars spent now by client on hardware.
func (l *Ledger) Add(client, hardware string, dollars float64) {
	if dollars <= 0 || math.IsNaN(dollars) || math.IsInf(dollars, 0) {
		return
	}
	day := l.now().UTC().Format(dayFormat)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dollars[key{day, client, hardware}] += dollars
	l.pruneLocked()
}

// pruneLocked drops days older than Retention. It scans at most once a day.
func (l *Ledger) pruneLocked() {
	today := l.now().UTC()
	if d := today.Format(dayFormat); d == l.pruned {
		return
	} else {
		l.pruned = d
	}
	cutoff := today.AddDate(0, 0, -Retention).Format(dayFormat)
	for k := range l.dollars {
		if k.day < cutoff {
			delete(l.dollars, k)
		}
	}
}

// Line is one client's spend in a report.
type Line struct {
	Client     string             `json:"client"`
	Dollars    float64            `json:"dollars"`
	ByHardware map[string]float64 `json:"by_hardware"`
	ByDay      map[string]float64 `json:"by_day"`
}

// HardwareLine is one hardware type's spend in a report.
type HardwareLine struct {
	Hardware string  `json:"hardware"`
	Dollars  float64 `json:"dollars"`
	// IdleDollars is the part of Dollars that was idle time.
	IdleDollars float64 `json:"idle_dollars"`
}

// Report is spend over a range of UTC days, inclusive.
type Report struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Currency string         `json:"currency"`
	Total    float64        `json:"total_dollars"`
	Idle     float64        `json:"idle_dollars"`
	Clients  []Line         `json:"clients"`
	Hardware []HardwareLine `json:"hardware"`
}

// Report sums spend from one UTC day to another (2006-01-02, inclusive).
// Clients are sorted by spend, highest first; idle time is not a client
// but is counted in Total and per hardware.
func (l *Ledger) Report(from, to string) Report {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := Report{From: from, To: to, Currency: "USD", Clients: []Line{}, Hardware: []HardwareLine{}}
	clients := map[string]*Line{}
	hw := map[string]*HardwareLine{}
	for k, v := range l.dollars {
		if k.day < from || k.day > to {
			continue
		}
		r.Total += v
		h := hw[k.hardware]
		if h == nil {
			h = &HardwareLine{Hardware: k.hardware}
			hw[k.hardware] = h
		}
		h.Dollars += v
		if k.client == Idle {
			r.Idle += v
			h.IdleDollars += v
			continue
		}
		c := clients[k.client]
		if c == nil {
			c = &Line{Client: k.client, ByHardware: map[string]float64{}, ByDay: map[string]float64{}}
			clients[k.client] = c
		}
		c.Dollars += v
		c.ByHardware[k.hardware] += v
		c.ByDay[k.day] += v
	}
	for _, c := range clients {
		c.Dollars = cents(c.Dollars)
		for k, v := range c.ByHardware {
			c.ByHardware[k] = cents(v)
		}
		for k, v := range c.ByDay {
			c.ByDay[k] = cents(v)
		}
		r.Clients = append(r.Clients, *c)
	}
	for _, h := range hw {
		h.Dollars, h.IdleDollars = cents(h.Dollars), cents(h.IdleDollars)
		r.Hardware = append(r.Hardware, *h)
	}
	r.Total, r.Idle = cents(r.Total), cents(r.Idle)
	slices.SortFunc(r.Clients, func(a, b Line) int {
		if a.Dollars != b.Dollars {
			return cmpDesc(a.Dollars, b.Dollars)
		}
		return strings.Compare(a.Client, b.Client)
	})
	slices.SortFunc(r.Hardware, func(a, b HardwareLine) int {
		if a.Dollars != b.Dollars {
			return cmpDesc(a.Dollars, b.Dollars)
		}
		return strings.Compare(a.Hardware, b.Hardware)
	})
	return r
}

// LastDays is a report on the n UTC days ending today.
func (l *Ledger) LastDays(n int) Report {
	today := l.now().UTC()
	return l.Report(today.AddDate(0, 0, -(n-1)).Format(dayFormat), today.Format(dayFormat))
}

// cents rounds to a hundredth of a cent, enough for small requests to show.
func cents(v float64) float64 { return math.Round(v*1e4) / 1e4 }

func cmpDesc(a, b float64) int {
	if a > b {
		return -1
	}
	return 1
}

// saved is the on-disk form.
type saved struct {
	Day      string  `json:"day"`
	Client   string  `json:"client"`
	Hardware string  `json:"hardware"`
	Dollars  float64 `json:"dollars"`
}

// Save writes the ledger as JSON.
func (l *Ledger) Save(w io.Writer) error {
	l.mu.Lock()
	out := make([]saved, 0, len(l.dollars))
	for k, v := range l.dollars {
		out = append(out, saved{k.day, k.client, k.hardware, v})
	}
	l.mu.Unlock()
	slices.SortFunc(out, func(a, b saved) int {
		return strings.Compare(a.Day+a.Client+a.Hardware, b.Day+b.Client+b.Hardware)
	})
	return json.NewEncoder(w).Encode(out)
}

// Load adds what Save wrote to the ledger.
func (l *Ledger) Load(r io.Reader) error {
	var in []saved
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range in {
		if s.Dollars > 0 {
			l.dollars[key{s.Day, s.Client, s.Hardware}] += s.Dollars
		}
	}
	l.pruned = ""
	l.pruneLocked()
	return nil
}
