// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix darwin dragonfly freebsd js,wasm linux nacl netbsd openbsd solaris windows

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// 集成的网络轮训器（平台无关的部分）
// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue) must define the following functions:
// func netpollinit()			// to initialize the poller
// func netpollopen(fd uintptr, pd *pollDesc) int32	// to arm edge-triggered notifications
// and associate fd with pd.
// An implementation must call the following function to denote that the pd is ready.
// func netpollready(gpp **g, pd *pollDesc, mode int32)

// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
// pdReady - io readiness notification is pending;
//           a goroutine consumes the notification by changing the state to nil.
// pdWait - a goroutine prepares to park on the semaphore, but not yet parked;
//          the goroutine commits to park by changing the state to G pointer,
//          or, alternatively, concurrent io notification changes the state to READY,
//          or, alternatively, concurrent timeout/close changes the state to nil.
// G pointer - the goroutine is blocked on the semaphore;
//             io notification or timeout/close changes the state to READY or nil respectively
//             and unparks the goroutine.
// nil - nothing of the above.
//
// 集成网络轮询（与平台无关的）
// 特定的实现（epoll / kqueue）必须定义以下功能：
// func netpollinit()								// 初始化网络轮询器
// func netpollopen(fd uintptr, pd *pollDesc) int32	// 进行边缘触发的通知
// 并将fd与pd关联。
// 实现必须调用以下函数来表示 pd 已准备就绪。
// func netpollready(gpp **g, pd *pollDesc, mode int32)
//
// pollDesc 包含 2 个二进制信号量 rg 和 wg ，分别用于 park reader 和 writer goroutine。信号量可以处于以下状态：
// pdReady		网络io就绪通知，goroutine消费完后应置为nil
// pdWait		goroutine等待被挂起，后续可能有3种情况:
//					1. goroutine 被调度器挂起，置为 goroutine 地址
//					2. 收到 io 通知，置为 pdReady
//					3. 超时或者被关闭，置为 nil
// G pointer	被挂起的 goroutine 的地址，当 io 就绪时、或者超时、被关闭时，此 goroutine 将被唤醒，同时将状态改为 pdReady 或者 nil 。
// nil 			以上都不是
const (
	pdReady uintptr = 1
	pdWait  uintptr = 2
)

const pollBlockSize = 4 * 1024

// Network poller descriptor.
// 网络轮询器描述符，建立起文件描述符和 goroutine 之间的关联
//
// No heap pointers. 无堆指针
//
//go:notinheap
type pollDesc struct {
	link *pollDesc // in pollcache, protected by pollcache.lock // 在 pollcache中，受 pollcache.lock 的保护

	// The lock protects pollOpen, pollSetDeadline, pollUnblock and deadlineimpl operations.
	// This fully covers seq, rt and wt variables. fd is constant throughout the PollDesc lifetime.
	// pollReset, pollWait, pollWaitCanceled and runtime·netpollready (IO readiness notification)
	// proceed w/o taking the lock. So closing, rg, rd, wg and wd are manipulated
	// in a lock-free way by all operations.
	// NOTE(dvyukov): the following code uses uintptr to store *g (rg/wg),
	// that will blow up when GC starts moving objects.
	// 该 lock 保护 pollOpen ， polleSetDeadline ， pollUnblock 和 deadlineimpl 操作。这完全涵盖了 seq ， rt 和 wt 变量。
	// fd 在整 个PollDes c生命周期中都是恒定的。 pollReset ， pollWait ， pollWaitCanceled 和 runtime·netpollready（IO准备就绪通知）不带锁继续进行。
	// 因此， closing ， rg ， rd ， wg 和 wd 由所有操作以 lock-free 方式进行操作。
	// NOTE（dvyukov）：以下代码使用 uintptr 来存储 *g (rg/wg) ，这将在 GC 开始移动对象时崩溃了（rg/wg 指针无效）。
	lock    mutex   // protects the following fields
	fd      uintptr // fd 是底层网络 io 文件描述符，整个生命期内，不能改变值
	closing bool
	user    uint32  // user settable cookie // 用户可设置的 cookie
	rseq    uintptr // protects from stale read timers // 防止过时的读计时器，这个序列号用来判断当前定时器是否过期
	rg      uintptr // pdReady, pdWait, G waiting for read or nil // 读状态 pdReady , pdWait , 等待读的 G 指针 或者 nil
	rt      timer   // read deadline timer (set if rt.f != nil) // 读超时计时器
	rd      int64   // read deadline // 读超时时间
	wseq    uintptr // protects from stale write timers // 防止过时的写计时器，这个序列号用来判断当前定时器是否过期
	wg      uintptr // pdReady, pdWait, G waiting for write or nil // 写状态 pdReady , pdWait , 等待写的 G 指针 或者 nil
	wt      timer   // write deadline timer // 写超时计时器
	wd      int64   // write deadline // 写超时时间
}

