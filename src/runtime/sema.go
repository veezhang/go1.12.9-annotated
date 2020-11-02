// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Semaphore implementation exposed to Go.
// Intended use is provide a sleep and wakeup
// primitive that can be used in the contended case
// of other synchronization primitives.
// Thus it targets the same goal as Linux's futex,
// but it has much simpler semantics.
//
// That is, don't think of these as semaphores.
// Think of them as a way to implement sleep and wakeup
// such that every sleep is paired with a single wakeup,
// even if, due to races, the wakeup happens before the sleep.
//
// See Mullender and Cox, ``Semaphores in Plan 9,''
// https://swtch.com/semaphore.pdf
//
// Go 中的信号量实现
// 旨在提供一个 sleep 和 wakeup 原语，并用于竞争情况下的同步原语，因此目标与 linux 的 futex 一样，但语义更加简单
// 因此不要将其视为信号量，将其视为 sleep 和 wakeup 的一种实现, 使每个 sleep 都有一个唯一的 wakeup 配对，
// 即使如此，由于竞争的存在 wakeup happens before sleep。

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"unsafe"
)

// Asynchronous semaphore for sync.Mutex.
// sync.Mutex 的异步信号量

// A semaRoot holds a balanced tree of sudog with distinct addresses (s.elem).
// Each of those sudog may in turn point (through s.waitlink) to a list
// of other sudogs waiting on the same address.
// The operations on the inner lists of sudogs with the same address
// are all O(1). The scanning of the top-level semaRoot list is O(log n),
// where n is the number of distinct addresses with goroutines blocked
// on them that hash to the given semaRoot.
// See golang.org/issue/17953 for a program that worked badly
// before we introduced the second level of list, and test/locklinear.go
// for a test that exercises this.
// semaRoot 包含一个具有不同地址（s.elem）的平衡的 sudog 树。每个 sudog 可以反过来（通过 s.waitlink）指向其他在相同地址上等待的 sudog 的列表。
// sudog 内部列表上的操作都是 O(1)。最顶端 semaRoot 列表的扫描是 O(log n)，n 是阻塞了 goroutine 的不同地址的数量，这些地址散列到给定的 semaRoot。
type semaRoot struct {
	lock  mutex
	treap *sudog // root of balanced tree of unique waiters.
	nwait uint32 // Number of waiters. Read w/o the lock.
}

// Prime to not correlate with any user patterns.
const semTabSize = 251

// semTabSize（质数） 个的 semaRoot 表
var semtable [semTabSize]struct {
	root semaRoot
	pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte
}

//go:linkname sync_runtime_Semacquire sync.runtime_Semacquire
func sync_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile)
}

//go:linkname poll_runtime_Semacquire internal/poll.runtime_Semacquire
func poll_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile)
}

//go:linkname sync_runtime_Semrelease sync.runtime_Semrelease
func sync_runtime_Semrelease(addr *uint32, handoff bool) {
	semrelease1(addr, handoff)
}

//go:linkname sync_runtime_SemacquireMutex sync.runtime_SemacquireMutex
func sync_runtime_SemacquireMutex(addr *uint32, lifo bool) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile)
}

//go:linkname poll_runtime_Semrelease internal/poll.runtime_Semrelease
func poll_runtime_Semrelease(addr *uint32) {
	semrelease(addr)
}

func readyWithTime(s *sudog, traceskip int) {
	if s.releasetime != 0 {
		s.releasetime = cputicks()
	}
	goready(s.g, traceskip)
}

type semaProfileFlags int

const (
	semaBlockProfile semaProfileFlags = 1 << iota
	semaMutexProfile
)

// Called from runtime.
// 从运行时调用
func semacquire(addr *uint32) {
	semacquire1(addr, false, 0)
}

