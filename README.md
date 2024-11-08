<p align="center"><img src="https://github.com/user-attachments/assets/1038467d-6058-4227-8a59-cf29b847fb2b" alt="pocache gopher" width="256px"/></p>

[![](https://github.com/naughtygopher/pocache/actions/workflows/go.yml/badge.svg?branch=main)](https://github.com/naughtygopher/pocache/actions)
[![Go Reference](https://pkg.go.dev/badge/github.com/naughtygopher/pocache.svg)](https://pkg.go.dev/github.com/naughtygopher/pocache)
[![Go Report Card](https://goreportcard.com/badge/github.com/naughtygopher/pocache)](https://goreportcard.com/report/github.com/naughtygopher/pocache)
[![Coverage Status](https://coveralls.io/repos/github/naughtygopher/pocache/badge.svg?branch=main)](https://coveralls.io/github/naughtygopher/pocache?branch=main)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/creativecreature/sturdyc/blob/master/LICENSE)

# Pocache

Pocache (`poh-cash (/poʊ kæʃ/)`), **P**reemptive **o**ptimistic cache, is a lightweight in-app caching package. It introduces preemptive cache updates, optimizing performance in concurrent environments by reducing redundant database calls while maintaining fresh data. It uses [Hashicorp's Go LRU package](https://github.com/hashicorp/golang-lru) as the default storage.

Yet another _elegant_ solution for the infamous [Thundering herd problem](https://en.wikipedia.org/wiki/Thundering_herd_problem), save your database(s)!

## Key Features

1. **Preemptive Cache Updates:** Automatically updates cache entries _nearing_ expiration.
2. **Threshold Window:** Configurable time window before cache expiration to trigger updates.
3. **Serve stale**: Opt-in configuration to serve expired cache and do a background refresh.
4. **Debounced Updates:** Prevents excessive I/O calls by debouncing concurrent update requests for the same key.
5. **Custom store**: customizable underlying storage to extend/replace in-app cache or use external cache database.

## Why use Pocache?

In highly concurrent environments (e.g., web servers), multiple requests try to access the same cache entry simultaneously. Without query call suppression / call debouncing, the app would query the underlying database multiple times until the cache is refreshed. While trying to solve the thundering herd problem, most applications serve stale/expired cache until the update is completed.

Pocache solves these scenarios by combining debounce mechanism along with optimistic updates during the threshold window, keeping the cache up to date all the time and never having to serve stale cache!

## How does it work?

Given a cache expiration time and a threshold window, Pocache triggers a preemptive cache update when a value is accessed within the threshold window.

Example:

-   Cache expiration: 10 minutes
-   Threshold window: 1 minute

```
|______________________ ____threshold window__________ ______________|
0 min                   9 mins                         10 mins
Add key here            Get key within window          Key expires
```

When a key is fetched within the threshold window (between 9-10 minutes), Pocache initiates a background update for that key (_preemptive_). This ensures fresh data availability, anticipating future usage (_optimistic_).

## Custom store

Pocache defines the following interface for its underlying storage. You can configure storage of your choice as long as it implements this simple interface, and is provided as a configuration.

```golang
type store[K comparable, T any] interface {
	Add(key K, value *Payload[T]) (evicted bool)
	Get(key K) (value *Payload[T], found bool)
	Remove(key K) (present bool)
}
```

Below is an example(not for production use) of setting a custom store.

```golang
type mystore[Key comparable, T any] struct{
    data sync.Map
}

func (ms *mystore[K,T]) Add(key K, value *Payload[T]) (evicted bool) {
    ms.data.Store(key, value)
}

func (ms *mystore[K,T]) Get(key K) (value *Payload[T], found bool) {
    v, found  := ms.data.Load(key)
    if !found {
        return nil, found
    }

    value, _ := v.(*Payload[T])
    return value, true
}

func (ms *mystore[K,T]) Remove(key K) (present bool) {
    _, found  := ms.data.Load(key)
    ms.data.Delete(key)
    return found
}

func foo() {
    cache, err := pocache.New(pocache.Config[string, string]{
        Store: mystore{data: sync.Map{}}
	})
}
```

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

The gopher used here was created using [Gopherize.me](https://gopherize.me/). Pocache helps you to stop the herd from thundering.
