package costs

import (
	"bytes"
	"testing"
	"time"
)

func at(day string) func() time.Time {
	return func() time.Time {
		t, _ := time.Parse(time.DateOnly, day)
		return t.Add(12 * time.Hour)
	}
}

func TestReportByClientHardwareAndDay(t *testing.T) {
	l := New()
	l.now = at("2026-10-05")
	l.Add("team-x", "H100", 100)
	l.Add("team-x", "MI300X", 12)
	l.Add("team-y", "H100", 5)
	l.Add(Idle, "H100", 20)
	l.now = at("2026-10-06")
	l.Add("team-x", "H100", 300)
	l.now = at("2026-09-01")
	l.Add("team-x", "H100", 999) // outside the week

	l.now = at("2026-10-07")
	r := l.LastDays(7)
	if r.From != "2026-10-01" || r.To != "2026-10-07" {
		t.Fatalf("range %s..%s", r.From, r.To)
	}
	if r.Total != 437 || r.Idle != 20 {
		t.Errorf("total %v idle %v, want 437 and 20", r.Total, r.Idle)
	}
	if len(r.Clients) != 2 || r.Clients[0].Client != "team-x" || r.Clients[0].Dollars != 412 {
		t.Fatalf("clients = %+v", r.Clients)
	}
	x := r.Clients[0]
	if x.ByHardware["H100"] != 400 || x.ByHardware["MI300X"] != 12 || x.ByDay["2026-10-06"] != 300 {
		t.Errorf("team-x breakdown = %+v", x)
	}
	if r.Hardware[0].Hardware != "H100" || r.Hardware[0].Dollars != 425 || r.Hardware[0].IdleDollars != 20 {
		t.Errorf("hardware = %+v", r.Hardware)
	}
}

func TestSaveLoadAndRetention(t *testing.T) {
	l := New()
	l.now = at("2025-01-01")
	l.Add("old", "H100", 1)
	l.now = at("2026-10-05")
	l.Add("team-x", "H100", 2.5)
	var buf bytes.Buffer
	if err := l.Save(&buf); err != nil {
		t.Fatal(err)
	}
	m := New()
	m.now = at("2026-10-05")
	if err := m.Load(&buf); err != nil {
		t.Fatal(err)
	}
	r := m.Report("2000-01-01", "2100-01-01")
	if len(r.Clients) != 1 || r.Clients[0].Client != "team-x" || r.Total != 2.5 {
		t.Errorf("after reload: %+v (days past retention should be gone)", r)
	}
}

func TestIgnoresNonPositive(t *testing.T) {
	l := New()
	l.Add("a", "h", 0)
	l.Add("a", "h", -1)
	if r := l.LastDays(1); len(r.Clients) != 0 {
		t.Errorf("clients = %+v", r.Clients)
	}
}
