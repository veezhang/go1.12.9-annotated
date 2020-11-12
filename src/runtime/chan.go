// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

// 不变的是：
// c.sendq 和 c.recvq 中至少一个为空，除非是  unbuffered channle 并且一个 goroutine 阻塞在 select 语句上同时发送和接受 ，
// 这时 c.sendq 和 c.recvq 的长度由 select 语句的大小限制。

// 对于 buffered channel，同样：
//  c.qcount > 0 隐含 c.recvq 为空； c.qcount < c.dataqsiz 隐含 c.sendq 为空。

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	maxAlign  = 8
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1))
	debugChan = false
)

type hchan struct {
	qcount   uint           // total data in the queue						// 队列中的所有数据数
	dataqsiz uint           // size of the circular queue					// 环形队列的大小
	buf      unsafe.Pointer // points to an array of dataqsiz elements		// 指向大小为 dataqsiz 的数组
	elemsize uint16         //												// 元素大小
	closed   uint32         //												// 是否关闭
	elemtype *_type         // element type									// 元素类型
	sendx    uint           // send index									// 发送索引
	recvx    uint           // receive index								// 接受索引
	recvq    waitq          // list of recv waiters							// recv 等待列表，即（ <-chan）
	sendq    waitq          // list of send waiters							// send 等待列表，即（ ch<- ）

	// lock protects all fields in hchan, as well as several
	// fields in sudogs blocked on this channel.
	//
	// Do not change another G's status while holding this lock
	// (in particular, do not ready a G), as this can deadlock
	// with stack shrinking.
	//
	// lock 保护了 hchan 的所有字段，以及阻塞在此 channel 上的 sudog 的一些字段。
	// 当持有此锁时不应该改变其他 G 的状态（特别的，不 ready 一个 G），因为它会在栈收缩时发生死锁。
	lock mutex
}

// 等待队列 sudog 双向队列
type waitq struct {
	first *sudog
	last  *sudog
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

// makechan make(type, size)
func makechan(t *chantype, size int) *hchan {
	elem := t.elem

	// compiler checks this but be safe.
	// 编译器检查
	if elem.size >= 1<<16 {
		throw("makechan: invalid channel element type")
	}
	if hchanSize%maxAlign != 0 || elem.align > maxAlign {
		throw("makechan: bad alignment")
	}

	mem, overflow := math.MulUintptr(elem.size, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	// Hchan does not contain pointers interesting for GC when elements stored in buf do not contain pointers.
	// buf points into the same allocation, elemtype is persistent.
	// SudoG's are referenced from their owning thread so they can't be collected.
	// TODO(dvyukov,rlh): Rethink when collector can move allocated objects.
	// hchan 不包含指针时，buf 指向了相同的分配区，elemtype 是持久的。
	// sudoG 则被他们拥有的线程引用，因此他们不会被 GC 。
	var c *hchan
	switch {
	case mem == 0:
		// Queue or element size is zero.
		// 队列或元素大小为零
		c = (*hchan)(mallocgc(hchanSize, nil, true))
		// Race detector uses this location for synchronization.
		// 竞争检查使用此位置进行同步
		c.buf = c.raceaddr()
	case elem.kind&kindNoPointers != 0:
		// Elements do not contain pointers.
		// Allocate hchan and buf in one call.
		// 元素不包含指针，在一个调用中分配 hchan 和 buf ，也就是一起连续分配。减少 GC 压力。
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default:
		// Elements contain pointers.
		// 元素包含指针， buf 单独分配
		c = new(hchan)
		c.buf = mallocgc(mem, elem, true)
	}

	// 初始化
	c.elemsize = uint16(elem.size)
	c.elemtype = elem
	c.dataqsiz = uint(size)

	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.size, "; elemalg=", elem.alg, "; dataqsiz=", size, "\n")
	}
	return c
}

// chanbuf(c, i) is pointer to the i'th slot in the buffer.
// chanbuf(c, i) 返回 buf 的第 i 个槽，也就是 buf[i]
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// entry point for c <- x from compiled code
// 入口点： c <- x
//go:nosplit
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

