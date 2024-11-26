package pocache

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCache(tt *testing.T) {
	var (
		prefix   = "prefix"
		value    = "value"
		requirer = require.New(tt)
		asserter = require.New(tt)
	)

	tt.Run("found", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v := cache.Get(prefix)
		asserter.True(v.Found)
		asserter.Equal(v.V, value)
	})

	tt.Run("not found", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v := cache.Get(prefix + "_does_not_exist")
		asserter.False(v.Found)
		asserter.Equal(v.V, nil)
	})

	tt.Run("cache age expired", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 1,
			CacheAge:     time.Nanosecond,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		time.Sleep(time.Millisecond)
		v := cache.Get(prefix)
		asserter.False(v.Found)
		asserter.Equal(v.V, nil)
	})

	tt.Run("update cache", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v := cache.Get(prefix)
		asserter.True(v.Found)
		asserter.Equal(v.V, value)

		value = "new_value"
		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v = cache.Get(prefix)
		asserter.True(v.Found)
		asserter.Equal(v.V, value)
	})

	tt.Run("multiple Add/Get to check if channel blocks", func(t *testing.T) { //nolint:govet
		// limit should be greater than the channel buffer for updateQ & deleteQ
		limit := 200
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			DisableCache: false,
		})
		requirer.NoError(err)

		for i := 0; i < limit; i++ {
			prefix := fmt.Sprintf("%s_%d", prefix, i)
			value := fmt.Sprintf("%s_%d", value, i)
			cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		}

		for i := 0; i < limit; i++ {
			prefix := fmt.Sprintf("%s_%d", prefix, i)
			value := fmt.Sprintf("%s_%d", value, i)
			v := cache.Get(prefix)
			asserter.True(v.Found)
			asserter.Equal(v.V, value)
		}
	})

	tt.Run("serve stale", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Second * 2,
			DisableCache: false,
			ServeStale:   true,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		// wait for cache to expire
		time.Sleep(time.Second * 3)

		v := cache.Get(prefix)
		asserter.True(v.Found)
		asserter.Equal(v.V, value)
	})

	tt.Run("debounce", func(t *testing.T) {
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

		_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		// wait for threshold window
		time.Sleep(time.Second)
		// trigger auto update within threshold window
		_ = cache.Get(prefix)

		// re-trigger auto update within threshold window
		_ = cache.Get(prefix)
		// check if key added to debounce checker map
		_, found := cache.updateInProgress.Load(prefix)
		asserter.True(found)
	})

	tt.Run("disabled", func(t *testing.T) {
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

		_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		// wait for threshold window
		time.Sleep(time.Second * 2)

		// trigger auto update within threshold window
		_ = cache.Get(prefix)

		// wait for updater to be executed
		time.Sleep(time.Second * 1)
		v := cache.Get(prefix)
		asserter.False(v.Found)
	})

	tt.Run("no updater", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			Threshold:    time.Second * 59,
			DisableCache: false,
			Updater:      nil,
		})
		requirer.NoError(err)

		_ = cache.Add(prefix, value)
		// wait for threshold window
		time.Sleep(time.Second * 2)
		// trigger auto update within threshold window
		_ = cache.Get(prefix)
		// wait for updater to run
		time.Sleep(time.Second * 2)

		v := cache.Get(prefix)
		asserter.EqualValues(value, v.V)
	})

}

func TestThresholdUpdater(tt *testing.T) {
	var (
		requirer  = require.New(tt)
		asserter  = require.New(tt)
		cacheAge  = time.Second
		threshold = time.Millisecond * 500
	)

	ranUpdater := atomic.Bool{}

	ch, err := New(Config[string, string]{
		CacheAge:  cacheAge,
		Threshold: threshold,
		Updater: func(ctx context.Context, key string) (string, error) {
			ranUpdater.Store(true)
			return key, nil
		},
	})
	requirer.NoError(err)
	tt.Run("before threshold", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_1"
		ch.Add(key, key)
		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})

		v := ch.Get(key)
		asserter.True(v.Found)
		asserter.False(ranUpdater.Load())
		asserter.EqualValues(key, v.V)
	})

	tt.Run("during threshold", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_2"

		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
		time.Sleep((cacheAge - threshold) + time.Millisecond)
		v := ch.Get(key)
		asserter.True(v.Found)
		asserter.EqualValues(key, v.V)
		// wait for updater to complete execution
		time.Sleep(time.Millisecond * 100)
		asserter.True(ranUpdater.Load())
	})

	tt.Run("after threshold (cache expired)", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_3"

		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
		time.Sleep(time.Millisecond * 1100)

		v := ch.Get(key)
		asserter.False(v.Found)
		asserter.False(ranUpdater.Load())
		asserter.EqualValues("", v.V)
	})
}

