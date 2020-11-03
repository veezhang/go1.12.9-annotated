// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build ignore

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
// Pool 是一个临时对象池。一句话来概括，sync.Pool 管理了一组临时对象， 当需要时从池中获取，使用完毕后从再放回池中，以供他人使用。
// Pool 中存储的任何 item 在任何时间都可能没有通知就被移除。
type Pool struct {
	noCopy noCopy

	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal 	// local 固定大小 per-P 数组, 实际类型为 [P]poolLocal
	localSize uintptr        // size of the local array										// local array 的大小

	victim     unsafe.Pointer // local from previous cycle	// 来自前一个周期的 local
	victimSize uintptr        // size of victims array		// victim 数组的大小

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	// New 可选生产函数
	New func() interface{}
}

// Local per-P Pool appendix.
// 每个 P 的本地的
type poolLocalInternal struct {
	private interface{} // Can be used only by the respective P.				// 只能被各自的 P 使用。没有并发读写
	shared  poolChain   // Local P can pushHead/popHead; any P can popTail.  	// Local P 可以进行 pushHead/popHead 操作; 任何 P 都可以进行 popTail
}

// poolLocal 本地缓存池，防止伪共享
type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	// 防止伪共享
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrand() uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x interface{}) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
// Put 将 x 放回到 pool 中
func (p *Pool) Put(x interface{}) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrand()%4 == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	// 获取一个 poolLocal
	l, _ := p.pin()
	// 优先放入 private
	if l.private == nil {
		l.private = x
		x = nil
	}
	// 如果不能放入 private 则放入 shared
	if x != nil {
		l.shared.pushHead(x)
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
// Get 从 Pool 中选择一个任意的对象，将其移出 Pool, 并返回给调用方。
// Get 可能会返回一个非零值对象（被其他人使用过），因此调用方不应假设返回的对象具有任何形式的状态.
func (p *Pool) Get() interface{} {
	if race.Enabled {
		race.Disable()
	}
	// 获取一个 poolLocal
	l, pid := p.pin()
	// 先从 private 选择
	x := l.private
	l.private = nil
	if x == nil {
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		// 尝试从 poolLocal 的 shared 队列队头读取，因为队头的内存局部性比队尾更好。
		x, _ = l.shared.popHead()
		// 如果取不到，则获取新的缓存对象
		if x == nil {
			x = p.getSlow(pid)
		}
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}
	// 如果 getSlow 还是获取不到，则 New 一个
	if x == nil && p.New != nil {
		x = p.New()
	}
	return x
}

func (p *Pool) getSlow(pid int) interface{} {
	// See the comment in pin regarding ordering of the loads.
	size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                            // load-consume
	// Try to steal one element from other procs.
	// 从其他 proc (poolLocal) 偷一个对象
	for i := 0; i < int(size); i++ {
		// 从其他的 poolLocal 中获取, 引入 pid 保证不是自身
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	// 尝试 victim 缓存。在所有的主要缓存中获取后，才尝试这个，因为我们希望 victim 缓存经常能的老化。
	size = atomic.LoadUintptr(&p.victimSize)
	// 如果 pid >= size，表示 victim 中没有 pid 对应的 poolLocal ，返回 nil
	if uintptr(pid) >= size {
		return nil
	}
	locals = p.victim
	// 获取 poolLocal
	l := indexLocal(locals, pid)
	// 先从 private 中找
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	// 从其他的 poolLocal 中获取
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	// 没有找到，则 victimSize 设置为 0 ，之后再次访问，就可以快速直接返回了
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
// pin 会将当前的 goroutine 固定到 P 上，禁用抢占，并返回 localPool 池以及当前 P 的 pid。
func (p *Pool) pin() (*poolLocal, int) {
	// 禁止抢占，返回当前 P.id
	pid := runtime_procPin()
	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	// 在 pinSlow 中会存储 local，后再存储 localSize，因此这里反过来读取。因为我们已经禁用了抢占，这时不会发生 GC。
	// 因此这里， local 至少和 localSize 一样大。
	s := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                              // load-consume
	// 因为可能存在动态的 P（运行时调整 P 的个数）procresize/GOMAXPROCS
	// 如果 P.id 没有越界，则直接返回
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	// 没有结果时，涉及全局加锁。例如重新分配数组内存，添加到全局列表
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	// 这时取消 P 的禁止抢占，因为使用 mutex 时候 P 必须可抢占
	runtime_procUnpin()
	// 加锁
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	// 当锁住后，再次固定 P 取其 id
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	// 并再次检查是否符合条件，因为可能中途已被其他线程调用。当再次固定 P 时 poolCleanup 不会被调用。
	s := p.localSize
	l := p.local
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	// 如果数组为空，新建。将其添加到 allPools，垃圾回收器从这里获取所有 Pool 实例。
	if p.local == nil {
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	// 根据 P 数量创建 slice，如果 GOMAXPROCS 在 GC 间发生变化
	// 我们重新分配此数组并丢弃旧的
	size := runtime.GOMAXPROCS(0)
	local := make([]poolLocal, size)
	// 将底层数组起始指针保存到 p.local，并设置 p.localSize
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.
	// 该函数会注册到运行时 GC 阶段(前)，此时为 STW 状态，不需要加锁。它必须不处理分配且不调用任何运行时函数。

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).
	// 由于此时是 STW，不存在用户态代码能尝试读取 localPool，进而所有的 P 都已固定（与 goroutine 绑定）

	// Drop victim caches from all pools.
	// 从所有的 oldPols 中删除 victim
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	// 将主缓存移动到 victim 缓存
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex // allPools 的锁

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	// allPools 是一组 pool 的集合，具有非空主缓存。有两种形式来保护它的读写：1. allPoolsMu 锁; 2. STW.
	allPools []*Pool

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	// oldPools 是一组 pool 的集合，具有非空 victim 缓存。由 STW 保护
	oldPools []*Pool
)

func init() {
	// 将缓存清理函数注册到运行时 GC 时间段
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	// 简单的通过 p.local 的头指针与索引来第 i 个 pooLocal ，也就是 p.local[i] 。
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