/*
 * generic single channel send/recv
 * If block is not nil,
 * then the protocol will not
 * sleep but return if it could
 * not complete.
 *
 * sleep can wake up with g.param == nil
 * when a channel involved in the sleep has
 * been closed.  it is easiest to loop and re-run
 * the operation; we'll see that it's now closed.
 */
// chansend 发送
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	// 当向 nil channel 发送数据时，会调用 gopark
	// 而 gopark 会将当前的 goroutine 休眠，并用第一个参数的 unlockf 来回调唤醒
	// 但此处传递的参数为 nil，因此向 channel 发送数据的 goroutine 和接收数据的 goroutine 都会阻塞，
	// 进而死锁
	if c == nil {
		if !block {
			return false
		}
		gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, funcPC(chansend))
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not closed, we observe that the channel is
	// not ready for sending. Each of these observations is a single word-sized read
	// (first c.closed and second c.recvq.first or c.qcount depending on kind of channel).
	// Because a closed channel cannot transition from 'ready for sending' to
	// 'not ready for sending', even if the channel is closed between the two observations,
	// they imply a moment between the two when the channel was both not yet closed
	// and not ready for sending. We behave as if we observed the channel at that moment,
	// and report that the send cannot proceed.
	//
	// It is okay if the reads are reordered here: if we observe that the channel is not
	// ready for sending and then observe that it is not closed, that implies that the
	// channel wasn't closed during the first observation.
	//
	// 快速路径: 在不需要锁的情况下检查失败的非阻塞操作
	// 注意到 channel 不可能从关闭转换为未关闭状态，因此这里的失败条件为：
	// 非阻塞并且还没有关闭，并行满足以下条件之一：
	// 		1. unbuffered channel 并且 没有接收者
	// 		2. buffered channel 并且已满
	if !block && c.closed == 0 && ((c.dataqsiz == 0 && c.recvq.first == nil) ||
		(c.dataqsiz > 0 && c.qcount == c.dataqsiz)) {
		return false
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	// 不允许向已经 close 的 channel 发送数据
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}
	// 找到了阻塞在 channel 上的 reciver，直接发送
	if sg := c.recvq.dequeue(); sg != nil {
		// Found a waiting receiver. We pass the value we want to send
		// directly to the receiver, bypassing the channel buffer (if any).
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	//  判断 channel 中缓存是否仍然有空间剩余
	if c.qcount < c.dataqsiz {
		// Space is available in the channel buffer. Enqueue the element to send.
		// 有空间剩余，入队
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			raceacquire(qp)
			racerelease(qp)
		}
		typedmemmove(c.elemtype, qp, ep)
		c.sendx++
		if c.sendx == c.dataqsiz {
			c.sendx = 0
		}
		c.qcount++
		unlock(&c.lock)
		return true
	}
	// 非阻塞
	if !block {
		unlock(&c.lock)
		return false
	}

	// Block on the channel. Some receiver will complete our operation for us.
	// 阻塞在 channel 上，等待接收方接收数据
	gp := getg()
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	mysg.elem = ep
	mysg.waitlink = nil
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.waiting = mysg
	gp.param = nil
	// 入队
	c.sendq.enqueue(mysg)
	goparkunlock(&c.lock, waitReasonChanSend, traceEvGoBlockSend, 3) // 将当前的 g 从调度队列移出
	// 因为调度器在停止当前 g 的时候会记录运行现场，当恢复阻塞的发送操作时候，会从此处继续开始执行

	// Ensure the value being sent is kept alive until the
	// receiver copies it out. The sudog has a pointer to the
	// stack object, but sudogs aren't considered as roots of the
	// stack tracer.
	// 确保发送的值保持活动状态，直到接收者将其复制出来。 sudog 具有指向堆栈对象的指针，但是 sudog 不被视为堆栈跟踪器的根对象。
	KeepAlive(ep)

	// someone woke us up.
	// 被唤醒
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	if gp.param == nil {
		// 正常唤醒状态，goroutine 应该包含需要传递的参数，但如果没有唤醒时的参数，且 channel 没有被关闭，则为虚假唤醒
		if c.closed == 0 {
			throw("chansend: spurious wakeup")
		}
		panic(plainError("send on closed channel"))
	}
	gp.param = nil
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	mysg.c = nil       // 取消与之前阻塞的 channel 的关联
	releaseSudog(mysg) // 从 sudog 中移除
	return true
}

