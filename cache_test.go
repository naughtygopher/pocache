package pocache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errCapture safely hands an error captured inside a background goroutine
// (e.g. an ErrWatcher callback) back to the test goroutine. Using
// require/assert directly from a non-test goroutine can panic, so
// background code must stash the error here and the test goroutine reads
// it back after a synctest.Wait().
type errCapture struct {
	mu  sync.Mutex
	err error
}

func (c *errCapture) set(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *errCapture) get() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func TestCache(tt *testing.T) {
	const (
		prefix = "prefix"
		value  = "value"
	)

	tt.Run("found", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				DisableCache: false,
			})
			requirer.NoError(err)
			defer cache.Close()

			cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			v := cache.Get(prefix)
			asserter.True(v.Found)
			asserter.Equal(v.V, value)
		})
	})

	tt.Run("not found", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				DisableCache: false,
			})
			requirer.NoError(err)
			defer cache.Close()

			cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			v := cache.Get(prefix + "_does_not_exist")
			asserter.False(v.Found)
			asserter.Equal(v.V, nil)
		})
	})

	tt.Run("cache age expired", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 1,
				CacheAge:     time.Nanosecond,
				DisableCache: false,
			})
			requirer.NoError(err)
			defer cache.Close()

			cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			time.Sleep(time.Millisecond)
			synctest.Wait()
			v := cache.Get(prefix)
			asserter.False(v.Found)
			asserter.Equal(v.V, nil)
		})
	})

	tt.Run("update cache", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				DisableCache: false,
			})
			requirer.NoError(err)
			defer cache.Close()

			cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			v := cache.Get(prefix)
			asserter.True(v.Found)
			asserter.Equal(v.V, value)

			newValue := "new_value"
			cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: newValue}})
			v = cache.Get(prefix)
			asserter.True(v.Found)
			asserter.Equal(v.V, newValue)
		})
	})

	tt.Run("multiple Add/Get to check if channel blocks", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			// limit should be greater than the channel buffer for updateQ & deleteQ
			limit := 200
			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				DisableCache: false,
			})
			requirer.NoError(err)
			defer cache.Close()

			for i := range limit {
				key := fmt.Sprintf("%s_%d", prefix, i)
				val := fmt.Sprintf("%s_%d", value, i)
				cache.BulkAdd([]Tuple[string, any]{{Key: key, Value: val}})
			}

			for i := range limit {
				key := fmt.Sprintf("%s_%d", prefix, i)
				val := fmt.Sprintf("%s_%d", value, i)
				v := cache.Get(key)
				asserter.True(v.Found)
				asserter.Equal(v.V, val)
			}
		})
	})

	tt.Run("serve stale", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Second * 2,
				DisableCache: false,
				ServeStale:   true,
			})
			requirer.NoError(err)
			defer cache.Close()

			cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			// wait for cache to expire
			time.Sleep(time.Second * 3)
			synctest.Wait()

			v := cache.Get(prefix)
			asserter.True(v.Found)
			asserter.Equal(v.V, value)
		})
	})

	tt.Run("debounce", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				Threshold:    time.Second * 59,
				DisableCache: false,
				Updater: func(ctx context.Context, key string) (any, error) {
					// intentional delay in updater to retain debounce key
					// in the map long enough to be tested
					time.Sleep(time.Second * 3)
					return key, nil
				},
			})
			requirer.NoError(err)
			defer cache.Close()

			_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			// wait for threshold window
			time.Sleep(time.Second)
			synctest.Wait()
			// trigger auto update within threshold window
			_ = cache.Get(prefix)

			// re-trigger auto update within threshold window
			_ = cache.Get(prefix)

			// wait for the update goroutine to pick up the key and enter its
			// (still sleeping) updater, at which point the debounce entry
			// must still be present
			synctest.Wait()
			_, found := cache.updateInProgress.Load(prefix)
			asserter.True(found)
		})
	})

	tt.Run("disabled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				Threshold:    time.Second * 59,
				DisableCache: true,
				Updater: func(ctx context.Context, key string) (any, error) {
					return key, nil
				},
			})
			requirer.NoError(err)
			defer cache.Close()

			_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			// wait for threshold window
			time.Sleep(time.Second * 2)
			synctest.Wait()

			// trigger auto update within threshold window
			_ = cache.Get(prefix)

			// wait for updater to be executed
			time.Sleep(time.Second * 1)
			synctest.Wait()
			v := cache.Get(prefix)
			asserter.False(v.Found)
		})
	})

	tt.Run("no updater", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				Threshold:    time.Second * 59,
				DisableCache: false,
				Updater:      nil,
			})
			requirer.NoError(err)
			defer cache.Close()

			_ = cache.Add(prefix, value)
			// wait for threshold window
			time.Sleep(time.Second * 2)
			synctest.Wait()
			// trigger auto update within threshold window
			_ = cache.Get(prefix)
			// wait for updater to run
			time.Sleep(time.Second * 2)
			synctest.Wait()

			v := cache.Get(prefix)
			asserter.EqualValues(value, v.V)
		})
	})
}

