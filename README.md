<p align="center"><img src="https://github.com/user-attachments/assets/1038467d-6058-4227-8a59-cf29b847fb2b" alt="pocache gopher" width="256px"/></p>

[![](https://github.com/naughtygopher/pocache/actions/workflows/go.yml/badge.svg?branch=main)](https://github.com/naughtygopher/pocache/actions)
[![](https://godoc.org/github.com/nathany/looper?status.svg)](http://godoc.org/github.com/naughtygopher/pocache)

# Pocache

Pocache (`poh-cash (/poʊ kæʃ/)`) is a lightweight in-app caching solution. It introduces preemptive cache updates, optimizing performance in concurrent environments by reducing redundant database calls while maintaining fresh data. It uses [Hashicorp's Go LRU package](https://github.com/hashicorp/golang-lru/tree/v2) as the default storage.

## Key Features

1. **Preemptive Cache Updates:** Automatically updates cache entries nearing expiration.
2. **Threshold Window:** Configurable time window before cache expiration to trigger updates.
3. **Debounced Updates:** Prevents excessive I/O calls by debouncing concurrent requests for the same key.

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

When a key is fetched between 9-10 minutes (within the threshold window), Pocache initiates an update for that key. This ensures fresh data availability, anticipating future usage (_optimistic_).

## Why Use Preemptive Updates?

In highly concurrent environments (e.g., web servers), multiple requests might try to access the same cache entry simultaneously. Without preemptive updates, the system would query the underlying database multiple times until the cache is refreshed.

Additionally by debouncing these requests, Pocache ensures only a single update is triggered, reducing load on both the underlying storage and the application.

## The gopher

The gopher used here was created using [Gopherize.me](https://gopherize.me/). Incache helps you keep your application latency low and your database destressed.