// pollDesc 缓存
type pollCache struct {
	lock  mutex
	first *pollDesc
	// PollDesc objects must be type-stable,
	// because we can get ready notification from epoll/kqueue
	// after the descriptor is closed/reused.
	// Stale notifications are detected using seq variable,
	// seq is incremented when deadlines are changed or descriptor is reused.
	// PollDesc 对象必须是类型稳定的，因为在关闭/重用描述符后，我们可以从 epoll/kqueue 获得就绪的通知。
	// 使用 seq 变量检测到过时的通知，当更改截止日期或重用描述符时，seq会增加。
}

var (
	netpollInited  uint32    // netpoll 是否初始化了
	pollcache      pollCache // pollDesc 缓存
	netpollWaiters uint32    // 等待 netpoll 的 g 的数目
)

// 初始化，linux 调用 epollcreate1 / epollcreate
//go:linkname poll_runtime_pollServerInit internal/poll.runtime_pollServerInit
func poll_runtime_pollServerInit() {
	netpollinit()
	atomic.Store(&netpollInited, 1)
}

// 返回是否已经初始化了
func netpollinited() bool {
	return atomic.Load(&netpollInited) != 0
}

//go:linkname poll_runtime_isPollServerDescriptor internal/poll.runtime_isPollServerDescriptor

// poll_runtime_isPollServerDescriptor reports whether fd is a
// descriptor being used by netpoll.
// 返回 fd 是否是 netpoll 使用的 fd ， Linux 上也就是 epfd
func poll_runtime_isPollServerDescriptor(fd uintptr) bool {
	fds := netpolldescriptor()
	if GOOS != "aix" {
		return fd == fds
	} else {
		// AIX have a pipe in its netpoll implementation.
		// Therefore, two fd are returned by netpolldescriptor using a mask.
		return fd == fds&0xFFFF || fd == (fds>>16)&0xFFFF
	}
}

// 加入到 netpoll 中， linux 是加入到 epoll 中，ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET, epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
//go:linkname poll_runtime_pollOpen internal/poll.runtime_pollOpen
func poll_runtime_pollOpen(fd uintptr) (*pollDesc, int) {
	// 缓存中分配一个 pd
	pd := pollcache.alloc()
	lock(&pd.lock)
	if pd.wg != 0 && pd.wg != pdReady {
		throw("runtime: blocked write on free polldesc")
	}
	if pd.rg != 0 && pd.rg != pdReady {
		throw("runtime: blocked read on free polldesc")
	}
	// 初始化 pd
	pd.fd = fd
	pd.closing = false
	pd.rseq++
	pd.rg = 0
	pd.rd = 0
	pd.wseq++
	pd.wg = 0
	pd.wd = 0
	unlock(&pd.lock)

	// 调用 netpollopen 加入到 netpoll 中
	var errno int32
	errno = netpollopen(fd, pd)
	return pd, int(errno)
}

