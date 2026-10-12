package pool

import (
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

// Cost accounting. Each backend's running cost (cost_per_hour) is split
// between the requests running on it at each moment: with n running, each
// is charged 1/n of the cost for that time. Instead of touching every
// running request when n changes, a backend keeps a "cost clock", the
// dollars charged so far to one request that has been running since the
// backend started. A request's share is the clock's advance between its
// start and its end. Time with nothing running is idle cost.
//
// n is the router's own in-flight count plus other clients' requests on
// the engine, so traffic from other routers (cluster mode) or clients
// calling the engine directly takes its share instead of the cost being
// pinned on this router's requests. Other clients' requests are what the
// engine last reported (running and queued) beyond this router's in-flight
// count at that scrape. Comparing with the in-flight count now instead
// would undercharge: the scrape is up to an interval old, so after requests
// finish it still counts them, and their share would go to no one.

// pricing is what a backend charges.
type pricing struct {
	perSecond          float64
	promptPerToken     float64
	completionPerToken float64
	hardwareName       string // configured; empty means derive it
}

func pricingOf(cb config.Backend) pricing {
	return pricing{
		perSecond:          cb.CostPerHour / 3600,
		promptPerToken:     cb.CostPerMillionPromptTokens / 1e6,
		completionPerToken: cb.CostPerMillionCompletionTokens / 1e6,
		hardwareName:       cb.HardwareName,
	}
}

// setPricingLocked changes b's prices from now on.
func (b *Backend) setPricingLocked(pr pricing, now time.Time) {
	b.advanceCostLocked(now)
	b.price = pr
}

// advanceCostLocked moves b's cost clock to now. Call it before anything
// that changes how many requests share b: in-flight count, engine stats, prices.
func (b *Backend) advanceCostLocked(now time.Time) {
	if b.costAt.IsZero() {
		b.costAt = now
		return
	}
	dt := now.Sub(b.costAt).Seconds()
	b.costAt = now
	if dt <= 0 || b.price.perSecond == 0 {
		return
	}
	n := float64(b.inflight)
	if st, ok := b.engineStats(now); ok && st.Running >= 0 {
		n += max(0, st.Running+st.Waiting-float64(b.engOurs))
	}
	if n > 0 {
		b.costClock += b.price.perSecond * dt / n
	} else {
		b.idleCost += b.price.perSecond * dt
	}
}

// hardwareNameLocked is how b is grouped in cost reports: the configured
// hardware_name, else the accelerator chipagent reports, else b's name.
func (b *Backend) hardwareNameLocked() string {
	if b.price.hardwareName != "" {
		return b.price.hardwareName
	}
	if b.hw != nil {
		if acc := b.hw.Accelerators(); len(acc) > 0 && acc[0].Name != "" {
			return acc[0].Name
		}
	}
	return b.Name
}

// Cost is what the request that held l was charged on its backend: its
// share of the backend's running cost while it held the slot, plus the
// backend's token prices for the tokens given. Call it after Release.
func (l *Lease) Cost(promptTokens, completionTokens int64) float64 {
	return l.timeCost + float64(promptTokens)*l.price.promptPerToken + float64(completionTokens)*l.price.completionPerToken
}

// Hardware is how the lease's backend is grouped in cost reports.
func (l *Lease) Hardware() string { return l.hardware }

// IdleCost is one backend's running cost with nothing running on it.
type IdleCost struct {
	Backend  string
	Hardware string
	// Dollars since the router started or the backend joined the pool.
	Dollars float64
}

// IdleCosts reports every backend that has a cost_per_hour.
func (p *Pool) IdleCosts() []IdleCost {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []IdleCost
	for _, b := range p.backends {
		if b.price.perSecond > 0 {
			b.advanceCostLocked(now)
			out = append(out, IdleCost{b.Name, b.hardwareNameLocked(), b.idleCost})
		}
	}
	return out
}
