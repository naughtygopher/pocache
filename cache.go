// Package pocache implements an in-memory, LRU cache, with preemptive update feature.
package pocache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

var (
	ErrValidation = errors.New("invalid")
	ErrPanic      = errors.New("panicked")
)

type (
	// ErrOnUpdate defines the type of the hook function, which is called
	// if there's any error when trying to update a key in the background
	// This function should not block the goroutine for too long
	ErrOnUpdate func(err error)

	// Updater defines the function which is used to get the new value
	// of a key. This is required for pocache to do background updates
	Updater[K comparable, T any] func(ctx context.Context, key K) (T, error)

	// This function MUST return a slice with the same length as they number of keys
	// received, else there is no way to tell which value belongs to which original key
	// Update multiple expired keys at the same time
	// This allows better parallelism for cache invalidation
	BulkUpdater[K comparable, T any] func(ctx context.Context, keys []K) []UpdateResult[T]

	UpdateResult[T any] struct {
		NewValue T
		Err      error
	}

	// Store defines the interface required for the underlying storage of pocache.
	Store[K comparable, T any] interface {
		Add(key K, value *Payload[T]) (evicted bool)
		Get(key K) (value *Payload[T], found bool)
		Remove(key K) (present bool)
	}
)

type Config[K comparable, T any] struct {
	// LRUCacheSize is the number of keys to be maintained in the cache
	LRUCacheSize uint
	// QLength is the length of update and delete queue
	QLength uint

	// CacheAge is for how long the cache would be maintained, apart from the LRU eviction
	// It's maintained to not maintain stale data if/when keys are not evicted based on LRU
	CacheAge time.Duration
	// Threshold is the duration prior to expiry, when the key is considered eligible to be updated
	Threshold    time.Duration
	DisableCache bool

	// ServeStale will not return error if the cache has expired. It will return the stale
	// value, and trigger an update as well. This is useful for usecases where it's ok
	// to serve stale values and data consistency is not of paramount importance
	ServeStale bool

	// UpdaterTimeout is the context time out for when the updater function is called
	UpdaterTimeout time.Duration
	Updater        Updater[K, T]

	// An option to refresh multiple expired keys at a time
	// If any of the item failed to update inside this bulk function, its update will happen
	// next time there is another `Get`` on that key, and the `Get`` function may returns a stale
	// data (depends on the config ServeStale) or a cache miss.
	BulkUpdater BulkUpdater[K, T]
	Store       Store[K, T]

	// ErrWatcher is called when there's any error when trying to update cache
	ErrWatcher ErrOnUpdate
}

func (cfg *Config[K, T]) Sanitize() {
	if cfg.LRUCacheSize == 0 {
		cfg.LRUCacheSize = 1000
	}

	if cfg.QLength == 0 {
		cfg.QLength = 1000
	}

	if cfg.CacheAge <= 0 {
		cfg.CacheAge = time.Minute
	}

	if cfg.Threshold <= 0 {
		cfg.Threshold = cfg.CacheAge - time.Second
	}

	if cfg.UpdaterTimeout <= 0 {
		cfg.UpdaterTimeout = time.Second
	}
}

func (cfg *Config[K, T]) Validate() error {
	if cfg.LRUCacheSize == 0 {
		return errors.Join(
			ErrValidation,
			fmt.Errorf("LRU cache size cannot be 0"),
		)
	}

	if cfg.CacheAge <= cfg.Threshold {
		return errors.Join(
			ErrValidation,
			fmt.Errorf(
				"cache age %s cannot be shorter than threshold %s",
				cfg.CacheAge,
				cfg.Threshold,
			))
	}

	return nil
}

func (cfg *Config[K, T]) SanitizeValidate() error {
	cfg.Sanitize()
	return cfg.Validate()
}

type Payload[T any] struct {
	// ExpireAt is an atomic pointer to avoid race condition
	// while concurrently reading the timestamp
	ExpireAt *atomic.Pointer[time.Time]
	Payload  T
}

func (pyl *Payload[T]) Expiry() time.Time {
	if pyl.ExpireAt == nil {
		return time.Time{}
	}

	return *pyl.ExpireAt.Load()
}

func (pyl *Payload[T]) Value() T {
	return pyl.Payload
}

type Tuple[K comparable, T any] struct {
	Key   K
	Value T
}

