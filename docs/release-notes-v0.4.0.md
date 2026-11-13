# v0.4.0 [minor]

Status: Draft, unreleased

Pocache now requires **Go 1.26 or newer**. This follows the supported Go release policy. Consumers using `GOTOOLCHAIN=local` need to upgrade their installed toolchain; the default `GOTOOLCHAIN=auto` can select a newer toolchain.

- Added `Cache.Close()` to cancel background updates and wait for workers. Reuse a cache across requests and close it when its owner shuts down. After close, reads miss and writes are no-ops.
- Includes the previously unreleased `BulkUpdater`, `Tuple`, and `BulkAdd` APIs. `BulkUpdater` returns `(map[K]T, error)`: keys missing from the map are retried on a later read, and errors are reported to `ErrWatcher` wrapped in `ErrBulkUpdate`. `BulkSize` (default 100) and `BulkMaxWait` (default 15ms) control batching.
- Updates, single key and bulk, now run on [nibbler](https://github.com/naughtygopher/nibbler) v0.3.1. Setting both `Updater` and `BulkUpdater` is now a validation error instead of silently ignoring `BulkUpdater`.
- `Get` no longer blocks when the update or deletion queue is full. The refresh or removal is skipped and retried by a later read. Raise `QLength` if reads outpace your updater.
- The module now depends on nibbler, which pulls testify to v1.12.1 for tests.
- Timing tests now use `testing/synctest`. Added shutdown race coverage, `b.Loop()` benchmarks, and modernization lint checks.

Updaters must honor context cancellation for prompt shutdown. Queued updates and deletions are abandoned; the store is retained. See the README for complete examples and the shutdown contract.
