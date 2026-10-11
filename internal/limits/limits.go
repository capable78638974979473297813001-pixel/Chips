// Package limits enforces per-client limits and counts each client's usage,
// so one team cannot starve the others and every team's consumption can be
// reported back to it.
package limits

import (
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// Limits for one client. Zero means unlimited.
type Limits struct {
	MaxConcurrent     int   // requests in flight or queued at once
	RequestsPerMinute int   // sustained rate; bursts up to a minute's worth
	TokensPerDay      int64 // prompt + completion tokens per UTC day
}

// Rejection says why a request was refused and when to try again.
type Rejection struct {
	Reason     string // concurrency_limit, rate_limited, quota_exceeded
	Message    string
	RetryAfter time.Duration
}

// Usage is what one client has consumed since the router started.
type Usage struct {
	Client           string `json:"client"`
	Requests         int64  `json:"requests"`
	Rejected         int64  `json:"rejected"`
	InFlight         int    `json:"in_flight"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	// EstimatedTokens is the part of the token counts the router estimated
	// because the backend did not report usage.
	EstimatedTokens int64 `json:"estimated_tokens"`
	TokensToday     int64 `json:"tokens_today"`
	// ClusterTokensToday adds what peer routers reported for this client
	// today; daily quotas are checked against it.
	ClusterTokensToday int64 `json:"cluster_tokens_today"`
	// ClusterInFlight adds the requests peer routers last reported in
	// flight for this client; max_concurrent is checked against it.
	ClusterInFlight int    `json:"cluster_in_flight"`
	TokensPerDay    int64  `json:"tokens_per_day,omitempty"`
	Day             string `json:"day"` // UTC date TokensToday counts
}

type client struct {
	limits   Limits
	inflight int
	bucket   float64 // available requests in the rate bucket
	refilled time.Time
	usage    Usage
	day      time.Time // UTC midnight starting the current quota day
}

// Daily is one client's token use on one UTC day, as exchanged between
// routers in a cluster.
type Daily struct {
	Day    string `json:"day"` // 2006-01-02
	Tokens int64  `json:"tokens"`
	// InFlight is the client's requests in progress on the reporting router.
	InFlight int `json:"in_flight,omitempty"`
}

// Tracker holds every client's limits and counters. Safe for concurrent use.
type Tracker struct {
	mu      sync.Mutex
	clients map[string]*client
	remote  map[string]map[string]Daily // peer -> client -> usage
	seen    map[string]time.Time        // peer -> when its last report arrived
	peerTTL time.Duration               // how long a report keeps a peer live
	now     func() time.Time
}

func New() *Tracker {
	return &Tracker{clients: map[string]*client{}, remote: map[string]map[string]Daily{}, seen: map[string]time.Time{}, now: time.Now}
}

// SetPeerTTL makes limits cluster-wide: a peer that reported within ttl
// is live, its in-flight requests count against max_concurrent, and
// requests_per_minute is split evenly between this router and its live
// peers (so the fleet admits about the configured rate behind a load
// balancer that spreads clients evenly). Zero, the default, keeps rate
// and concurrency limits per router.
func (t *Tracker) SetPeerTTL(ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.peerTTL = ttl
}

// livePeersLocked is how many peers reported within the TTL.
func (t *Tracker) livePeersLocked(now time.Time) int {
	if t.peerTTL <= 0 {
		return 0
	}
	n := 0
	for _, at := range t.seen {
		if now.Sub(at) <= t.peerTTL {
			n++
		}
	}
	return n
}

// remoteInFlightLocked sums live peers' in-flight requests for name.
func (t *Tracker) remoteInFlightLocked(name string, now time.Time) int {
	if t.peerTTL <= 0 {
		return 0
	}
	n := 0
	for peer, byClient := range t.remote {
		if now.Sub(t.seen[peer]) <= t.peerTTL {
			n += byClient[name].InFlight
		}
	}
	return n
}

// LocalDaily returns this router's own token use today, per client, for
// sharing with peers.
func (t *Tracker) LocalDaily() map[string]Daily {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]Daily, len(t.clients))
	for name, c := range t.clients {
		t.rollDayLocked(c, t.now())
		out[name] = Daily{Day: c.day.Format(dayFormat), Tokens: c.usage.TokensToday, InFlight: c.inflight}
	}
	return out
}

// SetRemote records a peer router's latest report of its own usage. Each
// report replaces the previous one from that peer.
func (t *Tracker) SetRemote(peer string, usage map[string]Daily) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.remote[peer] = usage
	t.seen[peer] = t.now()
}

// remoteTodayLocked sums what peers reported for name on today's date.
func (t *Tracker) remoteTodayLocked(name string, now time.Time) int64 {
	today := midnight(now).Format(dayFormat)
	var n int64
	for _, byClient := range t.remote {
		if d, ok := byClient[name]; ok && d.Day == today {
			n += d.Tokens
		}
	}
	return n
}

const dayFormat = "2006-01-02"

// SetLimits installs limits by client name. Counters are kept, so a config
// reload does not reset anyone's usage or quota.
func (t *Tracker) SetLimits(byClient map[string]Limits) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, c := range t.clients {
		c.limits = byClient[name]
	}
	for name, l := range byClient {
		t.clientLocked(name).limits = l
	}
}

func (t *Tracker) clientLocked(name string) *client {
	c, ok := t.clients[name]
	if !ok {
		now := t.now()
		c = &client{refilled: now, day: midnight(now), usage: Usage{Client: name}}
		c.bucket = math.Inf(1) // full; clamped on first use
		t.clients[name] = c
	}
	return c
}

func midnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Admit checks name's limits. On success it returns a release function that
// must be called when the request finishes; otherwise it says why not.
func (t *Tracker) Admit(name string) (release func(), rejected *Rejection) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.clientLocked(name)
	now := t.now()
	t.rollDayLocked(c, now)
	l := c.limits

	reject := func(r *Rejection) (func(), *Rejection) {
		c.usage.Rejected++
		return nil, r
	}
	if l.MaxConcurrent > 0 && c.inflight+t.remoteInFlightLocked(name, now) >= l.MaxConcurrent {
		return reject(&Rejection{"concurrency_limit", "too many requests in progress for this API key", time.Second})
	}
	if l.TokensPerDay > 0 && c.usage.TokensToday+t.remoteTodayLocked(name, now) >= l.TokensPerDay {
		return reject(&Rejection{"quota_exceeded", "daily token quota for this API key is used up",
			c.day.Add(24 * time.Hour).Sub(now)})
	}
	if l.RequestsPerMinute > 0 {
		// This router's share of the rate, when peers are live.
		share := float64(l.RequestsPerMinute) / float64(1+t.livePeersLocked(now))
		rate := share / 60 // per second
		capacity := math.Max(1, share)
		c.bucket = math.Min(capacity, c.bucket+now.Sub(c.refilled).Seconds()*rate)
		c.refilled = now
		if c.bucket < 1 {
			wait := time.Duration((1 - c.bucket) / rate * float64(time.Second))
			return reject(&Rejection{"rate_limited", "request rate limit for this API key exceeded", wait})
		}
		c.bucket--
	}
	c.inflight++
	c.usage.Requests++
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			c.inflight--
			t.mu.Unlock()
		})
	}, nil
}

func (t *Tracker) rollDayLocked(c *client, now time.Time) {
	if today := midnight(now); today.After(c.day) {
		c.day = today
		c.usage.TokensToday = 0
	}
}

// Record adds the tokens a finished request used.
func (t *Tracker) Record(name string, prompt, completion int64, estimated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.clientLocked(name)
	t.rollDayLocked(c, t.now())
	c.usage.PromptTokens += prompt
	c.usage.CompletionTokens += completion
	c.usage.TokensToday += prompt + completion
	if estimated {
		c.usage.EstimatedTokens += prompt + completion
	}
}

// Snapshot returns every client's usage, sorted by name.
func (t *Tracker) Snapshot() []Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Usage, 0, len(t.clients))
	for _, c := range t.clients {
		t.rollDayLocked(c, t.now())
		u := c.usage
		u.InFlight = c.inflight
		u.TokensPerDay = c.limits.TokensPerDay
		u.Day = c.day.Format(dayFormat)
		u.ClusterTokensToday = u.TokensToday + t.remoteTodayLocked(c.usage.Client, t.now())
		u.ClusterInFlight = c.inflight + t.remoteInFlightLocked(c.usage.Client, t.now())
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b Usage) int { return strings.Compare(a.Client, b.Client) })
	return out
}

// Restore loads counters saved from Snapshot (for example before a
// restart). Today's tokens count only if they are from today, so a daily
// quota survives a restart but still resets at midnight.
func (t *Tracker) Restore(saved []Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	today := midnight(t.now()).Format(dayFormat)
	for _, u := range saved {
		c := t.clientLocked(u.Client)
		c.usage.Requests += u.Requests
		c.usage.Rejected += u.Rejected
		c.usage.PromptTokens += u.PromptTokens
		c.usage.CompletionTokens += u.CompletionTokens
		c.usage.EstimatedTokens += u.EstimatedTokens
		if u.Day == today {
			c.usage.TokensToday += u.TokensToday
		}
	}
}
