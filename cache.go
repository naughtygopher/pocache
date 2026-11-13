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
	"github.com/naughtygopher/nibbler"
)

var (
	ErrValidation = errors.New("invalid")
	ErrPanic      = errors.New("panicked")
	// ErrBulkUpdate wraps the error returned by a BulkUpdater
	ErrBulkUpdate = errors.New("bulk update failed")
)

type (
	// ErrOnUpdate defines the type of the hook function, which is called
	// if there's any error when trying to update a key in the background
	// This function should not block the goroutine for too long
	ErrOnUpdate func(err error)

	// Updater defines the function which is used to get the new value
	// of a key. This is required for pocache to do background updates
	Updater[K comparable, T any] func(ctx context.Context, key K) (T, error)

	// BulkUpdater gets the new values of multiple keys at the same time, e.g. using a
	// single database query. It returns the values by key. Keys missing from the returned
	// map are not updated, and are retried on a later `Get`. Keys in the map which were
	// not requested are ignored. If it returns an error, the values returned alongside it
	// are still stored, and the error is reported to ErrWatcher wrapped in ErrBulkUpdate.
	// The keys slice is reused after the function returns, it must not be modified or retained.
	BulkUpdater[K comparable, T any] func(ctx context.Context, keys []K) (map[K]T, error)

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

	// BulkUpdater is an option to refresh multiple keys at a time. It is mutually exclusive
	// with Updater, setting both is a validation error. If a key is not updated by it, its update
	// will happen next time there is another `Get` on that key, and the `Get` function may return
	// stale data (depends on the config ServeStale) or a cache miss.
	BulkUpdater BulkUpdater[K, T]
	// BulkSize is the maximum number of keys sent to the BulkUpdater in a single call. If 0, defaults to 100
	BulkSize uint
	// BulkMaxWait is the longest a key waits in the queue before its batch is sent to the BulkUpdater,
	// even if the batch is not full. If less than 1ms, defaults to 15ms
	BulkMaxWait time.Duration

	Store Store[K, T]

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

	if cfg.BulkSize == 0 {
		cfg.BulkSize = 100
	}

	if cfg.BulkMaxWait < time.Millisecond {
		cfg.BulkMaxWait = 15 * time.Millisecond
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

	if cfg.Updater != nil && cfg.BulkUpdater != nil {
		return errors.Join(
			ErrValidation,
			fmt.Errorf("updater and bulk updater are mutually exclusive"),
		)
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
	threshold   time.Duration
	updateQ     chan<- K
	updater     Updater[K, T]
	bulkUpdater BulkUpdater[K, T]
	// updateInProgress is used to handle update debounce
	updateInProgress *sync.Map
	errWatcher       ErrOnUpdate
}

// initUpdater initializes all configuration required for threshold based update.
// Both Updater and BulkUpdater are served by a nibbler micro batch processor, a single key
// Updater is a batch of size 1.
func (ch *Cache[K, T]) initUpdater(cfg *Config[K, T]) error {
	if cfg.Updater == nil && cfg.BulkUpdater == nil {
		return nil
	}

	ch.threshold = cfg.Threshold.Abs()
	ch.updater = cfg.Updater
	ch.bulkUpdater = cfg.BulkUpdater
	ch.updateInProgress = new(sync.Map)
	ch.errWatcher = cfg.ErrWatcher

	// The processors recover panics and report errors by themselves, to release the keys of
	// the failed batch. Pending keys are dropped on Close, so FlushOnStop is left disabled.
	ncfg := &nibbler.Config[K]{
		ProcessingTimeout: cfg.UpdaterTimeout,
		QueueSize:         cfg.QLength,
		Size:              1,
		Processor:         ch.processUpdate,
		ResumeAfterErr:    true,
	}
	if ch.bulkUpdater != nil {
		ncfg.Size = cfg.BulkSize
		ncfg.TickerDuration = cfg.BulkMaxWait
		ncfg.Processor = ch.processBulkUpdate
	}

	nib, err := nibbler.New(ncfg)
	if err != nil {
		return fmt.Errorf("failed initializing update processor: %w", err)
	}

	ch.updateQ = nib.Receiver()
	ch.workers.Go(func() { _ = nib.Listen(ch.ctx) })

	return nil
}

func (ch *Cache[K, T]) errCallback(err error) {
	if err == nil || ch.errWatcher == nil {
		return
	}

	ch.errWatcher(err)
}

func (ch *Cache[K, T]) panicCallback(rec any) {
	err, isErr := rec.(error)
	if !isErr {
		err = fmt.Errorf("%+v", rec)
	}

	ch.errCallback(errors.Join(ErrPanic, err))
}

// enqueueUpdate never blocks. If the update queue is full, the update is skipped and the
// key becomes eligible again on a later `Get`.
func (ch *Cache[K, T]) enqueueUpdate(key K) {
	if ch.updateQ == nil || ch.ctx.Err() != nil {
		return
	}

	_, inprogress := ch.updateInProgress.LoadOrStore(key, struct{}{})
	if inprogress {
		// key is already queued for update, no need to update again
		return
	}

	select {
	case ch.updateQ <- key:
	default:
		ch.updateInProgress.Delete(key)
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

// processUpdate is the nibbler processor for the single key Updater, the batch always has 1 key.
// It always returns nil, as errors and panics are reported to ErrWatcher.
func (ch *Cache[K, T]) processUpdate(ctx context.Context, _ nibbler.Trigger, keys []K) error {
	key := keys[0]
	defer func() {
		ch.updateInProgress.Delete(key)
		if rec := recover(); rec != nil {
			ch.panicCallback(rec)
		}
	}()

	value, err := ch.updater(ctx, key)
	if err != nil {
		ch.errCallback(err)
		return nil
	}

	ch.Add(key, value)
	return nil
}

// processBulkUpdate is the nibbler processor for the BulkUpdater. It always returns nil, as
// errors and panics are reported to ErrWatcher. The keys are released even if the BulkUpdater panics.
func (ch *Cache[K, T]) processBulkUpdate(ctx context.Context, _ nibbler.Trigger, keys []K) error {
	defer func() {
		for _, key := range keys {
			ch.updateInProgress.Delete(key)
		}
		if rec := recover(); rec != nil {
			ch.panicCallback(rec)
		}
	}()

	values, err := ch.bulkUpdater(ctx, keys)
	// looking up by the requested keys ignores unrequested keys in values
	for _, key := range keys {
		value, found := values[key]
		if found {
			ch.Add(key, value)
		}
	}

	if err != nil {
		ch.errCallback(errors.Join(ErrBulkUpdate, err))
	}
	return nil
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
		// cache expired and should be removed. If the delete queue is full the removal is
		// skipped, the key is removed on a later `Get` or by the store's eviction
		select {
		case ch.deleteQ <- key:
		default:
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

	err = ch.initUpdater(&cfg)
	if err != nil {
		cancel()
		return nil, err
	}

	ch.workers.Go(func() { ch.deleteListener(deleteQ) })

	return ch, nil
}
