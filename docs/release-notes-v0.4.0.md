# v0.4.0 [minor]

Status: Draft, unreleased

Pocache now requires **Go 1.26 or newer**. This follows the supported Go release policy. Consumers using `GOTOOLCHAIN=local` need to upgrade their installed toolchain; the default `GOTOOLCHAIN=auto` can select a newer toolchain.

- Added `Cache.Close()` to cancel background updates and wait for workers. Reuse a cache across requests and close it when its owner shuts down. After close, reads miss and writes are no-ops.
- Includes the previously unreleased `BulkUpdater`, `UpdateResult`, `Tuple`, and `BulkAdd` APIs. Queued refreshes are processed together instead of unnecessarily splitting them into smaller batches.
- Timing tests now use `testing/synctest`. Added shutdown race coverage, `b.Loop()` benchmarks, and modernization lint checks.

Updaters must honor context cancellation for prompt shutdown. Queued updates and deletions are abandoned; the store is retained. See the README for complete examples and the shutdown contract.