type Value[T any] struct {
	V     T
	Found bool
}

// Cache is safe for concurrent use when its Store supports concurrent access.
// A Cache must not be copied after construction. Call Close to stop its workers.
type Cache[K comparable, T any] struct {
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup

	isDisabled        bool
	disableServeStale bool
	store             Store[K, T]
	cacheAge          time.Duration

	deleteQ chan<- K

	// following configurations are used only when an updater & threshold update are enabled
	// threshold is the duration within which if the cache is about to expire, it is eligible to be updated
	threshold      time.Duration
	updateQ        chan<- K
	updater        Updater[K, T]
	bulkUpdater    BulkUpdater[K, T]
	updaterTimeout time.Duration
	// updateInProgress is used to handle update debounce
	updateInProgress *sync.Map
	errWatcher       ErrOnUpdate
}

// initUpdater initializes all configuration required for threshold based update
func (ch *Cache[K, T]) initUpdater(cfg *Config[K, T]) {
	if cfg.Updater == nil && cfg.BulkUpdater == nil {
		return
	}

	ch.threshold = cfg.Threshold.Abs()
	updateQ := make(chan K, cfg.QLength)
	ch.updateQ = updateQ

	ch.updater = cfg.Updater
	ch.bulkUpdater = cfg.BulkUpdater
	ch.updaterTimeout = cfg.UpdaterTimeout
	ch.updateInProgress = new(sync.Map)
	ch.errWatcher = cfg.ErrWatcher

	ch.workers.Go(func() { ch.updateListener(updateQ) })
}

func (ch *Cache[K, T]) errCallback(err error) {
	if err == nil || ch.errWatcher == nil {
		return
	}

	ch.errWatcher(err)
}

func (ch *Cache[K, T]) enqueueUpdate(key K) {
	if ch.updater == nil && ch.bulkUpdater == nil {
		return
	}

	if ch.ctx.Err() != nil {
		return
	}
	_, inprogress := ch.updateInProgress.LoadOrStore(key, struct{}{})
	if inprogress {
		// key is already queued for update, no need to update again
		return
	}

	select {
	case <-ch.ctx.Done():
		ch.updateInProgress.Delete(key)
	case ch.updateQ <- key:
	}
}

func (ch *Cache[K, T]) deleteListener(keys <-chan K) {
	for {
		select {
		case <-ch.ctx.Done():
			return
		case key := <-keys:
			if ch.ctx.Err() != nil {
				return
			}
			ch.store.Remove(key)
		}
	}
}

func (ch *Cache[K, T]) updateListener(keys <-chan K) {
	if ch.updater != nil {
		for {
			select {
			case <-ch.ctx.Done():
				return
			case key := <-keys:
				if ch.ctx.Err() != nil {
					return
				}
				ch.update(key)
			}
		}
	}
	if ch.bulkUpdater != nil {
		batchTicker := time.NewTicker(15 * time.Millisecond)
		defer batchTicker.Stop()

		batched := make([]K, 0, cap(keys))
		for {
			select {
			case <-ch.ctx.Done():
				return
			case <-batchTicker.C:
			}
			for len(keys) > 0 {
				if ch.ctx.Err() != nil {
					return
				}
				// Snapshot the queue length; reading keys shrinks it.
				for range len(keys) {
					batched = append(batched, <-keys)
				}
				ch.bulkUpdate(batched)
				clear(batched)
				batched = batched[:0]
			}
		}
	}

}
func (ch *Cache[K, T]) bulkUpdate(keys []K) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		for _, k := range keys {
			ch.updateInProgress.Delete(k)
		}
		err, isErr := rec.(error)
		if isErr {
			ch.errCallback(errors.Join(ErrPanic, err))
			return
		}
		ch.errCallback(errors.Join(ErrPanic, fmt.Errorf("%+v", rec)))
	}()

	ctx, cancel := context.WithTimeout(ch.ctx, ch.updaterTimeout)
	defer cancel()

	updateResults := ch.bulkUpdater(ctx, keys)

	// TODO: not sure if there is a better error handling here
	// since user does not conform with the function spec, there is no way to tell
	// which value belongs to which key:w:w
	if len(updateResults) != len(keys) {
		panic(fmt.Sprintf("BulkUpdator returns a slice with length not equal" +
			" to the original keys provided"))
	}
	for idx := range keys {
		updateResult := updateResults[idx]
		if updateResult.Err != nil {
			ch.errCallback(updateResult.Err)
			continue
		}
		ch.Add(keys[idx], updateResult.NewValue)
	}

	for _, k := range keys {
		ch.updateInProgress.Delete(k)
	}
}