// 从 netpoll 中移除，linux 是从 epoll 中移除，epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
//go:linkname poll_runtime_pollClose internal/poll.runtime_pollClose
func poll_runtime_pollClose(pd *pollDesc) {
	if !pd.closing {
		throw("runtime: close polldesc w/o unblock")
	}
	if pd.wg != 0 && pd.wg != pdReady {
		throw("runtime: blocked write on closing polldesc")
	}
	if pd.rg != 0 && pd.rg != pdReady {
		throw("runtime: blocked read on closing polldesc")
	}
	// 从 netpll 中移除
	netpollclose(pd.fd)
	// 回收 pd
	pollcache.free(pd)
}

// 释放 pd ，回收到 pollCache 中串起来
func (c *pollCache) free(pd *pollDesc) {
	lock(&c.lock)
	pd.link = c.first
	c.first = pd
	unlock(&c.lock)
}

// 重置 pd
//go:linkname poll_runtime_pollReset internal/poll.runtime_pollReset
func poll_runtime_pollReset(pd *pollDesc, mode int) int {
	// 检查
	err := netpollcheckerr(pd, int32(mode))
	if err != 0 {
		return err
	}
	if mode == 'r' {
		pd.rg = 0
	} else if mode == 'w' {
		pd.wg = 0
	}
	return 0
}

// 等待直到 IO 就绪，或超时/关闭
//go:linkname poll_runtime_pollWait internal/poll.runtime_pollWait
func poll_runtime_pollWait(pd *pollDesc, mode int) int {
	// 检查
	err := netpollcheckerr(pd, int32(mode))
	if err != 0 {
		return err
	}
	// As for now only Solaris and AIX use level-triggered IO.
	// 到目前为止，只有Solaris和AIX使用水平触发的 IO 。
	if GOOS == "solaris" || GOOS == "aix" {
		netpollarm(pd, mode)
	}
	// 阻塞轮询直到网络就绪
	for !netpollblock(pd, int32(mode), false) {
		// 检查
		err = netpollcheckerr(pd, int32(mode))
		if err != 0 {
			return err
		}
		// Can happen if timeout has fired and unblocked us,
		// but before we had a chance to run, timeout has been reset.
		// Pretend it has not happened and retry.
		// 如果超时已触发并取消阻塞我们，则可能发生，但是在我们有机会运行之前，超时已被重置。 假装没有发生，然后重试。
	}
	return 0
}

//go:linkname poll_runtime_pollWaitCanceled internal/poll.runtime_pollWaitCanceled
func poll_runtime_pollWaitCanceled(pd *pollDesc, mode int) {
	// This function is used only on windows after a failed attempt to cancel
	// a pending async IO operation. Wait for ioready, ignore closing or timeouts.
	// 仅在 Windows 上当尝试取消挂起的异步IO操作失败后调用。 等待ioready，忽略关闭或超时。
	for !netpollblock(pd, int32(mode), true) {
	}
}

