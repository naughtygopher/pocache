package pocache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// blockingStore is a Store[K, T] whose Remove blocks on a gate channel
// before mutating the underlying map, letting tests hold a background
// worker "in progress" inside a store call while shutdown runs.
type blockingStore[K comparable, T any] struct {
	mu          sync.Mutex
	data        map[K]*Payload[T]
	gate        chan struct{}
	removeCalls atomic.Int32
}

func newBlockingStore[K comparable, T any]() *blockingStore[K, T] {
	return &blockingStore[K, T]{data: make(map[K]*Payload[T]), gate: make(chan struct{})}
}

func (s *blockingStore[K, T]) Add(key K, value *Payload[T]) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.data[key]
	s.data[key] = value
	return existed
}

func (s *blockingStore[K, T]) Get(key K) (*Payload[T], bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *blockingStore[K, T]) Remove(key K) bool {
	s.removeCalls.Add(1)
	<-s.gate
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.data[key]
	delete(s.data, key)
	return existed
}

// TestCloseIdempotentConcurrent verifies calling Close concurrently and
// repeatedly from multiple goroutines never panics or blocks forever.
func TestCloseIdempotentConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirer := require.New(t)
		cache, err := New(Config[string, string]{LRUCacheSize: 10, CacheAge: time.Minute})
		requirer.NoError(err)
		defer cache.Close()

		var wg sync.WaitGroup
		for range 20 {
			wg.Go(cache.Close)
		}
		wg.Wait()
		cache.Close() // also idempotent from the test goroutine itself
	})
}

// TestCloseThenGetAddBulkAdd verifies the post-Close contract by inspecting
// the underlying store directly: Get misses, Add/BulkAdd are no-ops, and the
// existing payload is left unmodified (store retained, not cleared).
func TestCloseThenGetAddBulkAdd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirer := require.New(t)
		store := newBlockingStore[string, string]()
		close(store.gate) // Remove never needs to block for this test

		cache, err := New(Config[string, string]{LRUCacheSize: 10, CacheAge: time.Minute, Store: store})
		requirer.NoError(err)
		defer cache.Close()

		cache.Add("k1", "v1")
		requirer.True(cache.Get("k1").Found)

		cache.Close()

		requirer.False(cache.Get("k1").Found, "Get must miss after Close")
		payload, found := store.Get("k1")
		requirer.True(found)
		requirer.Equal("v1", payload.Value(), "existing payload must be unmodified")

		requirer.False(cache.Add("k2", "v2"), "Add must be a no-op after Close")
		_, found = store.Get("k2")
		requirer.False(found, "Add after Close must not reach the store")

		results := cache.BulkAdd([]Tuple[string, string]{{Key: "k3", Value: "v3"}})
		requirer.Equal([]bool{false}, results)
		_, found = store.Get("k3")
		requirer.False(found, "BulkAdd after Close must not reach the store")
	})
}

// TestCloseJoinsWorkers exercises Close against every worker configuration:
// disabled cache, no updater, a single-key Updater, and a BulkUpdater. Close
// blocks on an internal WaitGroup, so returning from it inside the synctest
// bubble is itself the proof that every worker goroutine exited; no external
// goroutine-count check is needed (or reliable, given unrelated process
// goroutines).
func TestCloseJoinsWorkers(t *testing.T) {
	cases := map[string]Config[string, string]{
		"disabled":  {LRUCacheSize: 10, CacheAge: time.Minute, DisableCache: true},
		"noUpdater": {LRUCacheSize: 10, CacheAge: time.Minute},
		"singleUpdater": {
			LRUCacheSize: 10, CacheAge: time.Minute, Threshold: 59 * time.Second,
			Updater: func(ctx context.Context, key string) (string, error) { return key, nil },
		},
		"bulkUpdater": {
			LRUCacheSize: 10, CacheAge: time.Minute, Threshold: 59 * time.Second,
			BulkUpdater: func(ctx context.Context, keys []string) []UpdateResult[string] {
				results := make([]UpdateResult[string], len(keys))
				for i, k := range keys {
					results[i] = UpdateResult[string]{NewValue: k}
				}
				return results
			},
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requirer := require.New(t)
				cache, err := New(cfg)
				requirer.NoError(err)
				defer cache.Close()

				cache.Add("k1", "v1")
				cache.Get("k1")
				cache.Close() // must return: proves all workers joined
			})
		})
	}
}

