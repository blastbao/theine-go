package internal

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gammazero/deque"
)

const (
	MAX_READ_BUFF_SIZE  = 64
	MIN_WRITE_BUFF_SIZE = 4
	MAX_WRITE_BUFF_SIZE = 1024
)

type RemoveReason uint8

const (
	REMOVED RemoveReason = iota
	EVICTED
	EXPIRED
)

type Shard[K comparable, V any] struct {
	hashmap   map[K]*Entry[K, V]
	dookeeper *doorkeeper
	deque     *deque.Deque[*Entry[K, V]]
	group     Group[K, Loaded[V]]
	size      uint
	qsize     uint
	qlen      int				//
	counter   uint				// 布隆过滤器的非冲突元素数目
	mu        sync.RWMutex
}

func NewShard[K comparable, V any](size uint, qsize uint, doorkeeper bool) *Shard[K, V] {
	s := &Shard[K, V]{
		hashmap: make(map[K]*Entry[K, V]),
		size:    size,
		qsize:   qsize,
		deque:   deque.New[*Entry[K, V]](),
	}
	if doorkeeper {
		s.dookeeper = newDoorkeeper(0.01)
	}
	return s
}

func (s *Shard[K, V]) set(key K, entry *Entry[K, V]) {
	s.hashmap[key] = entry
	if s.dookeeper != nil {
		ds := 20 * len(s.hashmap)
		if ds > s.dookeeper.capacity {
			s.dookeeper.ensureCapacity(ds)
		}
	}
}

func (s *Shard[K, V]) get(key K) (entry *Entry[K, V], ok bool) {
	entry, ok = s.hashmap[key]
	return
}

func (s *Shard[K, V]) delete(entry *Entry[K, V]) bool {
	var deleted bool
	exist, ok := s.hashmap[entry.key]
	if ok && exist == entry {
		delete(s.hashmap, exist.key)
		deleted = true
	}
	return deleted
}

func (s *Shard[K, V]) len() int {
	return len(s.hashmap)
}

type Metrics struct {
}

type Store[K comparable, V any] struct {
	entryPool       sync.Pool
	writebuf        chan WriteBufItem[K, V]
	hasher          *Hasher[K]
	removalListener func(key K, value V, reason RemoveReason)
	policy          *TinyLfu[K, V]
	timerwheel      *TimerWheel[K, V]
	readbuf         *Queue[ReadBufItem[K, V]]
	cost            func(V) int64
	readCounter     *atomic.Uint32
	shards          []*Shard[K, V]
	cap             uint
	shardCount      uint
	mlock           sync.Mutex
	tailUpdate      bool
	doorkeeper      bool
	closed          bool
}

// New returns a new data struct with the specified capacity
func NewStore[K comparable, V any](maxsize int64, doorkeeper bool) *Store[K, V] {
	hasher := NewHasher[K]()
	writeBufSize := maxsize / 100
	if writeBufSize < MIN_WRITE_BUFF_SIZE {
		writeBufSize = MIN_WRITE_BUFF_SIZE
	}
	if writeBufSize > MAX_WRITE_BUFF_SIZE {
		writeBufSize = MAX_WRITE_BUFF_SIZE
	}
	shardCount := 1
	for shardCount < runtime.GOMAXPROCS(0)*2 {
		shardCount *= 2
	}
	if shardCount < 16 {
		shardCount = 16
	}
	if shardCount > 128 {
		shardCount = 128
	}
	dequeSize := int(maxsize) / 100 / shardCount
	shardSize := int(maxsize) / shardCount
	if shardSize < 50 {
		shardSize = 50
	}
	policySize := int(maxsize) - (dequeSize * shardCount)
	s := &Store[K, V]{
		cap:         uint(maxsize),
		hasher:      hasher,
		policy:      NewTinyLfu[K, V](uint(policySize), hasher),
		readCounter: &atomic.Uint32{},
		readbuf:     NewQueue[ReadBufItem[K, V]](),
		writebuf:    make(chan WriteBufItem[K, V], writeBufSize),
		entryPool:   sync.Pool{New: func() any { return &Entry[K, V]{} }},
		cost:        func(v V) int64 { return 1 },
		shardCount:  uint(shardCount),
		doorkeeper:  doorkeeper,
	}
	s.shards = make([]*Shard[K, V], 0, s.shardCount)
	for i := 0; i < int(s.shardCount); i++ {
		s.shards = append(s.shards, NewShard[K, V](uint(shardSize), uint(dequeSize), doorkeeper))
	}

	s.timerwheel = NewTimerWheel[K, V](uint(maxsize))
	go s.maintance()
	return s
}