// 设置超时时间
//go:linkname poll_runtime_pollSetDeadline internal/poll.runtime_pollSetDeadline
func poll_runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {
	lock(&pd.lock)
	if pd.closing {
		unlock(&pd.lock)
		return
	}
	rd0, wd0 := pd.rd, pd.wd
	combo0 := rd0 > 0 && rd0 == wd0
	// d > 0	设置到期时间还没有到
	// d < 0	设置到期时间已经到期了
	// d = 0	没有设置到期时间
	if d > 0 {
		d += nanotime() // d 为到期时间
		if d <= 0 {
			// If the user has a deadline in the future, but the delay calculation
			// overflows, then set the deadline to the maximum possible value.
			// 计算溢出
			d = 1<<63 - 1
		}
	}
	// 设置读写的超时时间
	if mode == 'r' || mode == 'r'+'w' {
		pd.rd = d
	}
	if mode == 'w' || mode == 'r'+'w' {
		pd.wd = d
	}
	combo := pd.rd > 0 && pd.rd == pd.wd
	// rtf 为到期处理函数
	rtf := netpollReadDeadline
	if combo {
		rtf = netpollDeadline
	}
	// 如果原来没有设置读计时器
	if pd.rt.f == nil {
		// 设置了都超时时间，这里增加读计时器
		if pd.rd > 0 {
			pd.rt.f = rtf
			pd.rt.when = pd.rd
			// Copy current seq into the timer arg.
			// Timer func will check the seq against current descriptor seq,
			// if they differ the descriptor was reused or timers were reset.
			pd.rt.arg = pd
			pd.rt.seq = pd.rseq
			addtimer(&pd.rt)
		}
	} else if pd.rd != rd0 || combo != combo0 {
		// 如果读截至时间有变化，或者 combo != combo0
		pd.rseq++ // invalidate current timers
		if pd.rd > 0 {
			// 修改原有的计时器
			modtimer(&pd.rt, pd.rd, 0, rtf, pd, pd.rseq)
		} else {
			// 删除原有的计时器
			deltimer(&pd.rt)
			pd.rt.f = nil
		}
	}
	// 如果原来没有设置写计时器
	if pd.wt.f == nil {
		// 设置了都超时时间，并且和读不共用定时器，这里增加写计时器
		if pd.wd > 0 && !combo {
			pd.wt.f = netpollWriteDeadline
			pd.wt.when = pd.wd
			pd.wt.arg = pd
			pd.wt.seq = pd.wseq
			addtimer(&pd.wt)
		}
	} else if pd.wd != wd0 || combo != combo0 {
		// 如果读截至时间有变化，或者 combo != combo0
		pd.wseq++ // invalidate current timers
		if pd.wd > 0 && !combo {
			// 修改原有的计时器
			modtimer(&pd.wt, pd.wd, 0, netpollWriteDeadline, pd, pd.wseq)
		} else {
			// 删除原有的计时器
			deltimer(&pd.wt)
			pd.wt.f = nil
		}
	}
	// If we set the new deadline in the past, unblock currently pending IO if any.
	// 如果我们设置了的新截止时间，请取消阻塞当前待处理的IO（如果有）。
	var rg, wg *g
	// 如果超时时间 < 0  则尝试将对应的 G 取出并设置为runnable
	if pd.rd < 0 || pd.wd < 0 {
		atomic.StorepNoWB(noescape(unsafe.Pointer(&wg)), nil) // full memory barrier between stores to rd/wd and load of rg/wg in netpollunblock
		if pd.rd < 0 {
			// 获取等待读的 G
			rg = netpollunblock(pd, 'r', false)
		}
		if pd.wd < 0 {
			// 获取等待写的 G
			wg = netpollunblock(pd, 'w', false)
		}
	}
	unlock(&pd.lock)
	// 如果有等待读的 G ，则唤醒
	if rg != nil {
		netpollgoready(rg, 3)
	}
	// 如果有等待的 G ，则唤醒
	if wg != nil {
		netpollgoready(wg, 3)
	}
}

// 非阻塞轮询，取消阻塞在 pd 的 goroutine 并加入到运行队列，设置 closing = ture ，seq++ ，清理 timer
//go:linkname poll_runtime_pollUnblock internal/poll.runtime_pollUnblock
func poll_runtime_pollUnblock(pd *pollDesc) {
	lock(&pd.lock)
	if pd.closing {
		throw("runtime: unblock on closing polldesc")
	}
	pd.closing = true
	pd.rseq++
	pd.wseq++
	var rg, wg *g
	atomic.StorepNoWB(noescape(unsafe.Pointer(&rg)), nil) // full memory barrier between store to closing and read of rg/wg in netpollunblock
	// 获取等待读的 G
	rg = netpollunblock(pd, 'r', false)
	// 获取等待写的 G
	wg = netpollunblock(pd, 'w', false)
	// 删除原有的计时器
	if pd.rt.f != nil {
		deltimer(&pd.rt)
		pd.rt.f = nil
	}
	if pd.wt.f != nil {
		deltimer(&pd.wt)
		pd.wt.f = nil
	}
	unlock(&pd.lock)
	// 如果有等待读的 G ，则唤醒
	if rg != nil {
		netpollgoready(rg, 3)
	}
	// 如果有等待的 G ，则唤醒
	if wg != nil {
		netpollgoready(wg, 3)
	}
}

