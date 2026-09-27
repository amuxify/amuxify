package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// With one job the items run inline, in order, and the context is checked
// before each one: a cancellation inside an item stops the next from
// starting.
func TestRunSequentialInline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var order []int
	ran := Run(ctx, 1, 5, nil, func(i int) {
		order = append(order, i)
		if i == 2 {
			cancel()
		}
	})
	if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
		t.Errorf("order %v", order)
	}
	for i, ok := range ran {
		if ok != (i <= 2) {
			t.Errorf("ran[%d] = %v", i, ok)
		}
	}
	for _, jobs := range []int{0, -3} {
		n := 0
		Run(context.Background(), jobs, 4, nil, func(int) { n++ })
		if n != 4 {
			t.Errorf("jobs=%d ran %d items", jobs, n)
		}
	}
	if got := Run(context.Background(), 8, 0, nil, func(int) { t.Error("fn called for n=0") }); len(got) != 0 {
		t.Errorf("n=0 returned %v", got)
	}
}

// Items that share a key never overlap and run in index order, while items
// with different keys do overlap, so the rule serialises only what it must.
func TestRunKeysSerialise(t *testing.T) {
	const n = 64
	keys := make([][]string, n)
	for i := range keys {
		if i%2 == 0 {
			keys[i] = []string{"shared"}
		}
	}
	var (
		mu        sync.Mutex
		sharedRun int32
		maxShared int32
		running   int32
		maxAll    int32
		sharedSeq []int
	)
	ran := Run(context.Background(), 8, n, keys, func(i int) {
		all := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxAll)
			if all <= m || atomic.CompareAndSwapInt32(&maxAll, m, all) {
				break
			}
		}
		if i%2 == 0 {
			s := atomic.AddInt32(&sharedRun, 1)
			for {
				m := atomic.LoadInt32(&maxShared)
				if s <= m || atomic.CompareAndSwapInt32(&maxShared, m, s) {
					break
				}
			}
			mu.Lock()
			sharedSeq = append(sharedSeq, i)
			mu.Unlock()
		}
		time.Sleep(2 * time.Millisecond)
		if i%2 == 0 {
			atomic.AddInt32(&sharedRun, -1)
		}
		atomic.AddInt32(&running, -1)
	})
	for i, ok := range ran {
		if !ok {
			t.Errorf("item %d did not run", i)
		}
	}
	if maxShared != 1 {
		t.Errorf("%d items with the shared key ran at once", maxShared)
	}
	if maxAll < 2 {
		t.Errorf("no two items ever ran at once; the pool is not parallel")
	}
	for k := 1; k < len(sharedSeq); k++ {
		if sharedSeq[k] < sharedSeq[k-1] {
			t.Errorf("shared key ran out of order: %v", sharedSeq)
			break
		}
	}
}

// Overlapping keys cannot deadlock: item i carries the keys of i and i+1,
// so every item is chained to its neighbours, and the run still completes
// with every item run exactly once.
func TestRunChainedKeysComplete(t *testing.T) {
	const n = 40
	keys := make([][]string, n)
	for i := range keys {
		keys[i] = []string{key(i), key(i + 1)}
	}
	var count int32
	done := make(chan []bool, 1)
	go func() {
		done <- Run(context.Background(), 6, n, keys, func(int) { atomic.AddInt32(&count, 1) })
	}()
	select {
	case ran := <-done:
		for i, ok := range ran {
			if !ok {
				t.Errorf("item %d did not run", i)
			}
		}
		if count != n {
			t.Errorf("fn ran %d times", count)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func key(i int) string { return string(rune('a'+i%26)) + string(rune('a'+i/26)) }

// A cancelled run starts nothing more, waits for the items it started, and
// reports exactly those as run. Nothing runs after Run returns.
func TestRunCancelStartsNothingNew(t *testing.T) {
	const n = 32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu       sync.Mutex
		finished = map[int]bool{}
		started  int32
		returned int32
	)
	ran := Run(ctx, 4, n, nil, func(i int) {
		if atomic.LoadInt32(&returned) != 0 {
			t.Errorf("item %d ran after Run returned", i)
		}
		if atomic.AddInt32(&started, 1) == 4 {
			cancel()
		}
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		finished[i] = true
		mu.Unlock()
	})
	atomic.StoreInt32(&returned, 1)
	mu.Lock()
	defer mu.Unlock()
	count := 0
	for i, ok := range ran {
		if ok != finished[i] {
			t.Errorf("ran[%d] = %v but finished = %v", i, ok, finished[i])
		}
		if ok {
			count++
		}
	}
	if count == n {
		t.Error("every item ran although the run was cancelled")
	}
	if count < 4 {
		t.Errorf("%d items ran; the four that had started must all finish", count)
	}
	// A context that is already done starts nothing at all.
	got := Run(ctx, 4, 8, nil, func(i int) { t.Errorf("item %d ran under a done context", i) })
	for i, ok := range got {
		if ok {
			t.Errorf("ran[%d] under a done context", i)
		}
	}
}

// Claims hands one key to exactly one of many concurrent claimants, and a
// released key can be claimed again.
func TestClaimsExactlyOneWinner(t *testing.T) {
	var c Claims
	const workers = 32
	for round := 0; round < 20; round++ {
		var wins int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if c.Claim("dest") {
					atomic.AddInt32(&wins, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d winners", round, wins)
		}
		if !c.Held("dest") || c.Held("other") {
			t.Fatal("Held disagrees with Claim")
		}
		c.Release("dest")
		if c.Held("dest") {
			t.Fatal("released key still held")
		}
	}
	c.Release("never-claimed")
	if !c.Claim("a") || c.Claim("a") || !c.Claim("A") {
		t.Error("keys are not exact strings")
	}
}
