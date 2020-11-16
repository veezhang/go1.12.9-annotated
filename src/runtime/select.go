// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go select statements.

import (
	"unsafe"
)

const debugSelect = false

// scase.kind values.
// Known to compiler.
// Changes here must also be made in src/cmd/compile/internal/gc/select.go's walkselect.
const (
	caseNil     = iota // 0 ：表示case 为nil；在send 或者 recv 发生在一个 nil channel 上，就有可能出现这种情况
	caseRecv           // 1 : 表示case 为接收通道 <- ch
	caseSend           // 2 ：表示case 为发送通道 ch <-
	caseDefault        // 3 ：表示 default 语句块
)

// Select case descriptor.
// Known to compiler.
// Changes here must also be made in src/cmd/internal/gc/select.go's scasetype.
// select case 描述
type scase struct {
	c *hchan // chan									// 表示当前 case 语句操作的 chan 指针
	// 表示缓冲区地址
	// scase.kind == caseRecv ： elem 表示读出 channel 的数据存放地址
	// scase.kind == caseSend ： elem 表示将要写入 channel 的数据存放地址
	elem unsafe.Pointer // data element
	// 表示当前的 chan 的类型
	kind        uint16  // 上述的四种 kind ： caseNil,caseRecv,caseSend,caseDefault
	pc          uintptr // race pc (for race detector / msan)
	releasetime int64
}

var (
	chansendpc = funcPC(chansend)
	chanrecvpc = funcPC(chanrecv)
)

// selectsetpc 设置 cas.pc 为调用者 pc
func selectsetpc(cas *scase) {
	cas.pc = getcallerpc()
}

// sellock 加锁所有的 channel
func sellock(scases []scase, lockorder []uint16) {
	var c *hchan
	for _, o := range lockorder {
		c0 := scases[o].c // 根据加锁顺序获取 case
		// c 记录了上次加锁的 hchan 地址，如果和当前 *hchan 相同，那么就不会再次加锁
		if c0 != nil && c0 != c {
			c = c0
			lock(&c.lock)
		}
	}
}

// selunlock 根据 lockorder 解锁 channel
func selunlock(scases []scase, lockorder []uint16) {
	// We must be very careful here to not touch sel after we have unlocked
	// the last lock, because sel can be freed right after the last unlock.
	// Consider the following situation.
	// First M calls runtime·park() in runtime·selectgo() passing the sel.
	// Once runtime·park() has unlocked the last lock, another M makes
	// the G that calls select runnable again and schedules it for execution.
	// When the G runs on another M, it locks all the locks and frees sel.
	// Now if the first M touches sel, it will access freed memory.
	// 我们必须非常小心，在解锁最后一个锁之后不要触摸sel，因为sel可以在最后一次解锁后立即释放。
	// 考虑以下情况：第一个 M 通过 sel 调用 runtime·selectgo() 中的 runtime·park() 。
	// 一旦 runtime·park() 解锁了最后一个锁，另一个 M 使调用 select runnable 的 G 再次执行。
	// 现在，如果第一个 M 碰到 sel ，它将访问已释放的内存。
	for i := len(scases) - 1; i >= 0; i-- {
		c := scases[lockorder[i]].c
		if c == nil {
			break
		}
		// c 记录了上次加锁的 hchan 地址，如果和前面的 *hchan 相同，那么就下一次再解锁
		if i > 0 && c == scases[lockorder[i-1]].c {
			continue // will unlock it on the next iteration
		}
		unlock(&c.lock)
	}
}

// selparkcommit 根据等待列表依次解锁所有 channel
func selparkcommit(gp *g, _ unsafe.Pointer) bool {
	// This must not access gp's stack (see gopark). In
	// particular, it must not access the *hselect. That's okay,
	// because by the time this is called, gp.waiting has all
	// channels in lock order.
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc && lastc != nil {
			// As soon as we unlock the channel, fields in
			// any sudog with that channel may change,
			// including c and waitlink. Since multiple
			// sudogs may have the same channel, we unlock
			// only after we've passed the last instance
			// of a channel.
			unlock(&lastc.lock)
		}
		lastc = sg.c
	}
	if lastc != nil {
		unlock(&lastc.lock)
	}
	return true
}

// block 永远阻塞
func block() {
	gopark(nil, nil, waitReasonSelectNoCases, traceEvGoStop, 1) // forever
}