// send processes a send operation on an empty channel c.
// The value ep sent by the sender is copied to the receiver sg.
// The receiver is then woken up to go on its merry way.
// Channel c must be empty and locked.  send unlocks c with unlockf.
// sg must already be dequeued from c.
// ep must be non-nil and point to the heap or the caller's stack.
// send
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg)
		} else {
			// Pretend we go through the buffer, even though
			// we copy directly. Note that we need to increment
			// the head/tail locations only when raceenabled.
			qp := chanbuf(c, c.recvx)
			raceacquire(qp)
			racerelease(qp)
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
			c.recvx++
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}
	if sg.elem != nil { // 接收者的变量
		sendDirect(c.elemtype, sg, ep) // 直接拷贝过去
		sg.elem = nil
	}
	gp := sg.g
	unlockf() // 拷贝完毕再释放 channel 锁，避免多个发送者；
	gp.param = unsafe.Pointer(sg)
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 复始一个 goroutine，放入调度队列等待被后续调度
	// 第二个参数用于 trace 追踪 ip 寄存器的位置，go runtime 又不希望暴露太多内部的调用，因此记录需要跳过多少 ip
	goready(gp, skip+1) // 唤醒接受者
}

// Sends and receives on unbuffered or empty-buffered channels are the
// only operations where one running goroutine writes to the stack of
// another running goroutine. The GC assumes that stack writes only
// happen when the goroutine is running and are only done by that
// goroutine. Using a write barrier is sufficient to make up for
// violating that assumption, but the write barrier has to work.
// typedmemmove will call bulkBarrierPreWrite, but the target bytes
// are not in the heap, so that will not help. We arrange to call
// memmove and typeBitsBulkBarrier instead.
// sendDirect 直接发送
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// src is on our stack, dst is a slot on another stack.

	// Once we read sg.elem out of sg, it will no longer
	// be updated if the destination's stack gets copied (shrunk).
	// So make sure that no preemption points can happen between read & use.
	// 我们必须在一个函数调用中完成 sg.elem 指针的读取，否则当发生栈伸缩时，指针可能失效（被移动了）。
	dst := sg.elem
	// 为了确保发送的数据能够被立刻观察到，需要写屏障支持，执行写屏障，保证代码正确性
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	// No need for cgo write barrier checks because dst is always
	// Go memory.
	memmove(dst, src, t.size) // 直接写入 reader 的执行栈！
}

// recvDirect 直接读
func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// dst is on our stack or the heap, src is on another stack.
	// The channel is locked, so src will not move during this
	// operation.
	src := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	memmove(dst, src, t.size)
}

// closechan 关闭 chan
func closechan(c *hchan) {
	if c == nil {
		panic(plainError("close of nil channel"))
	}

	lock(&c.lock)
	// 重复关闭
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		racewritepc(c.raceaddr(), callerpc, funcPC(closechan))
		racerelease(c.raceaddr())
	}

	c.closed = 1

	var glist gList

	// release all readers
	// 释放所有的 readers
	for {
		// 出栈一个 reader
		sg := c.recvq.dequeue()
		if sg == nil {
			break
		}
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem)
			sg.elem = nil
		}
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = nil
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		// 放到 glist 中，后面解锁后再一起在唤醒
		glist.push(gp)
	}

	// release all writers (they will panic)
	// 释放所有的 writers (panic)
	for {
		// 出栈一个 writer
		sg := c.sendq.dequeue()
		if sg == nil {
			break
		}
		sg.elem = nil
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = nil
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		// 放到 glist 中，后面解锁后再一起在唤醒
		glist.push(gp)
	}
	unlock(&c.lock)

	// Ready all Gs now that we've dropped the channel lock.
	// 释放 chan 锁之后就绪所有的 G
	for !glist.empty() {
		gp := glist.pop()
		gp.schedlink = 0
		goready(gp, 3)
	}
}