func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags) {
	// 获取当前 goroutine ，该调用发生在 goroutine 运行时，所以有绑定的 P
	gp := getg()
	if gp != gp.m.curg {
		throw("semacquire not on the G stack")
	}

	// Easy case.
	// 简单情况，直接 acquire 成功
	if cansemacquire(addr) {
		return
	}

	// Harder case:
	//	increment waiter count
	//	try cansemacquire one more time, return if succeeded
	//	enqueue itself as a waiter
	//	sleep
	//	(waiter descriptor is dequeued by signaler)
	// 复杂情况
	//		增加等待计数
	// 		再试一次 cansemacquire 如果成功则直接返回
	//		将自己作为等待器入队
	//		休眠
	//		(等待器描述符由出队信号产生出队行为)
	s := acquireSudog()
	root := semroot(addr)
	t0 := int64(0)
	s.releasetime = 0
	s.acquiretime = 0
	s.ticket = 0
	if profile&semaBlockProfile != 0 && blockprofilerate > 0 {
		t0 = cputicks()
		s.releasetime = -1
	}
	if profile&semaMutexProfile != 0 && mutexprofilerate > 0 {
		if t0 == 0 {
			t0 = cputicks()
		}
		s.acquiretime = t0
	}
	for {
		lock(&root.lock)
		// Add ourselves to nwait to disable "easy case" in semrelease.
		// 将我们自己添加到 nwait 以禁用 semrelease 中的 "Easy case" 。
		atomic.Xadd(&root.nwait, 1)
		// Check cansemacquire to avoid missed wakeup.
		// 检测 cansemacquire ，以免错过 wakeup 。
		if cansemacquire(addr) {
			atomic.Xadd(&root.nwait, -1)
			unlock(&root.lock)
			break
		}
		// Any semrelease after the cansemacquire knows we're waiting
		// (we set nwait above), so go to sleep.
		// 在 cansemacquire 之后的任何 semrelease 都知道我们在等待（我们在上面设置了nwait），所以去睡觉吧。
		root.queue(addr, s, lifo)
		goparkunlock(&root.lock, waitReasonSemacquire, traceEvGoBlockSync, 4)
		if s.ticket != 0 || cansemacquire(addr) {
			break
		}
	}
	if s.releasetime > 0 {
		blockevent(s.releasetime-t0, 3)
	}
	releaseSudog(s)
}

func semrelease(addr *uint32) {
	semrelease1(addr, false)
}

func semrelease1(addr *uint32, handoff bool) {
	root := semroot(addr)
	atomic.Xadd(addr, 1)

	// Easy case: no waiters?
	// This check must happen after the xadd, to avoid a missed wakeup
	// (see loop in semacquire).
	// 简单的情况：没有等待的 goroutine ？
	// 此检查必须在xadd之后进行，以避免错过唤醒（请参阅 semacquire 中的循环）。
	if atomic.Load(&root.nwait) == 0 {
		return
	}

	// Harder case: search for a waiter and wake it.
	// 复杂情况：找一个等待的 goroutine 并唤醒
	lock(&root.lock)
	if atomic.Load(&root.nwait) == 0 {
		// The count is already consumed by another goroutine,
		// so no need to wake up another goroutine.
		// 计数已被另一个 goroutine 消耗，因此不需要唤醒另一个 goroutine 。
		unlock(&root.lock)
		return
	}
	// 出队
	s, t0 := root.dequeue(addr)
	// 如果 s 已经入队过，修正 nwait
	if s != nil {
		atomic.Xadd(&root.nwait, -1)
	}
	unlock(&root.lock)
	if s != nil { // May be slow, so unlock first
		acquiretime := s.acquiretime
		if acquiretime != 0 {
			mutexevent(t0-acquiretime, 3)
		}
		if s.ticket != 0 {
			throw("corrupted semaphore ticket")
		}
		if handoff && cansemacquire(addr) {
			s.ticket = 1
		}
		// readyWithTime 调用 goready ，将 sudog 加入到可运行队列
		readyWithTime(s, 5)
	}
}

