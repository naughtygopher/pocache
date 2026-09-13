# Cache shutdown

Date: 2026-09-13
Status: Accepted

`New` starts background workers. They need a defined lifetime both for short-lived cache owners and for deterministic tests, which wait for every goroutine to exit.

## Decision

- Add `Close()` without changing `New` or the `Store` interface. A cache-owned context supplies cancellation to queue operations and updater deadlines.
- Cancel active updater contexts, abandon queued work, stop the batch ticker, and wait for workers. Queue channels stay open because concurrent readers may still send to them.
- Once shutdown begins, new reads miss and writes are no-ops. Calls already in progress may finish. Repeated and concurrent closes wait on the same workers.
- Retain the store without clearing or closing it. Cache instances cannot restart. Updaters, store methods, and error watchers must return; callbacks cannot synchronously close their own cache.

## Consequences

Draining would make shutdown proportional to the queue length and could start fresh I/O during teardown. Abandoning work gives cancellation-aware updaters a prompt shutdown path, while retaining the existing API and caller-owned store.

`Close()` has no error result because cancellation and worker joining introduce no fallible operations. It cannot force user code to return. Calls that entered the store before shutdown are not joined; owners should stop request handlers before disposing of their store.

For a service, construct one cache during startup, pass it to handlers, and call `Close()` after the handlers stop. Tests construct the cache inside `synctest.Test` and defer `Close()`, so leaked workers fail the test.