func (ch *Cache[K, T]) update(key K) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		ch.updateInProgress.Delete(key)
		err, isErr := rec.(error)
		if isErr {
			ch.errCallback(errors.Join(ErrPanic, err))
			return
		}
		ch.errCallback(errors.Join(ErrPanic, fmt.Errorf("%+v", rec)))
	}()

	ctx, cancel := context.WithTimeout(ch.ctx, ch.updaterTimeout)
	defer cancel()

	value, err := ch.updater(ctx, key)
	ch.updateInProgress.Delete(key)
	if err != nil {
		ch.errCallback(err)
		return
	}

	ch.Add(key, value)
}

func (ch *Cache[K, T]) Get(key K) Value[T] {
	var v Value[T]

	if ch.isDisabled || ch.ctx.Err() != nil {
		return v
	}

	cp, found := ch.store.Get(key)
	if !found {
		return v
	}

	expireAt := cp.ExpireAt.Load()
	delta := time.Since(*expireAt)
	if delta >= 0 && ch.disableServeStale {
		// cache expired and should be removed
		select {
		case <-ch.ctx.Done():
		case ch.deleteQ <- key:
		}
		return v
	}

	inTreshold := delta < 0 && delta.Abs() <= ch.threshold
	expired := delta >= 0
	if inTreshold || expired {
		// key is eligible for update
		ch.enqueueUpdate(key)
	}

	v.Found = true
	v.V = cp.Payload

	return v
}

func (ch *Cache[K, T]) Add(key K, value T) (evicted bool) {
	if ch.isDisabled || ch.ctx.Err() != nil {
		return false
	}

	expireAt := time.Now().Add(ch.cacheAge)
	cea := atomic.Pointer[time.Time]{}
	cea.Store(&expireAt)

	return ch.store.Add(key, &Payload[T]{
		ExpireAt: &cea,
		Payload:  value,
	})
}

func (ch *Cache[K, T]) BulkAdd(tuples []Tuple[K, T]) (evicted []bool) {
	evicted = make([]bool, len(tuples))
	for i, tuple := range tuples {
		evicted[i] = ch.Add(tuple.Key, tuple.Value)
	}

	return evicted
}

// Close cancels active updater contexts, abandons queued work, and waits for
// background workers to exit. It is safe to call concurrently and repeatedly.
// After shutdown begins, Get returns a miss and Add is a no-op; calls already
// in progress may finish. The store is retained and is not closed or cleared.
// Updaters must honor context cancellation, and store methods and ErrWatcher
// must return for Close to finish. They must not call Close on this cache.
func (ch *Cache[K, T]) Close() {
	ch.cancel()
	ch.workers.Wait()
	if ch.updateInProgress != nil {
		ch.updateInProgress.Clear()
	}
}

func DefaultStore[K comparable, T any](lrusize int) (Store[K, T], error) {
	lCache, err := lru.New[K, *Payload[T]](int(lrusize))
	if err != nil {
		return nil, fmt.Errorf("failed initializing LRU cache: %w", err)
	}
	return lCache, nil
}

func New[K comparable, T any](cfg Config[K, T]) (*Cache[K, T], error) {
	err := cfg.SanitizeValidate()
	if err != nil {
		return nil, err
	}

	cstore := cfg.Store
	if cstore == nil {
		cstore, err = DefaultStore[K, T](int(cfg.LRUCacheSize))
		if err != nil {
			return nil, err
		}
	}

	deleteQ := make(chan K, cfg.QLength)
	ctx, cancel := context.WithCancel(context.Background())
	ch := &Cache[K, T]{
		ctx:               ctx,
		cancel:            cancel,
		isDisabled:        cfg.DisableCache,
		disableServeStale: !cfg.ServeStale,
		store:             cstore,
		cacheAge:          cfg.CacheAge.Abs(),
		deleteQ:           deleteQ,
	}

	ch.initUpdater(&cfg)

	ch.workers.Go(func() { ch.deleteListener(deleteQ) })

	return ch, nil
}
