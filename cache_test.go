package pochache

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCache(t *testing.T) {
	var (
		prefix   = "prefix"
		value    = "value"
		requirer = require.New(t)
	)

	t.Run("found", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v := cache.Get(prefix)
		requirer.True(v.Found)
		requirer.Equal(v.V, value)
	})

	t.Run("not found", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v := cache.Get(prefix + "_does_not_exist")
		requirer.False(v.Found)
		requirer.Equal(v.V, nil)
	})

	t.Run("cache age expired", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 1,
			CacheAge:     time.Nanosecond,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		time.Sleep(time.Millisecond)
		v := cache.Get(prefix)
		requirer.False(v.Found)
		requirer.Equal(v.V, nil)
	})

	t.Run("update cache", func(t *testing.T) {
		cache, err := New(Config[string, any]{
			LRUCacheSize: 10000,
			CacheAge:     time.Minute,
			DisableCache: false,
		})
		requirer.NoError(err)

		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v := cache.Get(prefix)
		requirer.True(v.Found)
		requirer.Equal(v.V, value)

		value = "new_value"
		cache.BulkAdd([]Tuple[string, any]{{Key: prefix, Value: value}})
		v = cache.Get(prefix)
		requirer.True(v.Found)
		requirer.Equal(v.V, value)
	})

	t.Run("multiple Add/Get to check if channel blocks", func(t *testing.T) { //nolint:govet
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
			requirer.True(v.Found)
			requirer.Equal(v.V, value)
		}
	})
}

func TestThresholdUpdater(t *testing.T) {
	var (
		requirer  = require.New(t)
		asserter  = require.New(t)
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
	t.Run("before threshold", func(t *testing.T) {
		ranUpdater.Store(false)
		key := "key_1"
		ch.Add(key, key)
		ch.BulkAdd([]Tuple[string, string]{{Key: key, Value: key}})

		v := ch.Get(key)
		asserter.True(v.Found)
		asserter.False(ranUpdater.Load())
		asserter.EqualValues(key, v.V)
	})

	t.Run("during threshold", func(t *testing.T) {
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

	t.Run("after threshold (cache expired)", func(t *testing.T) {
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
