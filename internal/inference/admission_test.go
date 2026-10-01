package inference

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSharedLaneLimitsAndCancel(t *testing.T) {
	p := DefaultPolicy()
	p.QueueSize = 1
	p.QueueWait = time.Second
	r := New(p)
	first, err := r.Acquire(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		ticket, err := r.Acquire(ctx, []string{"a"})
		if ticket != nil {
			ticket.Finish("canceled")
		}
		done <- err
	}()
	awaitQueued(t, r, 1)
	if _, err := r.Acquire(context.Background(), []string{"a"}); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	second, err := r.Acquire(context.Background(), []string{"a", "b"})
	if err != nil || second.ID() != "b" {
		t.Fatalf("did not route to free replica: %v", err)
	}
	second.Finish("completed")
	cancel()
	<-done
	first.Finish("completed")
	first.Finish("completed")
	stats := r.Snapshot()[0]
	if stats.Active != 0 || stats.Queued != 0 || stats.Completed != 1 || stats.Canceled != 1 || stats.Rejected != 1 {
		t.Fatalf("leaked or double finished: %+v", stats)
	}
}
func awaitQueued(t *testing.T, r *Router, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if stats := r.Snapshot(); len(stats) > 0 && stats[0].Queued == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queue did not reach expected depth")
}
func TestQueueTimeoutAndConcurrentRelease(t *testing.T) {
	p := DefaultPolicy()
	p.QueueWait = 10 * time.Millisecond
	p.QueueSize = 128
	r := New(p)
	first, _ := r.Acquire(context.Background(), []string{"a"})
	if _, err := r.Acquire(context.Background(), []string{"a"}); !errors.Is(err, ErrQueueTimeout) {
		t.Fatal(err)
	}
	if r.Snapshot()[0].Active != 1 || r.Snapshot()[0].Queued != 0 {
		t.Fatal("timed out waiter retained")
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			timer := time.AfterFunc(time.Millisecond, cancel)
			defer timer.Stop()
			defer cancel()
			ticket, _ := r.Acquire(ctx, []string{"a"})
			if ticket != nil {
				ticket.Finish("completed")
			}
		}()
	}
	first.Finish("completed")
	wg.Wait()
	s := r.Snapshot()[0]
	if s.Active != 0 || s.Queued != 0 {
		t.Fatalf("cancellation/release leaked ownership: %+v", s)
	}
}