// TestCloseCancelsActiveUpdater verifies that an in-flight Updater or
// BulkUpdater call observes context cancellation once Close runs.
func TestCloseCancelsActiveUpdater(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"single": func(t *testing.T) {
			requirer := require.New(t)
			entered := make(chan struct{})
			var gotErr atomic.Value

			cache, err := New(Config[string, string]{
				LRUCacheSize: 10, CacheAge: 2 * time.Second, Threshold: time.Second,
				Updater: func(ctx context.Context, key string) (string, error) {
					close(entered)
					<-ctx.Done()
					gotErr.Store(ctx.Err())
					return "", ctx.Err()
				},
			})
			requirer.NoError(err)
			defer cache.Close()

			cache.Add("k1", "v1")
			time.Sleep(time.Second + time.Millisecond)
			synctest.Wait()
			cache.Get("k1") // enqueues the update
			synctest.Wait()
			<-entered // updater is active before Close runs

			cache.Close()
			errv, _ := gotErr.Load().(error)
			requirer.ErrorIs(errv, context.Canceled)
		},
		"bulk": func(t *testing.T) {
			requirer := require.New(t)
			entered := make(chan struct{})
			var gotErr atomic.Value

			cache, err := New(Config[string, string]{
				LRUCacheSize: 10, CacheAge: 2 * time.Second, Threshold: time.Second,
				BulkUpdater: func(ctx context.Context, keys []string) []UpdateResult[string] {
					close(entered)
					<-ctx.Done()
					gotErr.Store(ctx.Err())
					results := make([]UpdateResult[string], len(keys))
					for i := range keys {
						results[i] = UpdateResult[string]{Err: ctx.Err()}
					}
					return results
				},
			})
			requirer.NoError(err)
			defer cache.Close()

			cache.Add("k1", "v1")
			time.Sleep(time.Second + time.Millisecond)
			synctest.Wait()
			cache.Get("k1")
			time.Sleep(20 * time.Millisecond) // let the 15ms batch ticker fire
			synctest.Wait()
			<-entered // bulk updater is active before Close runs

			cache.Close()
			errv, _ := gotErr.Load().(error)
			requirer.ErrorIs(errv, context.Canceled)
		},
	}

	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, run)
		})
	}
}