// selectgo implements the select statement.
//
// cas0 points to an array of type [ncases]scase, and order0 points to
// an array of type [2*ncases]uint16. Both reside on the goroutine's
// stack (regardless of any escaping in selectgo).
//
// selectgo returns the index of the chosen scase, which matches the
// ordinal position of its respective select{recv,send,default} call.
// Also, if the chosen scase was a receive operation, it reports whether
// a value was received.
// selectgo 实现 select 语法声明。
// cas0 指向一个类型为 [ncases]scase 的数组。
// order0 指向 [2*ncases]uint16 ，数组中的值都是 0 。
// selectgo 会返回选中的序号，如果是个接收操作，还会返回是否接收到一个值
func selectgo(cas0 *scase, order0 *uint16, ncases int) (int, bool) {
	if debugSelect {
		print("select: cas0=", cas0, "\n")
	}

	// cas1,order1 是将其转换为数组
	cas1 := (*[1 << 16]scase)(unsafe.Pointer(cas0))
	order1 := (*[1 << 17]uint16)(unsafe.Pointer(order0))

	// 分别获取 scases, pollorder, lockorder, [:n:n] 的方式会让slice 的 len 和 cap 相等
	scases := cas1[:ncases:ncases]
	pollorder := order1[:ncases:ncases]
	lockorder := order1[ncases:][:ncases:ncases]

	// Replace send/receive cases involving nil channels with
	// caseNil so logic below can assume non-nil channel.
	// cas.c == nil 的时候， 用 caseNil 替换，以便下面的逻辑不用处理非 nil 的情况。
	for i := range scases {
		cas := &scases[i]
		if cas.c == nil && cas.kind != caseDefault {
			*cas = scase{}
		}
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
		for i := 0; i < ncases; i++ {
			scases[i].releasetime = -1
		}
	}

	// The compiler rewrites selects that statically have
	// only 0 or 1 cases plus default into simpler constructs.
	// The only way we can end up with such small sel.ncase
	// values here is for a larger select in which most channels
	// have been nilled out. The general code handles those
	// cases correctly, and they are rare enough not to bother
	// optimizing (and needing to test).

	// generate permuted order
	// 生成随机顺序，pollorder 循环结束后值便是随机顺序的 scases 索引
	for i := 1; i < ncases; i++ {
		j := fastrandn(uint32(i + 1))
		pollorder[i] = pollorder[j]
		pollorder[j] = uint16(i)
	}

	// sort the cases by Hchan address to get the locking order.
	// simple heap sort, to guarantee n log n time and constant stack footprint.
	// 加锁前首先会对 lockorder 进行堆排序，生成由 case.c(*hchan) 来排序的 scases 索引顺序
	for i := 0; i < ncases; i++ {
		j := i
		// Start with the pollorder to permute cases on the same channel.
		// 根据 pollorder 开始进而在同一 channel 上排序所有 case
		c := scases[pollorder[i]].c
		for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
			k := (j - 1) / 2
			lockorder[j] = lockorder[k]
			j = k
		}
		lockorder[j] = pollorder[i]
	}
	for i := ncases - 1; i >= 0; i-- {
		o := lockorder[i]
		c := scases[o].c
		lockorder[i] = lockorder[0]
		j := 0
		for {
			k := j*2 + 1
			if k >= i {
				break
			}
			if k+1 < i && scases[lockorder[k]].c.sortkey() < scases[lockorder[k+1]].c.sortkey() {
				k++
			}
			if c.sortkey() < scases[lockorder[k]].c.sortkey() {
				lockorder[j] = lockorder[k]
				j = k
				continue
			}
			break
		}
		lockorder[j] = o
	}

	if debugSelect {
		for i := 0; i+1 < ncases; i++ {
			if scases[lockorder[i]].c.sortkey() > scases[lockorder[i+1]].c.sortkey() {
				print("i=", i, " x=", lockorder[i], " y=", lockorder[i+1], "\n")
				throw("select: broken sort")
			}
		}
	}

	// lock all the channels involved in the select
	// selectgo 在查找 scases 前，先对所有 channel 加锁
	sellock(scases, lockorder)

	var (
		gp     *g
		sg     *sudog
		c      *hchan
		k      *scase
		sglist *sudog
		sgnext *sudog
		qp     unsafe.Pointer
		nextp  **sudog
	)