// make pd ready, newly runnable goroutines (if any) are added to toRun.
// May run during STW, so write barriers are not allowed.
// 让 pd ready ，将可运行的 goroutine （如果有）添加到 toRun 。 可能在 STW 期间运行，因此不允许写入障碍。
//go:nowritebarrier
func netpollready(toRun *gList, pd *pollDesc, mode int32) {
	var rg, wg *g
	if mode == 'r' || mode == 'r'+'w' {
		rg = netpollunblock(pd, 'r', true)
	}
	if mode == 'w' || mode == 'r'+'w' {
		wg = netpollunblock(pd, 'w', true)
	}
	if rg != nil {
		toRun.push(rg)
	}
	if wg != nil {
		toRun.push(wg)
	}
}

// 超时/关闭检查 返回 0 成功，1 关闭，2 超时
func netpollcheckerr(pd *pollDesc, mode int32) int {
	if pd.closing {
		return 1 // errClosing
	}
	if (mode == 'r' && pd.rd < 0) || (mode == 'w' && pd.wd < 0) {
		return 2 // errTimeout
	}
	return 0
}

// 提交阻塞的 pd ， 在 netpollblock 中调用 gopark 的时候设置的，最后会调用此函数
func netpollblockcommit(gp *g, gpp unsafe.Pointer) bool {
	r := atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp)))
	if r {
		// Bump the count of goroutines waiting for the poller.
		// The scheduler uses this to decide whether to block
		// waiting for the poller if there is nothing else to do.
		// 累计等待轮询的 goroutines
		atomic.Xadd(&netpollWaiters, 1)
	}
	return r
}

// 将 goroutines 标记可运行，加入到运行队列
func netpollgoready(gp *g, traceskip int) {
	// 递减等待轮询的 goroutines
	atomic.Xadd(&netpollWaiters, -1)
	goready(gp, traceskip+1)
}

// returns true if IO is ready, or false if timedout or closed
// waitio - wait only for completed IO, ignore errors
// 阻塞网络轮询，IO 就绪，则返回 true ， 否则当超时或者关闭时返回 false
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	// set the gpp semaphore to WAIT
	for {
		old := *gpp
		// 已就绪
		if old == pdReady {
			*gpp = 0
			return true
		}
		// 此时如果 old != 0 ，则表示有多个 g 在等待同一个 pd
		if old != 0 {
			throw("runtime: double wait")
		}
		// 将 pd 设为 pdWait 状态
		if atomic.Casuintptr(gpp, 0, pdWait) {
			break
		}
	}

	// need to recheck error states after setting gpp to WAIT
	// this is necessary because runtime_pollUnblock/runtime_pollSetDeadline/deadlineimpl
	// do the opposite: store to closing/rd/wd, membarrier, load of rg/wg
	// 如果 waitio 或 没有超时/关闭 则 park 当前的 g
	if waitio || netpollcheckerr(pd, mode) == 0 {
		// park 当前的 g ， 如果 gpp 是 G 指针的话，netpollblockcommit 会对 netpollWaiters 累加
		gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceEvGoBlockNet, 5)
	}
	// be careful to not lose concurrent READY notification
	// 通过原子操作将 gpp 的值设置为 0 ，返回修改前的值并判断是否 pdReady
	old := atomic.Xchguintptr(gpp, 0)
	if old > pdWait {
		throw("runtime: corrupted polldesc")
	}
	return old == pdReady
}

