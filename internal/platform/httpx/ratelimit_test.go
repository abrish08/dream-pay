package httpx

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLimiter(10, 5)
	l.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if ok, _, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d refused within burst", i)
		}
	}
	ok, _, retry := l.Allow("k")
	if ok || retry <= 0 || retry > 100*time.Millisecond {
		t.Fatalf("6th request: ok=%v retry=%v", ok, retry)
	}
	if ok, _, _ := l.Allow("other"); !ok {
		t.Fatal("keys must be independent")
	}
	now = now.Add(100 * time.Millisecond) // 1 token back at 10/s
	if ok, _, _ := l.Allow("k"); !ok {
		t.Fatal("token not refilled")
	}
}

// Many goroutines hammer one key: exactly Burst requests get through.
func TestLimiterConcurrent(t *testing.T) {
	l := NewLimiter(0.001, 50)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if ok, _, _ := l.Allow("same"); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 50 {
		t.Fatalf("allowed %d, want exactly 50", allowed.Load())
	}
	if NewLimiter(0, 0) != nil {
		t.Fatal("rate 0 must disable limiting")
	}
}
