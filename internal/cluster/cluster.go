// Package cluster keeps several routers behind one load balancer in step.
// Each router periodically pushes to its peers what it learned since the
// last push: which backend now holds each conversation prefix, and each
// client's tokens used today, requests in flight and recent demand. A
// follow-up turn that lands on a different router still goes to the
// backend with its cache, and quotas and limits count the whole fleet.
//
// The state is a routing hint and an eventually consistent counter, so a
// lost push costs at most a cache miss or a few seconds of quota slack.
package cluster

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/limits"
)

// SyncPath is where routers receive each other's updates.
const SyncPath = "/cluster/sync"

// maxPending bounds the affinity records buffered between pushes.
const maxPending = 200_000

// Message is one push from a router to a peer.
type Message struct {
	From     string                  `json:"from"`
	Affinity []Record                `json:"affinity,omitempty"`
	Usage    map[string]limits.Daily `json:"usage"`
}

// Record maps a prompt-prefix hash to the backend that holds it.
type Record struct {
	Key     uint64 `json:"k"`
	Backend string `json:"b"`
}

// AffinityTable is the part of the prefix table the cluster needs.
type AffinityTable interface {
	Record(keys []uint64, backend string)
}

// Node is this router's membership in a cluster.
type Node struct {
	name     string
	peers    []string
	secret   string
	client   *http.Client
	affinity AffinityTable
	usage    *limits.Tracker
	log      *slog.Logger

	mu      sync.Mutex
	pending map[uint64]string
}

func New(name string, peers []string, secret string, client *http.Client, affinity AffinityTable, usage *limits.Tracker, log *slog.Logger) *Node {
	return &Node{
		name: name, peers: peers, secret: secret, client: client,
		affinity: affinity, usage: usage, log: log, pending: map[uint64]string{},
	}
}

// Note queues locally learned affinity for the next push.
func (n *Node) Note(keys []uint64, backend string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, k := range keys {
		if len(n.pending) >= maxPending {
			return // a hint, not a ledger: drop rather than grow without bound
		}
		n.pending[k] = backend
	}
}

// Run pushes to every peer each interval until ctx is done.
func (n *Node) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.SyncNow(ctx)
		}
	}
}

// SyncNow pushes pending updates to all peers in parallel and waits.
func (n *Node) SyncNow(ctx context.Context) {
	n.mu.Lock()
	recs := make([]Record, 0, len(n.pending))
	for k, b := range n.pending {
		recs = append(recs, Record{k, b})
	}
	n.pending = map[uint64]string{}
	n.mu.Unlock()

	body, err := json.Marshal(Message{From: n.name, Affinity: recs, Usage: n.usage.LocalDaily()})
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, peer := range n.peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.push(ctx, peer, body); err != nil {
				n.log.Warn("cluster sync failed", "peer", peer, "error", err)
			}
		}()
	}
	wg.Wait()
}

func (n *Node) push(ctx context.Context, peer string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(peer, "/")+SyncPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode == http.StatusConflict {
		return nil // the peer is this router itself
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("peer answered %s", resp.Status)
	}
	return nil
}

// ServeHTTP receives a peer's push.
func (n *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(n.secret)) != 1 {
		http.Error(w, "bad cluster secret", http.StatusUnauthorized)
		return
	}
	var m Message
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&m); err != nil {
		http.Error(w, "bad message: "+err.Error(), http.StatusBadRequest)
		return
	}
	if m.From == n.name {
		http.Error(w, "that is me", http.StatusConflict)
		return
	}
	// Group by backend so the table takes its lock once per backend.
	byBackend := map[string][]uint64{}
	for _, rec := range m.Affinity {
		byBackend[rec.Backend] = append(byBackend[rec.Backend], rec.Key)
	}
	for b, keys := range byBackend {
		n.affinity.Record(keys, b)
	}
	n.usage.SetRemote(m.From, m.Usage)
	w.WriteHeader(http.StatusNoContent)
}
