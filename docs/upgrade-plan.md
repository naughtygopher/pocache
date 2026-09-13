# Upgrade plan: Go 1.26 floor

Status: Implemented; hosted checks and release pending
Date: 2026-09-13
Depends on: PR #17 (README and workflow refresh)
Scope: single combined PR, stacked on PR #17

## Context

`go.mod` currently declares `go 1.22`, which is end of life; only Go 1.26 and
1.27 are supported. Commit 9f10da9 set the 1.22 floor to follow
endoflife.date, so the current directive contradicts the stated policy.

Two findings shape this plan:

1. `synctest.Test` waits for every goroutine in its bubble to exit, and fails
   the test if they deadlock (`testing/synctest/synctest.go:277-278`). Every
   `New()` starts `deleteListener` (cache.go:428) and, when an updater is
   configured, `updateListener` (cache.go:195). Neither exits, and
   `batchTicker` (cache.go:234) is never stopped. **`Close()` is a hard
   prerequisite for the `synctest` migration, not a nice-to-have.**
2. Go 1.26 adds no language-level changes; the spec's version appendix jumps
   from `Go_1.24` to `Go_1.27` (`doc/go_spec.html:8834,8842`). Every feature
   unlocked here (`synctest`, `testing.B.Loop`, `sync.WaitGroup.Go`,
   iterators, `weak`/`unique`) landed in Go 1.25 or earlier. Choosing 1.26
   over 1.25 buys support-policy alignment, not extra features.

## Commit sequence

Keep the commits ordered and individually green. The combined diff mixes an
API change with a test rewrite, so the split is what keeps review tractable.

### Commit 1 — Version floor

- [x] `go mod edit -go=1.26` and `go mod tidy`; do **not** add a `toolchain` directive
- [x] `.github/workflows/go.yml`: matrix to `["1.26.x", "oldstable", "stable"]`.
      `1.26.x` pins the floor explicitly; `oldstable` is redundant today and
      self-corrects when 1.28 ships. Keep `GOTOOLCHAIN: local`
- [x] `README.md`: change "Requires Go 1.22 or newer" to 1.26 and update the
      Development section's matrix wording
- [ ] Optional: testify v1.9.0 to v1.12.1 (its own floor is go 1.17, so it
      neither forces nor blocks this)

### Commit 2 — `Close()` lifecycle

Gates the `synctest` migration.

- [x] Add `Close()`: stop `batchTicker`, terminate `deleteListener` and
      `updateListener` so every goroutine exits
- [x] Resolve the shutdown race. `Get` sends on `deleteQ` and `updateQ`, so
      **closing those channels risks send-on-closed panics**. Prefer a `done`
      channel with `select`
- [x] Make `Close` idempotent; define `Get`/`Add` behaviour after close;
      decide drain vs. abandon for in-flight updates
- [x] Use `sync.WaitGroup.Go` if teardown waits in parallel
- [x] Remove the README "no `Close` or shutdown method" caveat and document
      shutdown plus instance reuse

### Commit 3 — `synctest` migration

- [x] Convert time-dependent tests to `synctest.Test`, constructing the cache
      **inside** the bubble with `defer cache.Close()`
- [x] Replace the 29 real sleeps with `time.Sleep(d)` followed by
      `synctest.Wait()`. **`synctest.Sleep` is Go 1.27 only**
- [x] Replace the `Eventually` polling in `TestThresholdBulkUpdater`
      (cache_test.go:256) with a deterministic `Wait()`
- [x] Keep one real-clock smoke test outside any bubble
- [x] Target: roughly 31s to near-instant, with flakiness eliminated

### Commit 4 — Benchmarks

- [x] Add `BenchmarkGet` and `BenchmarkAdd` using `b.Loop()`; none exist today
- [x] Cover the hit, miss, and in-threshold paths

### Commit 5 — Lint and modernization

- [x] Add `.golangci.yml` (none exists; only defaults run) enabling
      `modernize`, `intrange`, `copyloopvar`, `usestdlibvars`
- [x] Apply autofixes, for example `for i := 0; i < len(keys); i++`
      (cache.go:247) to `for range len(keys)`
- [ ] Optional CI gate: `go fix -diff ./...`, which exits non-zero when the
      diff is non-empty

## Deferred

Evaluate against a real need; do not adopt speculatively.

- [ ] **Iterators** (`iter.Seq`, Go 1.23): `Cache.All()`/`Keys()` is additive,
      but **adding to the `Store` interface breaks every custom store**
- [ ] **`weak`** (1.24) and **`unique`** (1.23): plausible for entry
      references and key interning; adopt only with measurements
- [ ] **Generic type aliases** (1.24): only if a concrete API need appears

## Verification gate

- [x] `go build`, `go vet`, and `go test -race -covermode=atomic` on 1.26.x and 1.27.x
- [x] `golangci-lint run ./...`, `gofmt -l .`, `actionlint`
- [x] README example harness: bump its module to `go 1.26`; released-API
      checks stay pinned to v0.3.2
- [x] Confirm zero goroutine leaks after `Close()`, under `-race`
- [x] Record before and after test wall-clock time
- [ ] All hosted PR checks green

## Release

- [ ] Tag `[minor]`, v0.4.0. The floor bump breaks consumers pinned with
      `GOTOOLCHAIN=local`; the default `GOTOOLCHAIN=auto` upgrades silently
- [x] Release notes: new minimum Go version, new `Close()`, and the bulk
      updater now being released

## Risks

1. **The `Close()` shutdown race is the highest-risk item**, not the version bump.
2. **Combined-PR review burden**: an API addition plus a full test rewrite in
   one diff. The commit split above is the mitigation.
3. Dropping support for Go 1.22 through 1.25 is immediate and irreversible for
    consumers on pinned toolchains.

## Execution notes

- Implemented on 2026-09-13. Shutdown decisions are in
  `adrs/001-cache-shutdown.md`; release notes are drafted in
  `release-notes-v0.4.0.md`. Changes are split into the five checkpoints above
  on `upgrade/go-1.26`, stacked on `maintenance` (PR #17). Each staged
  checkpoint passed build, vet, and uncached race tests before committing.
  Hosted PR checks and tagging remain pending.
- Build, vet, and uncached race/atomic-coverage tests passed with Go 1.26.7
  and 1.27.1. golangci-lint 2.13.2, actionlint 1.7.12, formatting, and
  `go fix -diff ./...` passed. Optional dependency upgrade and CI fix gate
  were left out.
- Baseline uncached race test: 31.757s package time, 31.98s command wall
  time. After migration: about 1.15s package time, including the race
  detector's exit delay and a 100ms real-clock smoke test. The first 1.26
  command included compilation (8.34s wall); the warm 1.27 command took 1.73s.
- The existing external README harness at
  `/tmp/opencode/check_pocache_readme.py` now uses `go 1.26`. All three
  local examples passed on both toolchains. Released API checks remain
  pinned to v0.3.2, omit the unreleased bulk example, and remove the new
  `defer cache.Close()` line when checking that release.
- The plan, ADR, and draft release notes are explicitly tracked despite
  the existing local exclusion of `docs/`.
- CI also accepts PRs targeting `maintenance` so the stacked upgrade gets
  build, race-test, and lint checks before PR #17 merges.