// 获取 addr 对应的 semaRoot
func semroot(addr *uint32) *semaRoot {
	return &semtable[(uintptr(unsafe.Pointer(addr))>>3)%semTabSize].root
}

// 检测是否可以 acquire
func cansemacquire(addr *uint32) bool {
	for {
		v := atomic.Load(addr)
		if v == 0 {
			// 没有了
			return false
		}
		if atomic.Cas(addr, v, v-1) {
			// 成功了，并且也减少了
			return true
		}
	}
}

// queue adds s to the blocked goroutines in semaRoot.
// queue 入队 semaRoot
func (root *semaRoot) queue(addr *uint32, s *sudog, lifo bool) {
	s.g = getg()
	s.elem = unsafe.Pointer(addr)
	s.next = nil
	s.prev = nil

	var last *sudog
	pt := &root.treap
	for t := *pt; t != nil; t = *pt {
		if t.elem == unsafe.Pointer(addr) {
			// Already have addr in list.
			// 已经有对于的 addr 了
			if lifo { // 后进先出， 在 Mutex 的饥饿模式
				// Substitute s in t's place in treap.
				// 在 treap 中 s 代替 t
				*pt = s
				s.ticket = t.ticket
				s.acquiretime = t.acquiretime
				s.parent = t.parent
				s.prev = t.prev
				s.next = t.next
				if s.prev != nil {
					s.prev.parent = s
				}
				if s.next != nil {
					s.next.parent = s
				}
				// Add t first in s's wait list.
				// t 加入到 s 的等待队列
				s.waitlink = t
				s.waittail = t.waittail
				if s.waittail == nil {
					s.waittail = t
				}
				t.parent = nil
				t.prev = nil
				t.next = nil
				t.waittail = nil
			} else { // 先进先出
				// Add s to end of t's wait list.
				// s 加入到 t 的尾部
				if t.waittail == nil {
					t.waitlink = s
				} else {
					t.waittail.waitlink = s
				}
				t.waittail = s
				s.waitlink = nil
			}
			return
		}
		last = t
		// 继续查询其他的
		if uintptr(unsafe.Pointer(addr)) < uintptr(t.elem) {
			pt = &t.prev
		} else {
			pt = &t.next
		}
	}

	// Add s as new leaf in tree of unique addrs.
	// The balanced tree is a treap using ticket as the random heap priority.
	// That is, it is a binary tree ordered according to the elem addresses,
	// but then among the space of possible binary trees respecting those
	// addresses, it is kept balanced on average by maintaining a heap ordering
	// on the ticket: s.ticket <= both s.prev.ticket and s.next.ticket.
	// https://en.wikipedia.org/wiki/Treap
	// https://faculty.washington.edu/aragon/pubs/rst89.pdf
	//
	// s.ticket compared with zero in couple of places, therefore set lowest bit.
	// It will not affect treap's quality noticeably.
	// 添加 s 作为新的叶子节点
	// treap 是用 ticket 作为随机 heap 优先级的 Tree Heap（树堆）
	// 也就是说，它是根据 elem 地址排序的二叉树，但是在考虑这些地址的可能的二叉树空间中，
	// 通过在 ticket 上保持堆排序来保持平均平衡：s.ticket <= both s.prev.ticket and s.next.ticket
	s.ticket = fastrand() | 1
	s.parent = last
	*pt = s

	// Rotate up into tree according to ticket (priority).
	// 根据 ticket 旋转
	for s.parent != nil && s.parent.ticket > s.ticket {
		if s.parent.prev == s {
			root.rotateRight(s.parent)
		} else {
			if s.parent.next != s {
				panic("semaRoot queue")
			}
			root.rotateLeft(s.parent)
		}
	}
}