// 非阻塞网络轮询， 返回之前等待的 G
func netpollunblock(pd *pollDesc, mode int32, ioready bool) *g {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	for {
		old := *gpp
		// 已就绪，没有等待的 G
		if old == pdReady {
			return nil
		}
		// 当前 IO 没有就绪
		if old == 0 && !ioready {
			// Only set READY for ioready. runtime_pollWait
			// will check for timeout/cancel before waiting.
			// 只有 ioready 的时候，菜设置 pdReady 状体，这里直接返回
			return nil
		}
		var new uintptr
		if ioready {
			new = pdReady
		}
		// 原子操作，将 gpp 设置为 new
		if atomic.Casuintptr(gpp, old, new) {
			// 如果之前是 pdReady 或 pdWait 状态，则设置为 0 ，因为没有等待的 G ，不是 G 指针
			if old == pdReady || old == pdWait {
				old = 0
			}
			return (*g)(unsafe.Pointer(old))
		}
	}
}

// 超时读写实现
// read/write -> 是否读/写
func netpolldeadlineimpl(pd *pollDesc, seq uintptr, read, write bool) {
	lock(&pd.lock)
	// Seq arg is seq when the timer was set.
	// If it's stale, ignore the timer event.
	// Seq 为定时器设置的时候的序号。如果过时了，请忽略计时器事件。
	currentSeq := pd.rseq
	if !read {
		currentSeq = pd.wseq
	}
	// 过时了，pd 重用了或者定时器重设了
	if seq != currentSeq {
		// The descriptor was reused or timers were reset.
		unlock(&pd.lock)
		return
	}
	// 读
	var rg *g
	if read {
		if pd.rd <= 0 || pd.rt.f == nil {
			throw("runtime: inconsistent read deadline")
		}
		// 设置超时时间 和 回调处理函数
		pd.rd = -1
		atomic.StorepNoWB(unsafe.Pointer(&pd.rt.f), nil) // full memory barrier between store to rd and load of rg in netpollunblock
		// 获取等待读的 G
		rg = netpollunblock(pd, 'r', false)
	}
	// 写
	var wg *g
	if write {
		if pd.wd <= 0 || pd.wt.f == nil && !read {
			throw("runtime: inconsistent write deadline")
		}
		// 设置超时时间 和 回调处理函数
		pd.wd = -1
		atomic.StorepNoWB(unsafe.Pointer(&pd.wt.f), nil) // full memory barrier between store to wd and load of wg in netpollunblock
		// 获取等待写的 G
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	// 如果有等待读的 G ，则唤醒
	if rg != nil {
		netpollgoready(rg, 0)
	}
	// 如果有等待写的 G ，则唤醒
	if wg != nil {
		netpollgoready(wg, 0)
	}
}

// netpollWriteDeadline 超时读写
func netpollDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, true)
}

// netpollWriteDeadline 超时读
func netpollReadDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, false)
}

// netpollWriteDeadline 超时写
func netpollWriteDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, false, true)
}

// 分配 pd
func (c *pollCache) alloc() *pollDesc {
	lock(&c.lock)
	// 如果不够了，
	if c.first == nil {
		const pdSize = unsafe.Sizeof(pollDesc{})
		n := pollBlockSize / pdSize
		if n == 0 {
			n = 1
		}
		// Must be in non-GC memory because can be referenced
		// only from epoll/kqueue internals.
		// 必须位于非 GC 内存中，因为只能从 epoll/kqueue 内部引用。
		// persistentalloc 分配不会被垃圾回收的内存空间
		mem := persistentalloc(n*pdSize, 0, &memstats.other_sys)
		// 串起来
		for i := uintptr(0); i < n; i++ {
			pd := (*pollDesc)(add(mem, i*pdSize))
			pd.link = c.first
			c.first = pd
		}
	}
	pd := c.first
	c.first = pd.link
	unlock(&c.lock)
	return pd
}
