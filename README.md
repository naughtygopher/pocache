[![Go CI](https://github.com/naughtygopher/pocache/actions/workflows/go.yml/badge.svg?branch=main&event=push)](https://github.com/naughtygopher/pocache/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/naughtygopher/pocache.svg)](https://pkg.go.dev/github.com/naughtygopher/pocache)
[![Coverage Status](https://coveralls.io/repos/github/naughtygopher/pocache/badge.svg?branch=main)](https://coveralls.io/github/naughtygopher/pocache?branch=main)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Mentioned in Awesome Go](https://awesome.re/mentioned-badge.svg)](https://github.com/avelino/awesome-go#caches)

<p align="center"><img src="https://github.com/user-attachments/assets/1038467d-6058-4227-8a59-cf29b847fb2b" alt="pocache gopher" width="256"/></p>

# Pocache

Pocache (`poh-cash /poʊ kæʃ/`), **P**reemptive **o**ptimistic cache, is a lightweight in-app caching package for Go. It refreshes cached values in the background when they are accessed near expiration, helping reduce repeated database calls. It uses [HashiCorp's Go LRU package](https://github.com/hashicorp/golang-lru) as its default storage.

## Installation

Requires **Go 1.22 or newer**.

```sh
go get github.com/naughtygopher/pocache@latest
```

The latest tagged version is **v0.3.2**. This README describes `main`; the [bulk updater](#bulk-updates-main-unreleased) is not yet in a tagged release. To use that feature:

```sh
go get github.com/naughtygopher/pocache@main
```

## Usage

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/naughtygopher/pocache"
)

func load(ctx context.Context, key string) (string, error) {
	// Replace with a database or API call that respects ctx.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "value for " + key, nil
}

func main() {
	cache, err := pocache.New(pocache.Config[string, string]{
		CacheAge:  10 * time.Minute,
		Threshold: time.Minute,
		Updater:   load,
		ErrWatcher: func(err error) {
			log.Printf("cache refresh: %v", err)
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	const key = "hello"
	value := cache.Get(key)
	if !value.Found {
		// Cache misses are loaded by the caller, not by Updater.
		fresh, err := load(context.Background(), key)
		if err != nil {
			log.Fatal(err)
		}
		cache.Add(key, fresh)
		value = pocache.Value[string]{V: fresh, Found: true}
	}
	fmt.Println(value.V)
}
```

## How it works

With a cache age of 10 minutes and a threshold of 1 minute:

```text
0 min                   9 min                         10 min
Add key                 Get can trigger refresh       Key expires
|-----------------------|------ threshold window ------|
```

- **Before the threshold:** `Get` returns the cached value.
- **Within the threshold:** `Get` queues a background refresh and returns the current value. Refreshes are triggered by reads, not by a periodic scan of every key.
- **After expiration, with `ServeStale: false` (default):** `Get` returns `Found: false` and queues deletion. It does not call the updater for that miss.
- **After expiration, with `ServeStale: true`:** `Get` returns the expired value and queues a refresh if an updater is configured.
- **Missing or evicted keys:** `Get` returns `Found: false`; the caller must load and add the value.

Successful updates reset the entry's expiration. Failed updates are reported to `ErrWatcher`, if configured. Without either updater, Pocache still provides caching and expiration, but does not refresh values automatically. Serving stale values without an updater can return expired values until they are replaced or evicted.

Preemptive refresh and per-key update tracking help reduce the [thundering herd problem](https://en.wikipedia.org/wiki/Thundering_herd_problem) for cached entries. They do not guarantee freshness or coalesce caller-side loads on cache misses. Refreshes may fail or finish after expiration.

## Configuration

`New` takes a `Config[K, T]` by value and calls `SanitizeValidate` before creating the cache.

| Field | Default | Purpose |
| --- | --- | --- |
| `LRUCacheSize` | `1000` | Maximum entries in the default LRU store. |
| `QLength` | `1000` | Capacity of each background update/deletion queue. |
| `CacheAge` | `time.Minute` | Entry lifetime, reset by each `Add`. |
| `Threshold` | `CacheAge - time.Second` | Window before expiration in which reads trigger a refresh. With the default cache age, this is **59 seconds**. |
| `ServeStale` | `false` | Allow reads of expired values and request background refreshes. |
| `DisableCache` | `false` | Make `Get` always miss and `Add` a no-op. |
| `UpdaterTimeout` | `time.Second` | Deadline on the context passed to an updater. The updater must honor cancellation. |
| `Updater` | `nil` | Refresh one key at a time. |
| `BulkUpdater` | `nil` | Refresh batches of keys; available on `main`, unreleased. |
| `Store` | Default LRU | Custom concurrent storage implementing `Store[K, T]`. |
| `ErrWatcher` | `nil` | Callback for background update errors and recovered updater panics. |

Zero sizes and non-positive durations are replaced by defaults. `CacheAge` must be greater than the configured `Threshold`. For cache ages of one second or less, set an explicit positive threshold smaller than the cache age to avoid the non-positive derived default.

Configure either `Updater` or `BulkUpdater`; if both are set, `Updater` takes precedence. Updaters and `ErrWatcher` run on background workers and should return promptly. Queueing can block when a queue is full.

`New` starts background goroutines. There is currently no `Close` or shutdown method, so reuse long-lived cache instances rather than creating one per request. Custom stores must support concurrent access; callers must also synchronize mutations to cached pointers, maps, or slices.

## API

- `New(Config[K, T]) (*Cache[K, T], error)` constructs a cache.
- `Add(key, value) bool` inserts or replaces a value and resets its expiration. The result reports whether the store evicted an entry.
- `BulkAdd([]Tuple[K, T]) []bool` adds values and returns an eviction result for each input, in order.
- `Get(key) Value[T]` returns `V` and `Found`; check `Found` to distinguish a miss from a cached zero value.
- `DefaultStore[K, T](size)` creates the default LRU store.
- `Payload[T].Value()` and `Payload[T].Expiry()` expose stored data and expiration for store integrations.

See the [Go reference](https://pkg.go.dev/github.com/naughtygopher/pocache) for released API documentation. Configuration errors can be matched with `errors.Is(err, pocache.ErrValidation)`; recovered updater panics are reported through `ErrWatcher` with `pocache.ErrPanic`.

## Bulk updates (`main`, unreleased)

`BulkUpdater` receives queued keys in batches. It must return exactly one `UpdateResult[T]` per key, in the same order. Successful results replace the cached values; per-key errors are sent to `ErrWatcher`, and later eligible reads can retry them. A result-count mismatch is reported as a recovered panic.

For example, this configuration uses an in-memory data source in place of a batch database query:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/naughtygopher/pocache"
)

func main() {
	cache, err := pocache.New(pocache.Config[string, string]{
		CacheAge:  10 * time.Minute,
		Threshold: time.Minute,
		BulkUpdater: func(ctx context.Context, keys []string) []pocache.UpdateResult[string] {
			results := make([]pocache.UpdateResult[string], len(keys))
			for i, key := range keys {
				if err := ctx.Err(); err != nil {
					results[i].Err = err
					continue
				}
				results[i].NewValue = "updated " + key
			}
			return results
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	cache.BulkAdd([]pocache.Tuple[string, string]{
		{Key: "one", Value: "initial one"},
		{Key: "two", Value: "initial two"},
	})
	fmt.Println(cache.Get("one").V)
	// Subsequent reads within the threshold window queue background updates.
}
```

## Custom store

The exported storage interface is:

```go
type Store[K comparable, T any] interface {
	Add(key K, value *Payload[T]) (evicted bool)
	Get(key K) (value *Payload[T], found bool)
	Remove(key K) (present bool)
}
```

The following complete example uses `sync.Map`. It is concurrency-safe but unbounded: unlike the default LRU store, it has no capacity eviction. Pocache checks expiration on reads; this example has no background expiration sweep.

```go
package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/naughtygopher/pocache"
)

type memoryStore[K comparable, T any] struct {
	data sync.Map
}

func (ms *memoryStore[K, T]) Add(key K, value *pocache.Payload[T]) bool {
	ms.data.Store(key, value)
	return false
}

func (ms *memoryStore[K, T]) Get(key K) (*pocache.Payload[T], bool) {
	v, found := ms.data.Load(key)
	if !found {
		return nil, false
	}
	payload, ok := v.(*pocache.Payload[T])
	return payload, ok
}

func (ms *memoryStore[K, T]) Remove(key K) bool {
	_, present := ms.data.LoadAndDelete(key)
	return present
}

var _ pocache.Store[string, string] = (*memoryStore[string, string])(nil)

func main() {
	cache, err := pocache.New(pocache.Config[string, string]{
		Store: &memoryStore[string, string]{},
	})
	if err != nil {
		log.Fatal(err)
	}
	cache.Add("hello", "world")
	fmt.Println(cache.Get("hello").V)
}
```

## Development

```sh
go build ./...
go vet ./...
go test -race -covermode=atomic -coverprofile=coverage.out ./...
golangci-lint run ./...
```

GitHub Actions tests Go 1.22, Go 1.25, and the two current stable release lines (`oldstable` and `stable`). Lint runs on the latest stable Go with golangci-lint v2.13.2. Coverage is uploaded to Coveralls from the latest stable Go job on pushes to `main` and manual runs. Dependabot checks action versions weekly.

## License

Pocache is available under the [MIT License](LICENSE).

## The gopher

The gopher was created using [Gopherize.me](https://gopherize.me/). Pocache helps you stop the herd from thundering.