// TestCloseAbandonsQueuedWork holds the update worker busy processing one
// key (blocked on a gate) while a second key waits behind it in the queue,
// then closes the cache. Per the lifecycle contract, the queued key must
// never reach the Updater/BulkUpdater. UpdaterTimeout is set generously so
// the blocked call is released by Close's cancellation, not by its own
// deadline expiring first.
func TestCloseAbandonsQueuedWork(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			gate := make(chan struct{})
			var mu sync.Mutex
			var invoked []string

			cache, err := New(Config[string, string]{
				LRUCacheSize: 10, QLength: 1, CacheAge: 2 * time.Second,
				Threshold: time.Second, UpdaterTimeout: time.Hour,
				Updater: func(ctx context.Context, key string) (string, error) {
					mu.Lock()
					invoked = append(invoked, key)
					mu.Unlock()
					select {
					case <-gate:
						return key + "_upd", nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				},
			})
			requirer.NoError(err)
			defer cache.Close()
			defer close(gate)

			cache.Add("k1", "v1")
			cache.Add("k2", "v2")
			time.Sleep(time.Second + time.Millisecond)
			synctest.Wait()

			cache.Get("k1") // dequeued immediately, updater blocks on gate
			synctest.Wait()
			cache.Get("k2") // buffered in updateQ (QLength=1), never dequeued
			synctest.Wait()

			cache.Close()

			mu.Lock()
			got := append([]string(nil), invoked...)
			mu.Unlock()
			requirer.Equal([]string{"k1"}, got, "k2 must be abandoned, not handed to the Updater")
		})
	})

	t.Run("bulk", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			gate := make(chan struct{})
			var mu sync.Mutex
			var batches [][]string

			cache, err := New(Config[string, string]{
				LRUCacheSize: 10, QLength: 2, CacheAge: 2 * time.Second,
				Threshold: time.Second, UpdaterTimeout: time.Hour,
				BulkUpdater: func(ctx context.Context, keys []string) []UpdateResult[string] {
					mu.Lock()
					batches = append(batches, append([]string(nil), keys...))
					mu.Unlock()

					results := make([]UpdateResult[string], len(keys))
					select {
					case <-gate:
						for i, k := range keys {
							results[i] = UpdateResult[string]{NewValue: k + "_upd"}
						}
					case <-ctx.Done():
						for i := range keys {
							results[i] = UpdateResult[string]{Err: ctx.Err()}
						}
					}
					return results
				},
			})
			requirer.NoError(err)
			defer cache.Close()
			defer close(gate)

			cache.Add("k1", "v1")
			time.Sleep(time.Second + time.Millisecond)
			synctest.Wait()
			cache.Get("k1")
			time.Sleep(20 * time.Millisecond) // batch ticker fires, k1's batch blocks on gate
			synctest.Wait()

			cache.Add("k2", "v2")
			time.Sleep(time.Second + time.Millisecond)
			synctest.Wait()
			cache.Get("k2") // queued behind the in-flight batch
			synctest.Wait()

			cache.Close()

			mu.Lock()
			got := append([][]string(nil), batches...)
			mu.Unlock()
			requirer.Len(got, 1, "only the first batch should have been handed to BulkUpdater")
			requirer.Equal([]string{"k1"}, got[0])
		})
	})
}

// TestCloseReleasesBlockedUpdateQueueSender fills updateQ to capacity while
// the update worker is busy, so a subsequent enqueue attempt blocks trying
// to send. Close must cancel the context so the blocked sender (inside
// enqueueUpdate's select) is released, while the truly in-progress call
// (which only unblocks via gate, not ctx) makes Close wait for it to finish.
func TestCloseReleasesBlockedUpdateQueueSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirer := require.New(t)
		gate := make(chan struct{})

		cache, err := New(Config[string, string]{
			LRUCacheSize: 10, QLength: 1, CacheAge: 2 * time.Second,
			Threshold: time.Second, UpdaterTimeout: time.Hour,
			Updater: func(ctx context.Context, key string) (string, error) {
				<-gate // deliberately ignores ctx: simulates a call in progress
				return key + "_upd", nil
			},
		})
		requirer.NoError(err)
		defer cache.Close()
		release := sync.OnceFunc(func() { close(gate) })
		defer release()

		cache.Add("k1", "v1")
		cache.Add("k2", "v2")
		cache.Add("k3", "v3")
		time.Sleep(time.Second + time.Millisecond)
		synctest.Wait()

		cache.Get("k1") // dequeued, updater now blocked on gate
		synctest.Wait()
		cache.Get("k2") // buffered (QLength=1)
		synctest.Wait()

		blockedDone := make(chan struct{})
		go func() {
			cache.Get("k3") // updateQ full, worker busy: enqueue must block
			close(blockedDone)
		}()
		synctest.Wait()

		select {
		case <-blockedDone:
			requirer.Fail("Get(k3) should still be blocked before Close")
		default:
		}

		closeDone := make(chan struct{})
		go func() {
			cache.Close()
			close(closeDone)
		}()
		synctest.Wait()

		select {
		case <-blockedDone:
		default:
			requirer.Fail("blocked enqueue must be released once Close cancels the context")
		}
		select {
		case <-closeDone:
			requirer.Fail("Close should still be waiting on the in-progress updater")
		default:
		}

		release()
		synctest.Wait()

		select {
		case <-closeDone:
		default:
			requirer.Fail("Close should complete once the in-progress updater returns")
		}
	})
}

