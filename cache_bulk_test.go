package pocache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// expireSoon adds the keys, and advances the fake clock so that they are within the threshold
func expireSoon(cache *Cache[string, string], keys ...string) {
	for _, key := range keys {
		cache.Add(key, key)
	}
	time.Sleep(time.Second + time.Millisecond)
	synctest.Wait()
}

func TestBulkUpdaterPartialResult(tt *testing.T) {
	synctest.Test(tt, func(t *testing.T) {
		requirer := require.New(t)

		cache, err := New(Config[string, string]{
			CacheAge: 2 * time.Second, Threshold: time.Second, ServeStale: true,
			BulkUpdater: func(ctx context.Context, keys []string) (map[string]string, error) {
				// k2 is not returned, and k9 was never requested
				return map[string]string{"k1": "k1_upd", "k3": "k3_upd", "k9": "k9_upd"}, nil
			},
		})
		requirer.NoError(err)
		defer cache.Close()

		expireSoon(cache, "k1", "k2", "k3")
		for _, key := range []string{"k1", "k2", "k3"} {
			cache.Get(key)
		}
		time.Sleep(15 * time.Millisecond)
		synctest.Wait()

		// checked before reading k2 again, as a read within the threshold queues it again
		_, inprogress := cache.updateInProgress.Load("k2")
		requirer.False(inprogress, "a key missing from the result must be released to retry")

		requirer.Equal("k1_upd", cache.Get("k1").V)
		requirer.Equal("k2", cache.Get("k2").V, "a key missing from the result keeps its value")
		requirer.Equal("k3_upd", cache.Get("k3").V)
		requirer.False(cache.Get("k9").Found, "unrequested keys are not added")
	})
}

func TestBulkUpdaterError(tt *testing.T) {
	synctest.Test(tt, func(t *testing.T) {
		requirer := require.New(t)
		errForced := errors.New("forced")
		var captured errCapture

		cache, err := New(Config[string, string]{
			CacheAge: 2 * time.Second, Threshold: time.Second, ServeStale: true,
			BulkUpdater: func(ctx context.Context, keys []string) (map[string]string, error) {
				return map[string]string{"k1": "k1_upd"}, errForced
			},
			ErrWatcher: captured.set,
		})
		requirer.NoError(err)
		defer cache.Close()

		expireSoon(cache, "k1", "k2")
		cache.Get("k1")
		cache.Get("k2")
		time.Sleep(15 * time.Millisecond)
		synctest.Wait()

		requirer.ErrorIs(captured.get(), ErrBulkUpdate)
		requirer.ErrorIs(captured.get(), errForced)
		requirer.Equal("k1_upd", cache.Get("k1").V, "values returned with an error are stored")
		requirer.Equal("k2", cache.Get("k2").V)
	})
}

func TestUpdaterPanicReleasesKeys(tt *testing.T) {
	cases := map[string]func(*Config[string, string]){
		"single": func(cfg *Config[string, string]) {
			cfg.Updater = func(ctx context.Context, key string) (string, error) { panic("single boom") }
		},
		"bulk": func(cfg *Config[string, string]) {
			cfg.BulkUpdater = func(ctx context.Context, keys []string) (map[string]string, error) {
				panic(errors.New("bulk boom"))
			}
		},
	}

	for name, setup := range cases {
		tt.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requirer := require.New(t)
				var captured errCapture

				cfg := Config[string, string]{
					CacheAge: 2 * time.Second, Threshold: time.Second, ServeStale: true,
					ErrWatcher: captured.set,
				}
				setup(&cfg)
				cache, err := New(cfg)
				requirer.NoError(err)
				defer cache.Close()

				expireSoon(cache, "k1", "k2")
				cache.Get("k1")
				cache.Get("k2")
				time.Sleep(15 * time.Millisecond)
				synctest.Wait()

				requirer.ErrorIs(captured.get(), ErrPanic)
				requirer.ErrorContains(captured.get(), "boom")
				for _, key := range []string{"k1", "k2"} {
					_, inprogress := cache.updateInProgress.Load(key)
					requirer.False(inprogress, "%s must be released after a panic", key)
				}

				// the worker survives the panic, and keeps processing
				cache.Get("k1")
				time.Sleep(15 * time.Millisecond)
				synctest.Wait()
				requirer.ErrorIs(captured.get(), ErrPanic)
			})
		})
	}
}

func TestBulkSizeAndMaxWait(tt *testing.T) {
	synctest.Test(tt, func(t *testing.T) {
		requirer := require.New(t)
		var (
			mu      sync.Mutex
			batches [][]string
		)

		cache, err := New(Config[string, string]{
			CacheAge: 2 * time.Second, Threshold: time.Second, ServeStale: true,
			BulkSize: 3, BulkMaxWait: 100 * time.Millisecond,
			BulkUpdater: func(ctx context.Context, keys []string) (map[string]string, error) {
				mu.Lock()
				batches = append(batches, append([]string(nil), keys...))
				mu.Unlock()
				return nil, nil
			},
		})
		requirer.NoError(err)
		defer cache.Close()

		keys := make([]string, 7)
		for i := range keys {
			keys[i] = fmt.Sprintf("k%d", i)
		}
		expireSoon(cache, keys...)
		for _, key := range keys {
			cache.Get(key)
		}

		// full batches are sent immediately, without waiting for BulkMaxWait
		synctest.Wait()
		mu.Lock()
		requirer.Equal([][]string{{"k0", "k1", "k2"}, {"k3", "k4", "k5"}}, batches)
		mu.Unlock()

		// the partial batch is sent once its first key waited for BulkMaxWait
		time.Sleep(99 * time.Millisecond)
		synctest.Wait()
		mu.Lock()
		requirer.Len(batches, 2)
		mu.Unlock()

		time.Sleep(time.Millisecond)
		synctest.Wait()
		mu.Lock()
		requirer.Len(batches, 3)
		requirer.Equal([]string{"k6"}, batches[2])
		mu.Unlock()
	})
}

func TestSingleUpdaterProcessesOneKeyAtATime(tt *testing.T) {
	synctest.Test(tt, func(t *testing.T) {
		requirer := require.New(t)
		var (
			mu       sync.Mutex
			inflight int
			maxSeen  int
			calls    int
		)

		cache, err := New(Config[string, string]{
			CacheAge: 2 * time.Second, Threshold: time.Second, ServeStale: true,
			Updater: func(ctx context.Context, key string) (string, error) {
				mu.Lock()
				inflight++
				calls++
				maxSeen = max(maxSeen, inflight)
				mu.Unlock()

				time.Sleep(10 * time.Millisecond)

				mu.Lock()
				inflight--
				mu.Unlock()
				return key + "_upd", nil
			},
		})
		requirer.NoError(err)
		defer cache.Close()

		keys := []string{"k1", "k2", "k3"}
		expireSoon(cache, keys...)
		for _, key := range keys {
			cache.Get(key)
		}
		time.Sleep(30 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		requirer.Equal(3, calls)
		requirer.Equal(1, maxSeen)
		mu.Unlock()
		for _, key := range keys {
			requirer.Equal(key+"_upd", cache.Get(key).V)
		}
	})
}