func TestThresholdBulkUpdater(tt *testing.T) {
	synctest.Test(tt, func(t *testing.T) {
		var (
			requirer           = require.New(t)
			asserter           = require.New(t)
			cacheAge           = 2 * time.Second
			threshold          = time.Second
			bulkUpdaterLatency = 100 * time.Millisecond
		)

		ranUpdater := atomic.Int64{}

		ch, err := New(Config[string, string]{
			CacheAge:   cacheAge,
			Threshold:  threshold,
			ServeStale: true,
			BulkUpdater: func(ctx context.Context, keys []string) []UpdateResult[string] {
				// delay 100 millisecond
				time.Sleep(bulkUpdaterLatency)
				ranUpdater.Add(int64(len(keys)))
				result := make([]UpdateResult[string], len(keys))
				for idx, key := range keys {
					result[idx] = UpdateResult[string]{
						NewValue: key + "_updated",
					}
				}
				return result
			},
		})
		requirer.NoError(err)
		defer ch.Close()

		// make the cache full of keys
		keys := make([]string, 0, 1000)
		for i := range 1000 {
			key := fmt.Sprintf("key_%d", i)
			keys = append(keys, key)
			ch.Add(key, key)
		}

		// advance the fake clock to the time all of the keys need cache refreshing
		time.Sleep(cacheAge - threshold + time.Millisecond)
		synctest.Wait()

		for _, key := range keys {
			v := ch.Get(key)
			asserter.True(v.Found)
			asserter.EqualValues(key, v.V)
		}

		// The next tick drains the queued keys in one batch.
		time.Sleep(15*time.Millisecond + bulkUpdaterLatency)
		synctest.Wait()
		asserter.EqualValues(len(keys), ranUpdater.Load())

		for idx, key := range keys {
			v := ch.Get(key)
			asserter.True(v.Found)
			expected := fmt.Sprintf("key_%d_updated", idx)
			asserter.EqualValues(expected, v.V)
		}
	})
}