// TestCloseReleasesBlockedDeleteQueueSender mirrors the update-queue test
// above for deleteQ, using blockingStore to hold Remove "in progress". It
// also verifies the store is retained (not cleared): the in-progress
// removal finishes, but the queued one is abandoned.
func TestCloseReleasesBlockedDeleteQueueSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirer := require.New(t)
		store := newBlockingStore[string, string]()

		cache, err := New(Config[string, string]{LRUCacheSize: 10, QLength: 1, CacheAge: time.Millisecond, Store: store})
		requirer.NoError(err)
		defer cache.Close()
		release := sync.OnceFunc(func() { close(store.gate) })
		defer release()

		cache.Add("k1", "v1")
		cache.Add("k2", "v2")
		cache.Add("k3", "v3")
		time.Sleep(2 * time.Millisecond)
		synctest.Wait()

		requirer.False(cache.Get("k1").Found) // expired: enqueues delete, listener blocks in Remove
		synctest.Wait()
		requirer.False(cache.Get("k2").Found) // buffered (QLength=1)
		synctest.Wait()

		blockedDone := make(chan struct{})
		go func() {
			cache.Get("k3") // deleteQ full, listener busy: send must block
			close(blockedDone)
		}()
		synctest.Wait()

		select {
		case <-blockedDone:
			requirer.Fail("Get(k3) should still be blocked before Close")
		default:
		}

		closeDone := make(chan struct{})
		go func() {
			cache.Close()
			close(closeDone)
		}()
		synctest.Wait()

		select {
		case <-blockedDone:
		default:
			requirer.Fail("blocked delete-queue sender must be released once Close cancels the context")
		}
		select {
		case <-closeDone:
			requirer.Fail("Close should still be waiting on the in-progress Remove call")
		default:
		}

		release()
		synctest.Wait()

		select {
		case <-closeDone:
		default:
			requirer.Fail("Close should complete once the in-progress Remove returns")
		}

		requirer.EqualValues(1, store.removeCalls.Load(), "k2's queued removal must be abandoned")
		_, foundK1 := store.Get("k1")
		requirer.False(foundK1, "the in-progress Remove(k1) must be allowed to finish")
		_, foundK2 := store.Get("k2")
		requirer.True(foundK2, "queued deletion for k2 must be abandoned; store retained as-is")
		_, foundK3 := store.Get("k3")
		requirer.True(foundK3, "k3 was never dequeued so it must remain untouched")
	})
}

// TestConcurrentGetAddCloseRace hammers Get/Add/BulkAdd from many goroutines
// concurrently with Close. Run with -race to catch data races; a panic in
// any goroutine fails the test naturally without a recover wrapper.
func TestConcurrentGetAddCloseRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirer := require.New(t)
		cache, err := New(Config[string, string]{LRUCacheSize: 100, CacheAge: time.Minute})
		requirer.NoError(err)
		defer cache.Close()

		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() { cache.Add("key", "v") })
			wg.Go(func() { cache.Get("key") })
			wg.Go(func() {
				cache.BulkAdd([]Tuple[string, string]{{Key: "key", Value: "v"}})
			})
		}
		wg.Go(cache.Close)
		wg.Wait()
	})
}

// TestDebounceLoadOrStoreConcurrentReaders verifies that many goroutines
// concurrently triggering an update-eligible Get for the same key result in
// the Updater running at most once, via updateInProgress's LoadOrStore.
func TestDebounceLoadOrStoreConcurrentReaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirer := require.New(t)
		var updateCalls atomic.Int32
		release := make(chan struct{})

		cache, err := New(Config[string, string]{
			LRUCacheSize: 10, QLength: 100, CacheAge: 2 * time.Second, Threshold: time.Second,
			Updater: func(ctx context.Context, key string) (string, error) {
				updateCalls.Add(1)
				<-release
				return key, nil
			},
		})
		requirer.NoError(err)
		defer cache.Close()

		cache.Add("k1", "v1")
		time.Sleep(time.Second + time.Millisecond)
		synctest.Wait()

		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() { cache.Get("k1") })
		}
		wg.Wait()
		synctest.Wait()

		close(release)
		synctest.Wait()

		requirer.EqualValues(1, updateCalls.Load(), "Updater must run at most once for a debounced key")
	})
}
