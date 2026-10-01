// Package inference owns request admission and progress deadlines shared by
// pooled and deployment-specific inference endpoints.
package inference

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrCapacity = errors.New("inference queue is full")
var ErrQueueTimeout = errors.New("inference queue wait exceeded")

type Policy struct {
	Concurrency int           `json:"concurrency"`
	QueueSize   int           `json:"queue_size"`
	QueueWait   time.Duration `json:"-"`
	FirstToken  time.Duration `json:"-"`
	Idle        time.Duration `json:"-"`
	Total       time.Duration `json:"-"`
}

func DefaultPolicy() Policy {
	return Policy{1, 8, 3 * time.Second, 15 * time.Second, 10 * time.Second, 120 * time.Second}
}

type Stats struct {
	DeploymentID    string    `json:"deployment_id"`
	Active          int       `json:"active"`
	Queued          int       `json:"queued"`
	Requests        uint64    `json:"requests"`
	Completed       uint64    `json:"completed"`
	Failed          uint64    `json:"failed"`
	Canceled        uint64    `json:"canceled"`
	Rejected        uint64    `json:"rejected"`
	QueueTimeouts   uint64    `json:"queue_timeouts"`
	FirstTimeouts   uint64    `json:"first_token_timeouts"`
	IdleTimeouts    uint64    `json:"idle_timeouts"`
	TotalTimeouts   uint64    `json:"total_timeouts"`
	TTFTCount       uint64    `json:"ttft_count"`
	TTFTSum         float64   `json:"ttft_seconds_sum"`
	TTFTBuckets     [7]uint64 `json:"ttft_buckets"`
	TTFTEWMA        float64   `json:"ttft_ewma_seconds"`
	LastOutcome     string    `json:"last_outcome,omitempty"`
	LastCompletedAt time.Time `json:"last_completed_at,omitempty"`
}

var TTFTBounds = [...]float64{.1, .5, 1, 2, 5, 10, 15}

type waiter struct {
	ready   chan struct{}
	started bool
}
type lane struct {
	Stats
	waiting []*waiter
}
type Router struct {
	mu     sync.Mutex
	lanes  map[string]*lane
	Policy Policy
}

func New(p Policy) *Router                    { return &Router{lanes: map[string]*lane{}, Policy: p} }
func (r *Router) ObserveDeployment(id string) { r.mu.Lock(); defer r.mu.Unlock(); r.lane(id) }
func (r *Router) lane(id string) *lane {
	l := r.lanes[id]
	if l == nil {
		l = &lane{Stats: Stats{DeploymentID: id}}
		r.lanes[id] = l
	}
	return l
}

// One lane per deployment prevents bypassing limits through multiple aliases.
// Selection is least outstanding requests, then observed first-token latency.
// The selected deployment remains pinned, including while it is queued.
func (r *Router) Acquire(ctx context.Context, candidates []string) (*Ticket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	var selected *lane
	for _, id := range candidates {
		l := r.lane(id)
		if l.Active >= r.Policy.Concurrency && len(l.waiting) >= r.Policy.QueueSize {
			continue
		}
		load := l.Active + len(l.waiting)
		if selected == nil || load < selected.Active+len(selected.waiting) || load == selected.Active+len(selected.waiting) && (l.TTFTEWMA < selected.TTFTEWMA || l.TTFTEWMA == selected.TTFTEWMA && id < selected.DeploymentID) {
			selected = l
		}
	}
	if selected == nil {
		// Attribute a full-pool rejection once, not once per replica.
		if len(candidates) > 0 {
			r.lane(candidates[0]).Rejected++
		}
		r.mu.Unlock()
		return nil, ErrCapacity
	}
	l := selected
	l.Requests++
	t := &Ticket{router: r, id: l.DeploymentID, arrived: time.Now()}
	if l.Active < r.Policy.Concurrency {
		l.Active++
		r.mu.Unlock()
		return t, nil
	}
	w := &waiter{ready: make(chan struct{})}
	l.waiting = append(l.waiting, w)
	r.mu.Unlock()
	timer := time.NewTimer(r.Policy.QueueWait)
	defer timer.Stop()
	var err error
	select {
	case <-w.ready:
		return t, nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = ErrQueueTimeout
	}
	r.mu.Lock()
	if w.started {
		r.releaseLocked(l)
	} else {
		for i, candidate := range l.waiting {
			if candidate == w {
				l.waiting = append(l.waiting[:i], l.waiting[i+1:]...)
				break
			}
		}
	}
	if errors.Is(err, ErrQueueTimeout) {
		l.QueueTimeouts++
	} else {
		l.Canceled++
	}
	l.LastOutcome = "queue_canceled"
	if errors.Is(err, ErrQueueTimeout) {
		l.LastOutcome = "queue_timeout"
	}
	l.LastCompletedAt = time.Now().UTC()
	r.mu.Unlock()
	return nil, err
}

func (r *Router) releaseLocked(l *lane) {
	l.Active--
	if len(l.waiting) > 0 {
		w := l.waiting[0]
		l.waiting = l.waiting[1:]
		l.Active++
		w.started = true
		close(w.ready)
	}
}

type Ticket struct {
	router  *Router
	id      string
	arrived time.Time
	once    sync.Once
	first   sync.Once
}

func (t *Ticket) ID() string { return t.id }

// TTFT includes queue time and starts on the first meaningful content/tool delta.
func (t *Ticket) FirstToken() {
	t.first.Do(func() {
		r := t.router
		r.mu.Lock()
		defer r.mu.Unlock()
		l := r.lane(t.id)
		elapsed := time.Since(t.arrived).Seconds()
		l.TTFTCount++
		l.TTFTSum += elapsed
		for i, b := range TTFTBounds {
			if elapsed <= b {
				l.TTFTBuckets[i]++
			}
		}
		if l.TTFTCount == 1 {
			l.TTFTEWMA = elapsed
		} else {
			l.TTFTEWMA = .2*elapsed + .8*l.TTFTEWMA
		}
	})
}
func (t *Ticket) Finish(outcome string) {
	t.once.Do(func() {
		r := t.router
		r.mu.Lock()
		defer r.mu.Unlock()
		l := r.lane(t.id)
		switch outcome {
		case "completed":
			l.Completed++
		case "canceled":
			l.Canceled++
		default:
			l.Failed++
		}
		switch outcome {
		case "first_token_timeout":
			l.FirstTimeouts++
		case "idle_timeout":
			l.IdleTimeouts++
		case "total_timeout":
			l.TotalTimeouts++
		}
		l.LastOutcome = outcome
		l.LastCompletedAt = time.Now().UTC()
		r.releaseLocked(l)
	})
}
func (r *Router) Snapshot() []Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Stats, 0, len(r.lanes))
	for _, l := range r.lanes {
		s := l.Stats
		s.Queued = len(l.waiting)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out
}