// dequeue searches for and finds the first goroutine
// in semaRoot blocked on addr.
// If the sudog was being profiled, dequeue returns the time
// at which it was woken up as now. Otherwise now is 0.
// dequeue 搜索并找到 semaRoot 中在addr上阻塞的第一个 goroutine 。
func (root *semaRoot) dequeue(addr *uint32) (found *sudog, now int64) {
	ps := &root.treap
	s := *ps
	for ; s != nil; s = *ps {
		if s.elem == unsafe.Pointer(addr) {
			// 找到对应的 addr
			goto Found
		}
		if uintptr(unsafe.Pointer(addr)) < uintptr(s.elem) {
			ps = &s.prev
		} else {
			ps = &s.next
		}
	}
	// 未找到，返回
	return nil, 0

Found:
	now = int64(0)
	if s.acquiretime != 0 {
		now = cputicks()
	}
	if t := s.waitlink; t != nil {
		// Substitute t, also waiting on addr, for s in root tree of unique addrs.
		// 有多个等在在 addr 上 ， 取出 s （第一个等待着的）
		*ps = t
		t.ticket = s.ticket
		t.parent = s.parent
		t.prev = s.prev
		if t.prev != nil {
			t.prev.parent = t
		}
		t.next = s.next
		if t.next != nil {
			t.next.parent = t
		}
		if t.waitlink != nil {
			t.waittail = s.waittail
		} else {
			t.waittail = nil
		}
		t.acquiretime = now
		s.waitlink = nil
		s.waittail = nil
	} else {
		// Rotate s down to be leaf of tree for removal, respecting priorities.
		// 这里就一个 s 在等待， 根据优先级旋转 s 到叶子节点，然后删除 s
		for s.next != nil || s.prev != nil {
			if s.next == nil || s.prev != nil && s.prev.ticket < s.next.ticket {
				root.rotateRight(s)
			} else {
				root.rotateLeft(s)
			}
		}
		// Remove s, now a leaf.
		if s.parent != nil {
			if s.parent.prev == s {
				s.parent.prev = nil
			} else {
				s.parent.next = nil
			}
		} else {
			root.treap = nil
		}
	}
	s.parent = nil
	s.elem = nil
	s.next = nil
	s.prev = nil
	s.ticket = 0
	return s, now
}

// rotateLeft rotates the tree rooted at node x.
// turning (x a (y b c)) into (y (x a b) c).
// rotateLeft 左旋
func (root *semaRoot) rotateLeft(x *sudog) {
	// p -> (x a (y b c))
	p := x.parent
	a, y := x.prev, x.next
	b, c := y.prev, y.next

	y.prev = x
	x.parent = y
	y.next = c
	if c != nil {
		c.parent = y
	}
	x.prev = a
	if a != nil {
		a.parent = x
	}
	x.next = b
	if b != nil {
		b.parent = x
	}

	y.parent = p
	if p == nil {
		root.treap = y
	} else if p.prev == x {
		p.prev = y
	} else {
		if p.next != x {
			throw("semaRoot rotateLeft")
		}
		p.next = y
	}
}

// rotateRight rotates the tree rooted at node y.
// turning (y (x a b) c) into (x a (y b c)).
// rotateRight 右旋
func (root *semaRoot) rotateRight(y *sudog) {
	// p -> (y (x a b) c)
	p := y.parent
	x, c := y.prev, y.next
	a, b := x.prev, x.next

	x.prev = a
	if a != nil {
		a.parent = x
	}
	x.next = y
	y.parent = x
	y.prev = b
	if b != nil {
		b.parent = y
	}
	y.next = c
	if c != nil {
		c.parent = y
	}

	x.parent = p
	if p == nil {
		root.treap = x
	} else if p.prev == y {
		p.prev = x
	} else {
		if p.next != y {
			throw("semaRoot rotateRight")
		}
		p.next = x
	}
}