func (s *Store[K, V]) Cost(cost func(v V) int64) {
	s.cost = cost
}

func (s *Store[K, V]) RemovalListener(listener func(key K, value V, reason RemoveReason)) {
	s.removalListener = listener
}

func (s *Store[K, V]) getFromShard(key K, hash uint64, shard *Shard[K, V]) (V, bool) {
	new := s.readCounter.Add(1)
	shard.mu.RLock()
	entry, ok := shard.get(key)
	var value V
	if ok {
		expire := entry.expire.Load()
		if expire != 0 && expire <= s.timerwheel.clock.nowNano() {
			ok = false
		} else {
			s.policy.hit.Add(1)
			value = entry.value
		}
	}
	shard.mu.RUnlock()
	switch {
	case new < MAX_READ_BUFF_SIZE:
		var send ReadBufItem[K, V]
		send.hash = hash
		if ok {
			send.entry = entry
		}
		s.readbuf.Push(send)
	case new == MAX_READ_BUFF_SIZE:
		s.drainRead()
	}
	return value, ok
}

func (s *Store[K, V]) Get(key K) (V, bool) {
	h, index := s.index(key)
	shard := s.shards[index]
	return s.getFromShard(key, h, shard)
}

func (s *Store[K, V]) Set(key K, value V, cost int64, ttl time.Duration) bool {
	// 计算字节数
	if cost == 0 {
		cost = s.cost(value)
	}
	if cost > int64(s.cap) {
		return false
	}

	// 定位分片
	h, index := s.index(key)
	shard := s.shards[index]

	// 计算超时时间（绝对时间）
	var expire int64
	if ttl != 0 {
		expire = s.timerwheel.clock.expireNano(ttl)
	}

	shard.mu.Lock()
	exist, ok := shard.get(key)
	// 已存在，更新
	if ok {
		var reschedule bool
		var costChange int64
		exist.value = value

		// 更新 cost
		oldCost := exist.cost.Swap(cost)
		if oldCost != cost {
			costChange = cost - oldCost
			if exist.deque {
				shard.qlen += int(costChange)
			}
		}

		// 更新 ttl
		shard.mu.Unlock()
		if expire > 0 {
			old := exist.expire.Swap(expire)
			if old != expire {
				reschedule = true
			}
		}

		// ?
		if reschedule || costChange != 0 {
			s.writebuf <- WriteBufItem[K, V]{
				entry: exist, code: UPDATE, costChange: costChange, rechedule: reschedule,
			}
		}

		return true
	}

	// 布隆过滤器
	if s.doorkeeper {
		// 布隆过滤器容量不足，误差升高，则重置
		if shard.counter > uint(shard.dookeeper.capacity) {
			shard.dookeeper.reset()
			shard.counter = 0
		}
		// 插入布隆过滤器
		hit := shard.dookeeper.insert(h)
		if !hit {
			shard.counter += 1 // 更新布隆过滤器的非冲突元素计数
			shard.mu.Unlock()
			return false
		}
	}

	// 新建 Entry 并初始化
	entry := s.entryPool.Get().(*Entry[K, V])
	entry.frequency.Store(-1)
	entry.shard = uint16(index)
	entry.key = key
	entry.value = value
	entry.expire.Store(expire)
	entry.cost.Store(cost)

	// 插入到 shard
	shard.set(key, entry)

	// cost larger than deque size, send to policy directly
	if cost > int64(shard.qsize) {
		shard.mu.Unlock()
		s.writebuf <- WriteBufItem[K, V]{entry: entry, code: NEW}
		return true
	}

	entry.deque = true
	shard.deque.PushFront(entry)
	shard.qlen += int(cost)
	s.processDeque(shard)
	return true
}

