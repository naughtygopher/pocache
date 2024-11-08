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
	ErrOnUpdate func(err error)

	// Updater defines the function which is used to get the new value
	// of a key. This is required for pocache to do background updates
	Updater[K comparable, T any] func(ctx context.Context, key K) (T, error)

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
	Store          Store[K, T]

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

type Cache[K comparable, T any] struct {
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
	updaterTimeout time.Duration
	// updateInProgress is used to handle update debounce
	updateInProgress *sync.Map
	errWatcher       ErrOnUpdate
}

// initUpdater initializes all configuration required for threshold based update
func (ch *Cache[K, T]) initUpdater(cfg *Config[K, T]) {
	if cfg.Updater == nil {
		return
	}

	ch.threshold = cfg.Threshold.Abs()
	updateQ := make(chan K, cfg.QLength)
	ch.updateQ = updateQ

	ch.updater = cfg.Updater
	ch.updaterTimeout = cfg.UpdaterTimeout
	ch.updateInProgress = new(sync.Map)
	ch.errWatcher = cfg.ErrWatcher

	go ch.updateListener(updateQ)
}

func (ch *Cache[K, T]) errCallback(err error) {
	if err == nil || ch.errWatcher == nil {
		return
	}

	ch.errWatcher(err)
}

func (ch *Cache[K, T]) enqueueUpdate(key K) {
	if ch.updater == nil {
		return
	}

	_, inprogress := ch.updateInProgress.Load(key)
	if inprogress {
		// key is already queued for update, no need to update again
		return
	}

	ch.updateInProgress.Store(key, struct{}{})
	ch.updateQ <- key
}

func (ch *Cache[K, T]) deleteListener(keys <-chan K) {
	for key := range keys {
		ch.store.Remove(key)
	}
}

func (ch *Cache[K, T]) updateListener(keys <-chan K) {
	for key := range keys {
		ch.update(key)
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

	ctx, cancel := context.WithTimeout(context.Background(), ch.updaterTimeout)
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

	if ch.isDisabled {
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
		ch.deleteQ <- key
		return v
	}

	if delta.Abs() <= ch.threshold {
		// key is eligible for update
		ch.enqueueUpdate(key)
	}

	v.Found = true
	v.V = cp.Payload

	return v
}

func (ch *Cache[K, T]) Add(key K, value T) (evicted bool) {
	if ch.isDisabled {
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
	ch := &Cache[K, T]{
		isDisabled:        cfg.DisableCache,
		disableServeStale: !cfg.ServeStale,
		store:             cstore,
		cacheAge:          cfg.CacheAge.Abs(),
		deleteQ:           deleteQ,
	}

	ch.initUpdater(&cfg)

	go ch.deleteListener(deleteQ)

	return ch, nil
}