func TestThresholdUpdater(tt *testing.T) {
	const (
		cacheAge  = time.Second
		threshold = time.Millisecond * 500
	)

	newUpdaterCache := func(t *testing.T, ranUpdater *atomic.Bool) *Cache[string, string] {
		requirer := require.New(t)
		ch, err := New(Config[string, string]{
			CacheAge:  cacheAge,
			Threshold: threshold,
			Updater: func(ctx context.Context, key string) (string, error) {
				ranUpdater.Store(true)
				return key, nil
			},
		})
		requirer.NoError(err)
		return ch
	}

	tt.Run("before threshold", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asserter := require.New(t)
			ranUpdater := atomic.Bool{}
			ch := newUpdaterCache(t, &ranUpdater)
			defer ch.Close()

			key := "key_1"
			ch.Add(key, key)
			ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})

			v := ch.Get(key)
			asserter.True(v.Found)
			asserter.False(ranUpdater.Load())
			asserter.EqualValues(key, v.V)
		})
	})

	tt.Run("during threshold", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asserter := require.New(t)
			ranUpdater := atomic.Bool{}
			ch := newUpdaterCache(t, &ranUpdater)
			defer ch.Close()

			key := "key_2"
			ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
			time.Sleep((cacheAge - threshold) + time.Millisecond)
			synctest.Wait()
			v := ch.Get(key)
			asserter.True(v.Found)
			asserter.EqualValues(key, v.V)

			// wait for updater to complete execution
			synctest.Wait()
			asserter.True(ranUpdater.Load())
		})
	})

	tt.Run("after threshold (cache expired)", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asserter := require.New(t)
			ranUpdater := atomic.Bool{}
			ch := newUpdaterCache(t, &ranUpdater)
			defer ch.Close()

			key := "key_3"
			ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
			time.Sleep(time.Millisecond * 1100)
			synctest.Wait()

			v := ch.Get(key)
			asserter.False(v.Found)
			asserter.False(ranUpdater.Load())
			asserter.EqualValues("", v.V)
		})
	})
}

func TestThresholdUpdaterStale(tt *testing.T) {
	const (
		cacheAge  = time.Second
		threshold = time.Millisecond * 500
	)

	newUpdaterCache := func(t *testing.T, ranUpdater *atomic.Bool) *Cache[string, string] {
		requirer := require.New(t)
		ch, err := New(Config[string, string]{
			ServeStale: true,
			CacheAge:   cacheAge,
			Threshold:  threshold,
			Updater: func(ctx context.Context, key string) (string, error) {
				ranUpdater.Store(true)
				return key, nil
			},
		})
		requirer.NoError(err)
		return ch
	}

	tt.Run("before threshold", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asserter := require.New(t)
			ranUpdater := atomic.Bool{}
			ch := newUpdaterCache(t, &ranUpdater)
			defer ch.Close()

			key := "key_1"
			ch.Add(key, key)
			ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})

			v := ch.Get(key)
			asserter.True(v.Found)
			asserter.False(ranUpdater.Load())
			asserter.EqualValues(key, v.V)
		})
	})

	tt.Run("during threshold", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asserter := require.New(t)
			ranUpdater := atomic.Bool{}
			ch := newUpdaterCache(t, &ranUpdater)
			defer ch.Close()

			key := "key_2"
			ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
			time.Sleep((cacheAge - threshold) + time.Millisecond)
			synctest.Wait()
			v := ch.Get(key)
			asserter.True(v.Found)
			asserter.EqualValues(key, v.V)

			// wait for updater to complete execution
			synctest.Wait()
			asserter.True(ranUpdater.Load())
		})
	})

	tt.Run("after threshold (cache expired)", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asserter := require.New(t)
			ranUpdater := atomic.Bool{}
			ch := newUpdaterCache(t, &ranUpdater)
			defer ch.Close()

			key := "key_3"
			ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
			time.Sleep(cacheAge + time.Millisecond)
			synctest.Wait()

			v := ch.Get(key)
			asserter.True(v.Found)
			asserter.EqualValues(key, v.V)

			// wait for updater to complete execution
			synctest.Wait()
			asserter.True(ranUpdater.Load())
		})
	})

	tt.Run("long after threshold (cache expired)", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asserter := require.New(t)
			ranUpdater := atomic.Bool{}
			ch := newUpdaterCache(t, &ranUpdater)
			defer ch.Close()

			key := "key_4"
			ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
			time.Sleep(cacheAge + 2*threshold)
			synctest.Wait()

			v := ch.Get(key)
			asserter.True(v.Found)
			asserter.EqualValues(key, v.V)

			// wait for updater to complete execution
			synctest.Wait()
			asserter.True(ranUpdater.Load())
		})
	})
}