type dequeKV[K comparable, V any] struct {
	k K
	v V
}

func (s *Store[K, V]) processDeque(shard *Shard[K, V]) {
	if shard.qlen <= int(shard.qsize) {
		shard.mu.Unlock()
		return
	}
	// send to slru
	send := make([]*Entry[K, V], 0, 2)
	// removed because frequency < slru tail frequency
	removedkv := make([]dequeKV[K, V], 0, 2)
	// expired
	expiredkv := make([]dequeKV[K, V], 0, 2)
	// expired
	for shard.qlen > int(shard.qsize) {
		evicted := shard.deque.PopBack()
		evicted.deque = false
		expire := evicted.expire.Load()
		shard.qlen -= int(evicted.cost.Load())
		if expire != 0 && expire <= s.timerwheel.clock.nowNano() {
			deleted := shard.delete(evicted)
			// double check because entry maybe removed already by Delete API
			if deleted {
				expiredkv = append(expiredkv, dequeKV[K, V]{k: evicted.key, v: evicted.value})
				s.postDelete(evicted)
			}
		} else {
			count := evicted.frequency.Load()
			threshold := s.policy.threshold.Load()
			if count == -1 {
				send = append(send, evicted)
			} else {
				if int32(count) >= threshold {
					send = append(send, evicted)
				} else {
					deleted := shard.delete(evicted)
					// double check because entry maybe removed already by Delete API
					if deleted {
						removedkv = append(
							expiredkv, dequeKV[K, V]{k: evicted.key, v: evicted.value},
						)
						s.postDelete(evicted)
					}
				}
			}
		}
	}
	shard.mu.Unlock()
	for _, entry := range send {
		s.writebuf <- WriteBufItem[K, V]{entry: entry, code: NEW}
	}
	if s.removalListener != nil {
		for _, e := range removedkv {
			s.removalListener(e.k, e.v, EVICTED)
		}
		for _, e := range expiredkv {
			s.removalListener(e.k, e.v, EXPIRED)
		}
	}
}

func (s *Store[K, V]) Delete(key K) {
	_, index := s.index(key)
	shard := s.shards[index]
	shard.mu.Lock()
	entry, ok := shard.get(key)
	if ok {
		shard.delete(entry)
	}
	shard.mu.Unlock()
	if ok {
		s.writebuf <- WriteBufItem[K, V]{entry: entry, code: REMOVE}
	}
}

func (s *Store[K, V]) Len() int {
	total := 0
	for _, s := range s.shards {
		s.mu.RLock()
		total += s.len()
		s.mu.RUnlock()
	}
	return total
}

// spread hash before get index
func (s *Store[K, V]) index(key K) (uint64, int) {
	base := s.hasher.hash(key)
	h := ((base >> 16) ^ base) * 0x45d9f3b
	h = ((h >> 16) ^ h) * 0x45d9f3b
	h = (h >> 16) ^ h
	return base, int(h & uint64(s.shardCount-1))
}

func (s *Store[K, V]) postDelete(entry *Entry[K, V]) {
	var zero V
	entry.value = zero
	s.entryPool.Put(entry)
}

// remove entry from cache/policy/timingwheel and add back to pool
func (s *Store[K, V]) removeEntry(entry *Entry[K, V], reason RemoveReason) {
	if prev := entry.meta.prev; prev != nil {
		s.policy.Remove(entry)
	}
	if entry.meta.wheelPrev != nil {
		s.timerwheel.deschedule(entry)
	}
	var k K
	var v V
	switch reason {
	case EVICTED, EXPIRED:
		shard := s.shards[entry.shard]
		shard.mu.Lock()
		deleted := shard.delete(entry)
		shard.mu.Unlock()
		if deleted {
			k, v = entry.key, entry.value
			if s.removalListener != nil {
				s.removalListener(k, v, reason)
			}
			s.postDelete(entry)
		}
	// already removed from shard map
	case REMOVED:
		shard := s.shards[entry.shard]
		shard.mu.RLock()
		k, v = entry.key, entry.value
		shard.mu.RUnlock()
		if s.removalListener != nil {
			s.removalListener(k, v, reason)
		}
	}
}

