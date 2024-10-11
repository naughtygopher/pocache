<p align="center"><img src="https://github.com/user-attachments/assets/1038467d-6058-4227-8a59-cf29b847fb2b" alt="pocache gopher" width="256px"/></p>

[![](https://github.com/naughtygopher/pocache/actions/workflows/go.yml/badge.svg?branch=main)](https://github.com/naughtygopher/pocache/actions)
[![Go Reference](https://pkg.go.dev/badge/github.com/naughtygopher/pocache.svg)](https://pkg.go.dev/github.com/naughtygopher/pocache)
[![Go Report Card](https://goreportcard.com/badge/github.com/naughtygopher/pocache)](https://goreportcard.com/report/github.com/naughtygopher/pocache)
[![Coverage Status](https://coveralls.io/repos/github/naughtygopher/pocache/badge.svg?branch=main)](https://coveralls.io/github/naughtygopher/pocache?branch=main)

# Pocache

Pocache (`poh-cash (/poʊ kæʃ/)`), **P**reemptive **o**ptimistic cache, is a lightweight in-app caching package. It introduces preemptive cache updates, optimizing performance in concurrent environments by reducing redundant database calls while maintaining fresh data. It uses [Hashicorp's Go LRU package](https://github.com/hashicorp/golang-lru) as the default storage.

## Key Features

1. **Preemptive Cache Updates:** Automatically updates cache entries _nearing_ expiration.
2. **Threshold Window:** Configurable time window before cache expiration to trigger updates.
3. **Serve stale**: Opt-in configuration to serve even expired cache and do a background refresh.
4. **Debounced Updates:** Prevents excessive I/O calls by debouncing concurrent requests for the same key.
5. **Custom store**: customizable underlying storage to extend in-app cache to external database

## How does it work?

Given a cache expiration time and a threshold window, Pocache triggers a preemptive cache update when a value is accessed within the threshold window.
Example:

-   Cache expiration: 10 minutes
-   Threshold window: 1 minute

```
|______________________ __threshold window__________ ______________|
0 min                   9 mins                       10 mins
Add key here            Get key within window        Key expires
```

When a key is fetched between 9-10 minutes (within the threshold window), Pocache initiates an update for that key (_preemptive_). This ensures fresh data availability, anticipating future usage (_optimistic_).

## Why use preemptive updates?

In highly concurrent environments (e.g., web servers), multiple requests might try to access the same cache entry simultaneously. Without preemptive updates, the system would query the underlying database multiple times until the cache is refreshed.

Additionally by debouncing these requests, Pocache ensures only a single update is triggered, reducing load on both the underlying storage and the application itself.

## Full example

```golang
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/naughtygopher/pocache"
)

type Item struct {
	ID          string
	Name        string
	Description string
}

func newItem(key string) *Item {
	return &Item{
		ID:          fmt.Sprintf("%d", time.Now().Nanosecond()),
		Name:        "name::" + key,
		Description: "description::" + key,
	}
}

func updater(ctx context.Context, key string) (*Item, error) {
	return newItem(key), nil
}

func onErr(err error) {
	panic(fmt.Sprintf("this should never have happened!: %+v", err))
}

func main() {
	cache, err := pocache.New(pocache.Config[string, *Item]{
		// LRUCacheSize is the number of keys to be maintained in the cache (Optional, default 1000)
		LRUCacheSize: 100000,
		// QLength is the length of update and delete queue (Optional, default 1000)
		QLength: 1000,

		// CacheAge is for how long the cache would be maintained, apart from the LRU eviction
		// It's maintained to not maintain stale data if/when keys are not evicted based on LRU
		// (Optional, default 1minute)
		CacheAge: time.Hour,
		// Threshold is the duration prior to expiry, when the key is considered eligible to be updated
		// (Optional, default 1 second)
		Threshold: time.Minute * 5,

		// ServeStale will not return error if the cache has expired. It will return the stale
		// value, and trigger an update as well. This is useful for usecases where it's ok
		// to serve stale values and data consistency is not of paramount importance.
		// (Optional, default false)
		ServeStale: false,

		// UpdaterTimeout is the context time out for when the updater function is called
		// (Optional, default 1 second)
		UpdaterTimeout: time.Second * 15,
		// Updater is optional, but without it it's a basic LRU cache
		Updater: updater,

		// ErrWatcher is called when there's any error when trying to update cache (Optional)
		ErrWatcher: onErr,
	})
	if err != nil {
		panic(err)
	}

	const key = "hello"
	item := newItem(key)
	e := cache.Add(key, item)
	fmt.Println("evicted:", e)

	ee := cache.BulkAdd([]pocache.Tuple[string, *Item]{
		{Key: key + "2", Value: newItem(key + "2")},
	})
	fmt.Println("evicted list:", ee)

	ii := cache.Get(key)
	if ii.Found {
		fmt.Println("value:", ii.V)
	}

	ii = cache.Get(key + "2")
	if ii.Found {
		fmt.Println("value:", ii.V)
	}
}
```

## The gopher

The gopher used here was created using [Gopherize.me](https://gopherize.me/). Incache helps you keep your application latency low and your database destressed.