// entry points for <- c from compiled code
//go:nosplit
// 入口点： <- c
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}

//go:nosplit
// 入口点： <- c
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
// A non-nil ep must point to the heap or the caller's stack.
// chanrecv 在 channel 上接受，并将收到的数据写到 ep 中。ep 可能为 nil ，此时收到的数据被忽略。
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	// raceenabled: don't need to check ep, as it is always on the stack
	// or is new memory allocated by reflect.

	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	// nil channel，同 send，会导致两个 goroutine 的死锁
	if c == nil {
		if !block {
			return
		}
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not ready for receiving, we observe that the
	// channel is not closed. Each of these observations is a single word-sized read
	// (first c.sendq.first or c.qcount, and second c.closed).
	// Because a channel cannot be reopened, the later observation of the channel
	// being not closed implies that it was also not closed at the moment of the
	// first observation. We behave as if we observed the channel at that moment
	// and report that the receive cannot proceed.
	//
	// The order of operations is important here: reversing the operations can lead to
	// incorrect behavior when racing with a close.
	// 快速路径: 在不需要锁的情况下检查失败的非阻塞操作。
	// 注意到 channel 不能由已关闭转换为未关闭，则失败的条件是：1. 无 buf 时发送队列为空 2. 有 buf 时，buf 为空
	// 此处的 c.closed 必须在条件判断之后进行验证，因为指令重排后，如果先判断 c.closed，得出 channel 未关闭，无法判断失败条件中
	// channel 是已关闭还是未关闭（从而需要 atomic 操作）
	if !block && (c.dataqsiz == 0 && c.sendq.first == nil ||
		c.dataqsiz > 0 && atomic.Loaduint(&c.qcount) == 0) &&
		atomic.Load(&c.closed) == 0 {
		return
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	// channel 已经 close，且 channel 中没有数据，则直接返回
	if c.closed != 0 && c.qcount == 0 {
		if raceenabled {
			raceacquire(c.raceaddr())
		}
		unlock(&c.lock)
		if ep != nil {
			typedmemclr(c.elemtype, ep)
		}
		return true, false
	}

	// 找到了阻塞在 channel 上的 writer ，直接接收
	if sg := c.sendq.dequeue(); sg != nil {
		// Found a waiting sender. If buffer is size 0, receive value
		// directly from sender. Otherwise, receive from head of queue
		// and add sender's value to the tail of the queue (both map to
		// the same buffer slot because the queue is full).
		recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true, true
	}

	// channel 的 buf 不空，放到 buf 中
	if c.qcount > 0 {
		// Receive directly from queue
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			raceacquire(qp)
			racerelease(qp)
		}
		if ep != nil {
			typedmemmove(c.elemtype, ep, qp)
		}
		typedmemclr(c.elemtype, qp)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.qcount--
		unlock(&c.lock)
		return true, true
	}

	if !block {
		unlock(&c.lock)
		return false, false
	}

	// no sender available: block on this channel.
	// 没有更多的 sender ，阻塞 channel
	gp := getg()
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	mysg.elem = ep
	mysg.waitlink = nil
	gp.waiting = mysg
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.param = nil
	c.recvq.enqueue(mysg)
	goparkunlock(&c.lock, waitReasonChanReceive, traceEvGoBlockRecv, 3) // 将当前的 g 从调度队列移出
	// 因为调度器在停止当前 g 的时候会记录运行现场，当恢复阻塞的发送操作时候，会从此处继续开始执行

	// someone woke us up
	// 被唤醒
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	closed := gp.param == nil
	gp.param = nil
	mysg.c = nil
	releaseSudog(mysg)
	return true, !closed
}