func (s *Store[K, V]) drainRead() {
	s.policy.total.Add(MAX_READ_BUFF_SIZE)
	s.mlock.Lock()
	for {
		v, ok := s.readbuf.Pop()
		if !ok {
			break
		}
		s.policy.Access(v)
	}
	s.mlock.Unlock()
	s.readCounter.Store(0)
}

func (s *Store[K, V]) maintance() {
	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			s.mlock.Lock()
			if s.closed {
				s.mlock.Unlock()
				return
			}
			s.timerwheel.advance(0, s.removeEntry)
			s.policy.UpdateThreshold()
			s.mlock.Unlock()
		}
	}()

	for item := range s.writebuf {
		s.mlock.Lock()
		entry := item.entry
		if entry == nil {
			s.mlock.Unlock()
			continue
		}

		// lock free because store API never read/modify entry metadata
		switch item.code {
		case NEW:
			if entry.removed {
				s.mlock.Unlock()
				continue
			}
			if entry.expire.Load() != 0 {
				s.timerwheel.schedule(entry)
			}
			evicted := s.policy.Set(entry)
			if evicted != nil {
				s.removeEntry(evicted, EVICTED)
				s.tailUpdate = true
			}
			removed := s.policy.EvictEntries()
			for _, e := range removed {
				s.tailUpdate = true
				s.removeEntry(e, EVICTED)
			}
		case REMOVE:
			entry.removed = true
			s.removeEntry(entry, REMOVED)
			s.policy.threshold.Store(-1)
		case UPDATE:
			if item.rechedule {
				s.timerwheel.schedule(entry)
			}
			if item.costChange != 0 {
				s.policy.UpdateCost(entry, item.costChange)
				removed := s.policy.EvictEntries()
				for _, e := range removed {
					s.tailUpdate = true
					s.removeEntry(e, EVICTED)
				}
			}
		}
		item.entry = nil
		if s.tailUpdate {
			s.policy.UpdateThreshold()
			s.tailUpdate = false
		}
		s.mlock.Unlock()
	}
}

func (s *Store[K, V]) Range(f func(key K, value V) bool) {
	now := s.timerwheel.clock.nowNano()
	for _, shard := range s.shards {
		shard.mu.RLock()
		for _, entry := range shard.hashmap {
			expire := entry.expire.Load()
			if expire != 0 && expire <= now {
				continue
			}
			if !f(entry.key, entry.value) {
				shard.mu.RUnlock()
				return
			}
		}
		shard.mu.RUnlock()
	}
}

func (s *Store[K, V]) Close() {
	for _, s := range s.shards {
		s.mu.RLock()
		s.hashmap = nil
		s.mu.RUnlock()
	}
	s.mlock.Lock()
	s.closed = true
	s.mlock.Unlock()
	close(s.writebuf)
}

type Loaded[V any] struct {
	Value V
	Cost  int64
	TTL   time.Duration
}

type LoadingStore[K comparable, V any] struct {
	loader func(ctx context.Context, key K) (Loaded[V], error)
	*Store[K, V]
}

func NewLoadingStore[K comparable, V any](store *Store[K, V]) *LoadingStore[K, V] {
	return &LoadingStore[K, V]{
		Store: store,
	}
}

func (s *LoadingStore[K, V]) Loader(loader func(ctx context.Context, key K) (Loaded[V], error)) {
	s.loader = loader
}

func (s *LoadingStore[K, V]) Get(ctx context.Context, key K) (V, error) {
	h, index := s.index(key)
	shard := s.shards[index]
	v, ok := s.getFromShard(key, h, shard)
	if !ok {
		loaded, err, _ := shard.group.Do(key, func() (Loaded[V], error) {
			loaded, err := s.loader(ctx, key)
			if err == nil {
				s.Set(key, loaded.Value, loaded.Cost, loaded.TTL)
			}
			return loaded, err
		})
		return loaded.Value, err
	}
	return v, nil
}