// notifyList is a ticket-based notification list used to implement sync.Cond.
//
// It must be kept in sync with the sync package.
// notifyList 基于 ticket 实现通知列表，用来实现 sync.Cond
type notifyList struct {
	// wait is the ticket number of the next waiter. It is atomically
	// incremented outside the lock.
	// wait 为下一个 waiter 的 ticket 编号。在没有 lock 的情况下原子自增。
	wait uint32

	// notify is the ticket number of the next waiter to be notified. It can
	// be read outside the lock, but is only written to with lock held.
	//
	// Both wait & notify can wrap around, and such cases will be correctly
	// handled as long as their "unwrapped" difference is bounded by 2^31.
	// For this not to be the case, we'd need to have 2^31+ goroutines
	// blocked on the same condvar, which is currently not possible.
	// notify 是下一个被通知的 waiter 的 ticket 编号。它可以在没有 lock 的情况下进行读取，但只有在持有 lock 的情况下才能进行写。
	//
	// wait 和 notify 会循环使用，当 2^31+ 个 goroutines 阻塞在同一个条件变量，则有问题。但是一般不可能。
	notify uint32

	// List of parked waiters.
	// waiter 列表
	lock mutex
	head *sudog
	tail *sudog
}

// less checks if a < b, considering a & b running counts that may overflow the
// 32-bit range, and that their "unwrapped" difference is always less than 2^31.
func less(a, b uint32) bool {
	return int32(a-b) < 0
}

// notifyListAdd adds the caller to a notify list such that it can receive
// notifications. The caller must eventually call notifyListWait to wait for
// such a notification, passing the returned ticket number.
//go:linkname notifyListAdd sync.runtime_notifyListAdd
// notifyListAdd 将调用者添加到通知列表，以便接收通知。调用者最终必须调用 notifyListWait 等待这样的通知，并传递返回的 ticket 编号。
func notifyListAdd(l *notifyList) uint32 {
	// This may be called concurrently, for example, when called from
	// sync.Cond.Wait while holding a RWMutex in read mode.
	// 这可以并发调用，例如，当在 read 模式下保持 RWMutex 时从 sync.Cond.Wait 调用时。
	return atomic.Xadd(&l.wait, 1) - 1
}

// notifyListWait waits for a notification. If one has been sent since
// notifyListAdd was called, it returns immediately. Otherwise, it blocks.
//go:linkname notifyListWait sync.runtime_notifyListWait
// notifyListWait 等待通知。如果在调用 notifyListAdd 后发送了一个，则立即返回。否则，它会阻塞。
func notifyListWait(l *notifyList, t uint32) {
	lock(&l.lock)

	// Return right away if this ticket has already been notified.
	// 如果 ticket 编号对应的 goroutine 已经被通知到，则立刻返回
	if less(t, l.notify) {
		unlock(&l.lock)
		return
	}

	// Enqueue itself.
	// 入队，获取一个 *sudog 并填充字段
	s := acquireSudog()
	s.g = getg()
	s.ticket = t
	s.releasetime = 0
	t0 := int64(0)
	if blockprofilerate > 0 {
		t0 = cputicks()
		s.releasetime = -1
	}
	// 加入到链表中
	if l.tail == nil {
		l.head = s
	} else {
		l.tail.next = s
	}
	l.tail = s
	// 将 M/P/G 解绑，并将 G 调整为等待状态，放入 sudog 等待队列中
	goparkunlock(&l.lock, waitReasonSyncCondWait, traceEvGoBlockCond, 3)
	if t0 != 0 {
		blockevent(s.releasetime-t0, 2)
	}
	releaseSudog(s)
}