loop:
	// pass 1 - look for something already waiting
	// 1. 首先根据 pollorder 的顺序查找 scases 是否有可以立即收发的 channel
	var dfli int
	var dfl *scase
	var casi int
	var cas *scase
	var recvOK bool
	for i := 0; i < ncases; i++ {
		casi = int(pollorder[i]) // 随机顺序
		cas = &scases[casi]
		c = cas.c

		switch cas.kind {
		case caseNil: // 没有对应的 chan ，则继续
			continue

		case caseRecv: // 收的 chan
			// 如果 channel 中有待发送的 goroutine ， 跳转到 recv ，调用 recv 完成接收操作
			sg = c.sendq.dequeue()
			if sg != nil {
				goto recv
			}
			// 如果 channel 中有缓冲数据，那么跳转到 bufrecv ，从缓冲区中获取数据
			if c.qcount > 0 {
				goto bufrecv
			}
			// 如果 channel 已关闭，跳转到 rclose, 将接收值置为空值，recvOK 置为 false
			if c.closed != 0 {
				goto rclose
			}

		case caseSend: // 发的 chan
			if raceenabled {
				racereadpc(c.raceaddr(), cas.pc, chansendpc)
			}
			// 对于发送操作会先判断 channel 是否已经关闭，跳转到 sclose，直接 panic
			if c.closed != 0 {
				goto sclose
			}
			// 如果 channel 为关闭，并且有待接收队列不为空，说明 channel 的缓冲区为空，跳转到 send , 调用 send 函数，直接发送数据给待接收者
			sg = c.recvq.dequeue()
			if sg != nil {
				goto send
			}
			// 如果缓冲区不为空的话，跳转到 bufsend，从缓冲区获取数据
			if c.qcount < c.dataqsiz {
				goto bufsend
			}

		case caseDefault: // default:
			// dfli 和 dfl 记录了 kind 为 caseDefault 的 case
			dfli = casi
			dfl = cas
		}
	}

	// 存在 default 分支，直接去 retc 执行
	if dfl != nil {
		selunlock(scases, lockorder)
		casi = dfli
		cas = dfl
		goto retc
	}
	// 如果没有 channel 可以执行收发操作，并且没有 default case，那么就将当前 goroutine 加入到 channel 相应的收发队列中，等待被其他 goroutine 唤醒
	// pass 2 - enqueue on all chans
	// 2. 将当前 goroutine 加入到每一个 channel 等待队列中
	gp = getg()
	if gp.waiting != nil {
		throw("gp.waiting != nil")
	}
	nextp = &gp.waiting
	for _, casei := range lockorder {
		casi = int(casei)
		cas = &scases[casi]
		if cas.kind == caseNil {
			continue // channel 为 nil 直接跳过
		}
		// 这里为 caseRecv 或者 caseSend (不可能是 caseDefault 了)
		c = cas.c
		// 构造 sudog
		sg := acquireSudog()
		sg.g = gp
		sg.isSelect = true
		// No stack splits between assigning elem and enqueuing
		// sg on gp.waiting where copystack can find it.
		// 在 gp.waiting 上分配 elem 和入队 sg 之间没有栈分段，copystack 可以在其中找到它。
		sg.elem = cas.elem
		sg.releasetime = 0
		if t0 != 0 {
			sg.releasetime = -1
		}
		sg.c = c
		// Construct waiting list in lock order.
		// 按锁的顺序创建等待链表
		*nextp = sg
		nextp = &sg.waitlink

		// 加入相应等待队列
		switch cas.kind {
		case caseRecv:
			c.recvq.enqueue(sg)

		case caseSend:
			c.sendq.enqueue(sg)
		}
	}

	// wait for someone to wake us up
	// 等待被唤醒
	gp.param = nil // 被唤醒后会根据 param 来判断是否是由 close 操作唤醒的，所以先置为 nil
	// selparkcommit 根据等待列表依次解锁所有 channel
	gopark(selparkcommit, nil, waitReasonSelect, traceEvGoBlockSelect, 1)

	// 加锁所有的 channel
	sellock(scases, lockorder)

	gp.selectDone = 0
	sg = (*sudog)(gp.param)
	// param 存放唤醒 goroutine 的 sudog，如果是关闭操作唤醒的，那么就为 nil
	gp.param = nil

	// pass 3 - dequeue from unsuccessful chans
	// otherwise they stack up on quiet channels
	// record the successful case, if any.
	// We singly-linked up the SudoGs in lock order.
	// pass 3 - 从不成功的 channel 中出队，否则将它们堆到一个安静的 channel 上并记录所有成功的分支。我们按锁的顺序单向链接 sudog
	casi = -1
	cas = nil           // cas 便是唤醒 goroutine 的 case
	sglist = gp.waiting // waiting 链表按照 lockorder 顺序存放着 sudog
	// Clear all elem before unlinking from gp.waiting.
	// 从 gp.waiting 取消链接之前清除所有的 elem
	for sg1 := gp.waiting; sg1 != nil; sg1 = sg1.waitlink {
		sg1.isSelect = false
		sg1.elem = nil
		sg1.c = nil
	}
	gp.waiting = nil

	for _, casei := range lockorder {
		k = &scases[casei]
		if k.kind == caseNil {
			continue
		}
		if sglist.releasetime > 0 {
			k.releasetime = sglist.releasetime
		}
		// 如果相等说明，goroutine 是被当前 case 的 channel 收发操作唤醒的
		// 如果是关闭操作，那么 sg 为 nil, 不会对 cas 赋值
		if sg == sglist {
			// sg has already been dequeued by the G that woke us up.
			// sg 已经被我们自己唤醒了
			casi = int(casei)
			cas = k
		} else {
			// goroutine 已经被唤醒，将 sudog 从相应的收发队列中移除
			c = k.c
			// dequeueSudoG 会通过 sudog.prev 和 sudog.next 将 sudog 从等待队列中移除
			if k.kind == caseSend {
				c.sendq.dequeueSudoG(sglist)
			} else {
				c.recvq.dequeueSudoG(sglist)
			}
		}
		// 释放 sudog，然后准备处理下一个 sudog
		sgnext = sglist.waitlink
		sglist.waitlink = nil
		releaseSudog(sglist)
		sglist = sgnext
	}

	if cas == nil {
		// We can wake up with gp.param == nil (so cas == nil)
		// when a channel involved in the select has been closed.
		// It is easiest to loop and re-run the operation;
		// we'll see that it's now closed.
		// Maybe some day we can signal the close explicitly,
		// but we'd have to distinguish close-on-reader from close-on-writer.
		// It's easiest not to duplicate the code and just recheck above.
		// We know that something closed, and things never un-close,
		// so we won't block again.
		// 当一个参与在 select 语句中的 channel 被关闭时，我们可以在 gp.param == nil 时进行唤醒(所以 cas == nil)
		// 最简单的方法就是循环并重新运行该操作，然后就能看到它现在已经被关闭了
		// 也许未来我们可以显式的发送关闭信号，
		// 但我们就必须区分在接收方上关闭(close-on-reader)和在写入方上关闭(close-on-writer)这两种情况了
		// 最简单的方法是不复制代码并重新检查上面的代码。
		// 我们知道某些 channel 被关闭了，也知道某些可能永远不会被重新打开，因此我们不会再次阻塞
		// 由关闭操作唤醒 goroutine，那么再次回到 loop 处
		goto loop
	}

	c = cas.c

	if debugSelect {
		print("wait-return: cas0=", cas0, " c=", c, " cas=", cas, " kind=", cas.kind, "\n")
	}

	if cas.kind == caseRecv {
		recvOK = true
	}

	if raceenabled {
		if cas.kind == caseRecv && cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, cas.pc, chanrecvpc)
		} else if cas.kind == caseSend {
			raceReadObjectPC(c.elemtype, cas.elem, cas.pc, chansendpc)
		}
	}
	if msanenabled {
		if cas.kind == caseRecv && cas.elem != nil {
			msanwrite(cas.elem, c.elemtype.size)
		} else if cas.kind == caseSend {
			msanread(cas.elem, c.elemtype.size)
		}
	}

	selunlock(scases, lockorder)
	goto retc