func TestValidate(tt *testing.T) {
	asserter := assert.New(tt)
	requirer := require.New(tt)

	tt.Run("invalid LRU cache size", func(t *testing.T) {
		cfg := Config[string, string]{
			LRUCacheSize: 0,
		}
		err := cfg.Validate()
		requirer.NotNil(err)
		asserter.ErrorIs(err, ErrValidation)
	})
	tt.Run("invalid threshold", func(t *testing.T) {
		cfg := Config[string, string]{
			LRUCacheSize: 10,
			CacheAge:     time.Second,
			Threshold:    time.Second,
		}
		err := cfg.Validate()
		requirer.NotNil(err)
		asserter.ErrorIs(err, ErrValidation)
	})

	tt.Run("valid configuration", func(t *testing.T) {
		cfg := Config[string, string]{
			LRUCacheSize: 10,
			CacheAge:     time.Minute,
			Threshold:    time.Second,
		}
		err := cfg.Validate()
		requirer.Nil(err)
	})
}

func TestSanitize(tt *testing.T) {
	asserter := assert.New(tt)

	cfg := Config[string, string]{}
	cfg.Sanitize()
	asserter.Equal(cfg.LRUCacheSize, uint(1000))
	asserter.Equal(cfg.QLength, uint(1000))
	asserter.Equal(cfg.CacheAge, time.Minute)
	asserter.Equal(cfg.Threshold, time.Second*59)
	asserter.Equal(cfg.UpdaterTimeout, time.Second)
}

func TestPayload(tt *testing.T) {
	asserter := assert.New(tt)

	tt.Run("expiry & payload available", func(t *testing.T) {
		expireAt := time.Now().Add(time.Minute)
		cea := atomic.Pointer[time.Time]{}
		cea.Store(&expireAt)
		value := "hello world"
		pyl := Payload[string]{
			ExpireAt: &cea,
			Payload:  value,
		}
		asserter.Equal(value, pyl.Value())
		asserter.EqualValues(expireAt, pyl.Expiry())
	})

	tt.Run("expiry not available", func(t *testing.T) {
		value := "hello world"
		pyl := Payload[string]{
			ExpireAt: nil,
			Payload:  value,
		}
		asserter.Equal(value, pyl.Value())
		asserter.EqualValues(time.Time{}, pyl.Expiry())
	})

	tt.Run("value not available", func(t *testing.T) {
		expireAt := time.Now().Add(time.Minute)
		cea := atomic.Pointer[time.Time]{}
		cea.Store(&expireAt)
		pyl := Payload[any]{
			ExpireAt: &cea,
			Payload:  nil,
		}
		asserter.Equal(nil, pyl.Value())
		asserter.EqualValues(expireAt, pyl.Expiry())
	})

	tt.Run("expiry & value not available", func(t *testing.T) {
		pyl := Payload[any]{
			ExpireAt: nil,
			Payload:  nil,
		}
		asserter.Equal(nil, pyl.Value())
		asserter.EqualValues(time.Time{}, pyl.Expiry())
	})
}

