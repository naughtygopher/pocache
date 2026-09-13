package pocache

import (
	"context"
	"testing"
	"time"
)

// BenchmarkGetHit measures the Get hot path when the key is present and not
// within the update threshold window.
func BenchmarkGetHit(b *testing.B) {
	cache, err := New(Config[string, string]{
		LRUCacheSize: 10000,
		CacheAge:     time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer cache.Close()

	cache.Add("key", "value")

	for b.Loop() {
		_ = cache.Get("key")
	}
}

// BenchmarkGetMiss measures the Get path when the key is absent.
func BenchmarkGetMiss(b *testing.B) {
	cache, err := New(Config[string, string]{
		LRUCacheSize: 10000,
		CacheAge:     time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer cache.Close()

	for b.Loop() {
		_ = cache.Get("missing_key")
	}
}

// BenchmarkGetInThreshold includes queueing and debouncing with an active updater.
func BenchmarkGetInThreshold(b *testing.B) {
	cache, err := New(Config[string, string]{
		LRUCacheSize: 10000,
		CacheAge:     time.Hour,
		Threshold:    time.Hour - time.Nanosecond,
		Updater: func(ctx context.Context, key string) (string, error) {
			return key, nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer cache.Close()

	cache.Add("key", "value")
	// Let the key cross into the threshold window before timing starts, so
	// every Get in the loop below enqueues/debounces an update.
	time.Sleep(time.Microsecond)

	for b.Loop() {
		_ = cache.Get("key")
	}
}

// BenchmarkAdd measures the cost of Add, including LRU insertion and
// expiry bookkeeping.
func BenchmarkAdd(b *testing.B) {
	cache, err := New(Config[string, string]{
		LRUCacheSize: 10000,
		CacheAge:     time.Hour,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer cache.Close()

	for b.Loop() {
		cache.Add("key", "value")
	}
}