bufrecv: // 如果 channel 中有缓冲数据，那么跳转到 bufrecv ，从缓冲区中获取数据
	// can receive from buffer
	if raceenabled {
		if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, cas.pc, chanrecvpc)
		}
		raceacquire(chanbuf(c, c.recvx))
		racerelease(chanbuf(c, c.recvx))
	}
	if msanenabled && cas.elem != nil {
		msanwrite(cas.elem, c.elemtype.size)
	}
	recvOK = true
	qp = chanbuf(c, c.recvx)
	if cas.elem != nil {
		typedmemmove(c.elemtype, cas.elem, qp)
	}
	typedmemclr(c.elemtype, qp)
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
	selunlock(scases, lockorder)
	goto retc

bufsend: // 如果缓冲区不为空的话，跳转到 bufsend，从缓冲区获取数据
	// can send to buffer
	// 可以发送到 buf
	if raceenabled {
		raceacquire(chanbuf(c, c.sendx))
		racerelease(chanbuf(c, c.sendx))
		raceReadObjectPC(c.elemtype, cas.elem, cas.pc, chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	typedmemmove(c.elemtype, chanbuf(c, c.sendx), cas.elem)
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
	selunlock(scases, lockorder)
	goto retc

recv: // 如果 channel 中有待发送的 goroutine ， 跳转到 recv ，调用 recv 完成接收操作
	// can receive from sleeping sender (sg)
	recv(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncrecv: cas0=", cas0, " c=", c, "\n")
	}
	recvOK = true
	goto retc

rclose: // 如果 channel 已关闭，跳转到 rclose, 将接收值置为空值，recvOK 置为 false
	// read at end of closed channel
	selunlock(scases, lockorder)
	recvOK = false
	if cas.elem != nil {
		typedmemclr(c.elemtype, cas.elem)
	}
	if raceenabled {
		raceacquire(c.raceaddr())
	}
	goto retc

send: // 如果 channel 为关闭，并且有待接收队列不为空，说明 channel 的缓冲区为空，跳转到 send , 调用 send 函数，直接发送数据给待接收者
	// can send to a sleeping receiver (sg)
	// 可以向一个休眠的接收方 (sg) 发送
	if raceenabled {
		raceReadObjectPC(c.elemtype, cas.elem, cas.pc, chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	send(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncsend: cas0=", cas0, " c=", c, "\n")
	}
	goto retc

retc: // 返回
	if cas.releasetime > 0 {
		blockevent(cas.releasetime-t0, 1)
	}
	return casi, recvOK

sclose: // 对于发送操作会先判断 channel 是否已经关闭，跳转到 sclose，直接 panic
	// send on closed channel
	selunlock(scases, lockorder)
	panic(plainError("send on closed channel"))
}

func (c *hchan) sortkey() uintptr {
	// TODO(khr): if we have a moving garbage collector, we'll need to
	// change this function.
	return uintptr(unsafe.Pointer(c))
}

// A runtimeSelect is a single case passed to rselect.
// This must match ../reflect/value.go:/runtimeSelect
type runtimeSelect struct {
	dir selectDir
	typ unsafe.Pointer // channel type (not used here)
	ch  *hchan         // channel
	val unsafe.Pointer // ptr to data (SendDir) or ptr to receive buffer (RecvDir)
}

// These values must match ../reflect/value.go:/SelectDir.
type selectDir int

const (
	_             selectDir = iota
	selectSend              // case Chan <- Send
	selectRecv              // case <-Chan:
	selectDefault           // default
)

//go:linkname reflect_rselect reflect.rselect
func reflect_rselect(cases []runtimeSelect) (int, bool) {
	if len(cases) == 0 {
		block()
	}
	sel := make([]scase, len(cases))
	order := make([]uint16, 2*len(cases))
	for i := range cases {
		rc := &cases[i]
		switch rc.dir {
		case selectDefault:
			sel[i] = scase{kind: caseDefault}
		case selectSend:
			sel[i] = scase{kind: caseSend, c: rc.ch, elem: rc.val}
		case selectRecv:
			sel[i] = scase{kind: caseRecv, c: rc.ch, elem: rc.val}
		}
		if raceenabled || msanenabled {
			selectsetpc(&sel[i])
		}
	}

	return selectgo(&sel[0], &order[0], len(cases))
}

func (q *waitq) dequeueSudoG(sgp *sudog) {
	x := sgp.prev
	y := sgp.next
	if x != nil {
		if y != nil {
			// middle of queue
			x.next = y
			y.prev = x
			sgp.next = nil
			sgp.prev = nil
			return
		}
		// end of queue
		x.next = nil
		q.last = x
		sgp.prev = nil
		return
	}
	if y != nil {
		// start of queue
		y.prev = nil
		q.first = y
		sgp.next = nil
		return
	}

	// x==y==nil. Either sgp is the only element in the queue,
	// or it has already been removed. Use q.first to disambiguate.
	if q.first == sgp {
		q.first = nil
		q.last = nil
	}
}