func TestErrWatcher(tt *testing.T) {
	const (
		prefix = "prefix"
		value  = "value"
	)

	tt.Run("err watcher", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			forcedErr := fmt.Errorf("forced error")
			ranUpdater := atomic.Bool{}
			ranErrWatcher := atomic.Bool{}
			var captured errCapture

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				Threshold:    time.Second * 59,
				DisableCache: false,
				Updater: func(ctx context.Context, key string) (any, error) {
					ranUpdater.Store(true)
					return nil, forcedErr
				},
				ErrWatcher: func(watcherErr error) {
					ranErrWatcher.Store(true)
					captured.set(watcherErr)
				},
			})
			requirer.NoError(err)
			defer cache.Close()

			_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			// wait for threshold window
			time.Sleep(time.Second)
			synctest.Wait()
			// trigger auto update within threshold window
			_ = cache.Get(prefix)

			// wait for the updater callback to be executed
			synctest.Wait()
			asserter.True(ranUpdater.Load())
			asserter.True(ranErrWatcher.Load())
			asserter.ErrorIs(captured.get(), forcedErr)
		})
	})

	tt.Run("no err watcher", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			forcedErr := fmt.Errorf("forced error")
			ranUpdater := atomic.Bool{}
			ranErrWatcher := atomic.Bool{}

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				Threshold:    time.Second * 59,
				DisableCache: false,
				Updater: func(ctx context.Context, key string) (any, error) {
					ranUpdater.Store(true)
					return nil, forcedErr
				},
			})
			requirer.NoError(err)
			defer cache.Close()

			_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
			// wait for threshold window
			time.Sleep(time.Second)
			synctest.Wait()
			// trigger auto update within threshold window
			_ = cache.Get(prefix)

			// wait for the updater callback to be executed
			synctest.Wait()
			asserter.True(ranUpdater.Load())
			asserter.False(ranErrWatcher.Load())
		})
	})

	tt.Run("err watcher: catch panic text", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			ranUpdater := atomic.Bool{}
			ranErrWatcher := atomic.Bool{}
			var captured errCapture

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				Threshold:    time.Second * 59,
				DisableCache: false,
				Updater: func(ctx context.Context, key string) (any, error) {
					ranUpdater.Store(true)
					panic("force panicked")
				},
				ErrWatcher: func(watcherErr error) {
					ranErrWatcher.Store(true)
					captured.set(watcherErr)
				},
			})
			requirer.NoError(err)
			defer cache.Close()
			cache.Add(prefix, value)

			// wait for threshold window
			time.Sleep(time.Second)
			synctest.Wait()
			// trigger auto update within threshold window
			_ = cache.Get(prefix)

			// wait for the updater callback to be executed
			synctest.Wait()
			asserter.True(ranUpdater.Load())
			asserter.True(ranErrWatcher.Load())
			asserter.ErrorContains(captured.get(), "force panicked")
		})
	})

	tt.Run("err watcher: catch panic err", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			requirer := require.New(t)
			asserter := require.New(t)

			ranUpdater := atomic.Bool{}
			ranErrWatcher := atomic.Bool{}
			forcedPanicErr := errors.New("panic err")
			var captured errCapture

			cache, err := New(Config[string, any]{
				LRUCacheSize: 10000,
				CacheAge:     time.Minute,
				Threshold:    time.Second * 59,
				DisableCache: false,
				Updater: func(ctx context.Context, key string) (any, error) {
					ranUpdater.Store(true)
					panic(forcedPanicErr)
				},
				ErrWatcher: func(watcherErr error) {
					ranErrWatcher.Store(true)
					captured.set(watcherErr)
				},
			})
			requirer.NoError(err)
			defer cache.Close()
			cache.Add(prefix, value)

			// wait for threshold window
			time.Sleep(time.Second)
			synctest.Wait()
			// trigger auto update within threshold window
			_ = cache.Get(prefix)

			// wait for the updater callback to be executed
			synctest.Wait()
			asserter.True(ranUpdater.Load())
			asserter.True(ranErrWatcher.Load())
			asserter.ErrorIs(captured.get(), forcedPanicErr)
		})
	})
}

// TestCacheRealClockSmoke exercises the cache against the real wall clock,
// outside of any synctest bubble, as a smoke test that the fake-clock
// migration above hasn't diverged from real time.Sleep/time.Now behaviour.
func TestCacheRealClockSmoke(tt *testing.T) {
	requirer := require.New(tt)
	asserter := require.New(tt)

	const (
		key = "smoke_key"
		val = "smoke_value"
	)

	cache, err := New(Config[string, string]{
		LRUCacheSize: 10,
		CacheAge:     50 * time.Millisecond,
	})
	requirer.NoError(err)
	defer cache.Close()

	cache.Add(key, val)
	// Only assert expiry here: a scheduler pause could consume the whole TTL
	// before an immediate hit assertion. Hits are covered in the fake clock tests.
	time.Sleep(100 * time.Millisecond)
	v := cache.Get(key)
	asserter.False(v.Found)
}