func TestThresholdUpdaterStale(tt *testing.T) {
	var (
		requirer  = require.New(tt)
		asserter  = require.New(tt)
		cacheAge  = time.Second
		threshold = time.Millisecond * 500
	)

	ranUpdater := atomic.Bool{}

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
	tt.Run("before threshold", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_1"
		ch.Add(key, key)
		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})

		v := ch.Get(key)
		asserter.True(v.Found)
		asserter.False(ranUpdater.Load())
		asserter.EqualValues(key, v.V)
	})

	tt.Run("during threshold", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_2"

		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
		time.Sleep((cacheAge - threshold) + time.Millisecond)
		v := ch.Get(key)
		asserter.True(v.Found)
		asserter.EqualValues(key, v.V)
		// wait for updater to complete execution
		time.Sleep(time.Millisecond * 100)
		asserter.True(ranUpdater.Load())
	})

	tt.Run("after threshold (cache expired)", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_3"

		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
		time.Sleep(cacheAge + time.Millisecond)

		v := ch.Get(key)
		asserter.True(v.Found)
		asserter.EqualValues(key, v.V)

		// wait for updater to complete execution
		time.Sleep(time.Millisecond * 100)
		asserter.True(ranUpdater.Load())
	})

	tt.Run("long after threshold (cache expired)", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_4"

		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})
		time.Sleep(cacheAge + 2*threshold)

		v := ch.Get(key)
		asserter.True(v.Found)
		asserter.EqualValues(key, v.V)

		// wait for updater to complete execution
		time.Sleep(time.Millisecond * 100)
		asserter.True(ranUpdater.Load())
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
	var (
		prefix   = "prefix"
		value    = "value"
		requirer = require.New(tt)
		asserter = require.New(tt)
	)

	tt.Run("err watcher", func(t *testing.T) {
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
			ErrWatcher: func(watcherErr error) {
				ranErrWatcher.Store(true)
				asserter.ErrorIs(watcherErr, forcedErr)
			},
		})
		requirer.NoError(err)

		_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		// wait for threshold window
		time.Sleep(time.Second)
		// trigger auto update within threshold window
		_ = cache.Get(prefix)

		// wait for the updater callback to be executed
		time.Sleep(time.Second * 2)
		asserter.True(ranUpdater.Load())
		asserter.True(ranErrWatcher.Load())
	})

	tt.Run("no err watcher", func(t *testing.T) {
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

		_ = cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		// wait for threshold window
		time.Sleep(time.Second)
		// trigger auto update within threshold window
		_ = cache.Get(prefix)

		// wait for the updater callback to be executed
		time.Sleep(time.Second * 2)
		asserter.True(ranUpdater.Load())
		asserter.False(ranErrWatcher.Load())
	})

	tt.Run("err watcher: catch panic text", func(t *testing.T) {
		ranUpdater := atomic.Bool{}
		ranErrWatcher := atomic.Bool{}

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
				asserter.ErrorContains(watcherErr, "force panicked")
			},
		})
		requirer.NoError(err)
		cache.Add(prefix, value)

		// wait for threshold window
		time.Sleep(time.Second)
		// trigger auto update within threshold window
		_ = cache.Get(prefix)

		// wait for the updater callback to be executed
		time.Sleep(time.Second * 2)
		asserter.True(ranUpdater.Load())
		asserter.True(ranErrWatcher.Load())

	})

	tt.Run("err watcher: catch panic err", func(t *testing.T) {
		ranUpdater := atomic.Bool{}
		ranErrWatcher := atomic.Bool{}
		ErrPanic := errors.New("panic err")

		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			Threshold:    time.Second * 59,
			DisableCache: false,
			Updater: func(ctx context.Context, key string) (any, error) {
				ranUpdater.Store(true)
				panic(ErrPanic)
			},
			ErrWatcher: func(watcherErr error) {
				ranErrWatcher.Store(true)
				asserter.ErrorIs(watcherErr, ErrPanic)
			},
		})
		requirer.NoError(err)
		cache.Add(prefix, value)

		// wait for threshold window
		time.Sleep(time.Second)
		// trigger auto update within threshold window
		_ = cache.Get(prefix)

		// wait for the updater callback to be executed
		time.Sleep(time.Second * 2)
		asserter.True(ranUpdater.Load())
		asserter.True(ranErrWatcher.Load())

	})

}