// notifyListNotifyAll notifies all entries in the list.
//go:linkname notifyListNotifyAll sync.runtime_notifyListNotifyAll
// notifyListNotifyAll 通知列表里的所有人
func notifyListNotifyAll(l *notifyList) {
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock.
	// 没有 waiter
	if atomic.Load(&l.wait) == atomic.Load(&l.notify) {
		return
	}

	// Pull the list out into a local variable, waiters will be readied
	// outside the lock.
	// 把 l.head 保存到本地变量，waiter 可以在无锁的情况下 ready
	lock(&l.lock)
	s := l.head
	l.head = nil
	l.tail = nil

	// Update the next ticket to be notified. We can set it to the current
	// value of wait because any previous waiters are already in the list
	// or will notice that they have already been notified when trying to
	// add themselves to the list.
	// 更新要通知的下一个 ticket。可以看成已经没有 waiter 了，可以设置为 notify
	atomic.Store(&l.notify, atomic.Load(&l.wait))
	unlock(&l.lock)

	// Go through the local list and ready all waiters.
	// 遍历整个本地列表，并 ready 所有的 waiter
	for s != nil {
		next := s.next
		s.next = nil
		// readyWithTime 调用 goready ，将 sudog 加入到可运行队列
		readyWithTime(s, 4)
		s = next
	}
}

// notifyListNotifyOne notifies one entry in the list.
//go:linkname notifyListNotifyOne sync.runtime_notifyListNotifyOne
// notifyListNotifyOne 通知列表中的一个
func notifyListNotifyOne(l *notifyList) {
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock at all.
	// 没有 waiter
	if atomic.Load(&l.wait) == atomic.Load(&l.notify) {
		return
	}

	lock(&l.lock)

	// Re-check under the lock if we need to do anything.
	// 在加速清楚下再次检测
	t := l.notify
	if t == atomic.Load(&l.wait) {
		unlock(&l.lock)
		return
	}

	// Update the next notify ticket number.
	// 更新下一个需要唤醒的 ticket 编号
	atomic.Store(&l.notify, t+1)

	// Try to find the g that needs to be notified.
	// If it hasn't made it to the list yet we won't find it,
	// but it won't park itself once it sees the new notify number.
	//
	// This scan looks linear but essentially always stops quickly.
	// Because g's queue separately from taking numbers,
	// there may be minor reorderings in the list, but we
	// expect the g we're looking for to be near the front.
	// The g has others in front of it on the list only to the
	// extent that it lost the race, so the iteration will not
	// be too long. This applies even when the g is missing:
	// it hasn't yet gotten to sleep and has lost the race to
	// the (few) other g's that we find on the list.
	// 尝试找到需要被通知的 g
	// 如果目前还没来得及入队，是无法找到的，但是，当它看到通知编号已经发生改变是不会被 park 的。
	//
	// 这个查找过程看起来是线性复杂度，但实际上很快就停了。
	// 因为 g 的队列与获取编号不同，队列中会出现少量重排，但我们希望找到靠前的 g 。
	// 只有在 g 竞争失败了，才可能有其他的 g 在它的前面，因此这个迭代也不会太久。
	// 同时，即便找不到 g，这个情况也成立：它还没有休眠，并且与我们在队列上找到的（少数）其他 g 的竞争失败了。
	for p, s := (*sudog)(nil), l.head; s != nil; p, s = s, s.next {
		if s.ticket == t {
			n := s.next
			if p != nil {
				p.next = n
			} else {
				l.head = n
			}
			if n == nil {
				l.tail = p
			}
			unlock(&l.lock)
			s.next = nil
			// readyWithTime 调用 goready ，将 sudog 加入到可运行队列
			readyWithTime(s, 4)
			// 找到了，就返回
			return
		}
	}
	unlock(&l.lock)
}

//go:linkname notifyListCheck sync.runtime_notifyListCheck
func notifyListCheck(sz uintptr) {
	if sz != unsafe.Sizeof(notifyList{}) {
		print("runtime: bad notifyList size - sync=", sz, " runtime=", unsafe.Sizeof(notifyList{}), "\n")
		throw("bad notifyList size")
	}
}

//go:linkname sync_nanotime sync.runtime_nanotime
func sync_nanotime() int64 {
	return nanotime()
}