// recv processes a receive operation on a full channel c.
// There are 2 parts:
// 1) The value sent by the sender sg is put into the channel
//    and the sender is woken up to go on its merry way.
// 2) The value received by the receiver (the current G) is
//    written to ep.
// For synchronous channels, both values are the same.
// For asynchronous channels, the receiver gets its data from
// the channel buffer and the sender's data is put in the
// channel buffer.
// Channel c must be full and locked. recv unlocks c with unlockf.
// sg must already be dequeued from c.
// A non-nil ep must point to the heap or the caller's stack.
// recv
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if c.dataqsiz == 0 { // 无缓冲区 chan
		if raceenabled {
			racesync(c, sg)
		}
		if ep != nil {
			// copy data from sender
			// 直接从对方的栈进行拷贝
			recvDirect(c.elemtype, sg, ep)
		}
	} else {
		// Queue is full. Take the item at the
		// head of the queue. Make the sender enqueue
		// its item at the tail of the queue. Since the
		// queue is full, those are both the same slot.
		// 从缓存队列拷贝
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			raceacquire(qp)
			racerelease(qp)
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}
		// copy data from queue to receiver
		// 从队列拷贝数据到接收方
		if ep != nil {
			typedmemmove(c.elemtype, ep, qp)
		}
		// copy data from sender to queue
		// 从发送方拷贝数据到队列
		typedmemmove(c.elemtype, qp, sg.elem)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	sg.elem = nil
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	goready(gp, skip+1)
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
//
// selectnbsend select 中的非阻塞发
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbrecv(&v, c) {
//		... foo
//	} else {
//		... bar
//	}
//
// selectnbsend select 中的非阻塞读(v = <-c)
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected bool) {
	selected, _ = chanrecv(c, elem, false)
	return
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if c != nil && selectnbrecv2(&v, &ok, c) {
//		... foo
//	} else {
//		... bar
//	}
//
// selectnbsend select 中的非阻塞读(v, ok = <-c)
func selectnbrecv2(elem unsafe.Pointer, received *bool, c *hchan) (selected bool) {
	// TODO(khr): just return 2 values from this function, now that it is in Go.
	selected, *received = chanrecv(c, elem, false)
	return
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

// enqueue 入队
func (q *waitq) enqueue(sgp *sudog) {
	sgp.next = nil
	x := q.last
	if x == nil { // 此时队列空
		sgp.prev = nil // 将 sgp 插入到队列中
		q.first = sgp  // 队首指针指向 sgp
		q.last = sgp   // 队尾指针指向 sgp
		return
	}
	// 此时队列不空
	sgp.prev = x
	x.next = sgp // sgp 放到最后一个 sgp 的后面
	q.last = sgp // 修改队尾指针
}

// dequeue 出队
func (q *waitq) dequeue() *sudog {
	for {
		// 获得队首 sgp
		sgp := q.first
		if sgp == nil { // 如果队列空，直接返回
			return nil
		}
		// 获得队首的下一个元素
		y := sgp.next
		if y == nil { // 如果此时队列已空，清空队首队尾指针
			q.first = nil
			q.last = nil
		} else { // 否则将队首指针移到下一个元素
			y.prev = nil
			q.first = y
			sgp.next = nil // mark as removed (see dequeueSudog) // 标记为已移除 (see dequeueSudog)
		}

		// if a goroutine was put on this queue because of a
		// select, there is a small window between the goroutine
		// being woken up by a different case and it grabbing the
		// channel locks. Once it has the lock
		// it removes itself from the queue, so we won't see it after that.
		// We use a flag in the G struct to tell us when someone
		// else has won the race to signal this goroutine but the goroutine
		// hasn't removed itself from the queue yet.
		//
		// 如果由于 select 而将 goroutine 放在此队列中，则在由不同的情况唤醒的 goroutine 之间
		// 存在一个小窗口并且它获取 channel lock。 一旦锁定，它会将自己从队列中删除，因此我们不
		// 会在那之后看到它。
		// 我们在 G 结构中使用一个标志来告诉我们，其他人何时已经获得了这个竞争条件以告知这个 goroutine，
		// 但是 goroutine 还没有将自己从队列中移除。
		if sgp.isSelect {
			// 正在参与一个 select，如果将 g 标记为已完成失败，则说明此 sgp 已经不在队列中，继续循环并重新出队
			if !atomic.Cas(&sgp.g.selectDone, 0, 1) {
				continue
			}
		}

		return sgp
	}
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}
