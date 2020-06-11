// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// defined constants
const (
	// G status
	// G状态
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's stack (and hence its
	// ability to execute user code).
	//
	// If you add to this list, add to the list
	// of "okay during garbage collection" status
	// in mgcmark.go too.
	// 除了指示 G 的一般状态之外，G 的状态就类似于 goroutine 的堆栈的一个锁（即它执行用户代码的能力）。
	// 如果你在给此列表增加内容，还需要添加到 mgcmark.go 中的 “垃圾收集期间的正常” 状态列表中。

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized.
	//  _Gidle 表示此 goroutine 已分配，尚未初始化。
	_Gidle = iota // 0

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The stack is not owned.
	//  _Grunnable 表示此 goroutine 在运行队列中。当前未执行用户代码。还未拥有运行栈。
	_Grunnable // 1

	// _Grunning means this goroutine may execute user code. The
	// stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P.
	//  _Grunning 表示此 goroutine 可能在运行用户代码。堆栈由该 goroutine 拥有。它不在运行队列中。它被分配了一个M和一个P。
	_Grunning // 2

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M.
	//  _Gsyscall 表示此 goroutine 正在执行系统调用，没有执行用户代码。该 goroutine 拥有一个栈且不在运行队列中，并分配有一个 M。
	_Gsyscall // 3

	// _Gwaiting means this goroutine is blocked in the runtime.
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere (e.g., a channel wait
	// queue) so it can be ready()d when necessary. The stack is
	// not owned *except* that a channel operation may read or
	// write parts of the stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the stack after a
	// goroutine enters _Gwaiting (e.g., it may get moved).
	//  _Gwaiting 表示当前 goroutine 在运行时中被阻塞。它没有执行用户代码。它不在运行队列中，但应该记录在某处（比如一个 channel 等待队列），
	// 因此可以在需要时 ready()。除了 channel 操作可以在适当的 channel 锁定下读取或写入堆栈的部分之外，它不拥有堆栈。否则，在 goroutine 进
	// 入 _Gwaiting 后访问堆栈是不安全的（例如，它可能会被移动）。
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	//  _Gmoribund_unused 当前未使用，但在 gdb 脚本中进行了硬编码。
	_Gmoribund_unused // 5

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized. It
	// is not executing user code. It may or may not have a stack
	// allocated. The G and its stack (if any) are owned by the M
	// that is exiting the G or that obtained the G from the free
	// list.
	//  _Gdead 表示当前 goroutine 当前未被使用。它可能刚被执行、在释放列表中、或刚刚被初始化。它没有执行用户代码。它可能有也可能没有分配的栈。
	//  G 及其栈（如果有）由正在退出 G 或从释放列表获得 G 的 M 拥有。
	_Gdead // 6

	// _Genqueue_unused is currently unused.
	//  _Genqueue_unused 当前未使用
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	//  _Gcopystack 表示此 goroutine 的栈正在移动。它不执行用户代码，也不在运行队列中。栈由将其放入 _Gcopystack 的goroutine 拥有。
	_Gcopystack // 8

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//  _Gscan 与上述状态中除 _Grunning 以外的一个组合表示 GC 正在清扫堆栈。 goroutine 未执行用户代码，并且堆栈由设置 _Gscan 标志位的 goroutine 拥有。
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//  _Gscanrunning 是不同的： _GCscanrunning 是用来短暂阻塞状态转换的，而 GC 会通知 G 清扫其自己的堆栈。其他的就像 _Grunning 。
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	//  atomicstatus&~Gscan 给出 goroutine 将在清扫完成时返回的状态。
	_Gscan         = 0x1000
	_Gscanrunnable = _Gscan + _Grunnable // 0x1001
	_Gscanrunning  = _Gscan + _Grunning  // 0x1002
	_Gscansyscall  = _Gscan + _Gsyscall  // 0x1003
	_Gscanwaiting  = _Gscan + _Gwaiting  // 0x1004
)

const (
	// P status
	// P状态
	_Pidle    = iota
	_Prunning // Only this P is allowed to change from _Prunning.
	_Psyscall
	_Pgcstop
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// 互斥锁。在无竞争的情况下，与自旋锁 spin lock（只是一些用户级指令）一样快，但在争用路径 contention path 中，它们在内核中休眠。]
// 零值互斥锁为未加锁状态（无需初始化每个锁）。
type mutex struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	// 基于 futex 的实现将其视为 uint32 key (linux)，而基于 sema 实现则将其视为 M* waitm。 (darwin)，以前作为 union 使用，但 union 会打破精确 GC。
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup.
// 休眠与唤醒一次性事件。在任何调用 notesleep 或 notewakeup 之前，必须调用 noteclear 来初始化这个 note，且只能有一个线程调用 notewakeup 一次。
// 旦 notewakeup 被调用后，notesleep 会返回。随后的 notesleep 调用则会立即返回。后续的 noteclear 必须在前一个 notesleep 返回前调用，例如 notewakeup
// 调用后直接调用 noteclear 是不允许的。
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//  notetsleep 类似于 notesleep 但会在给定数量的纳秒时间后唤醒，即使事件尚未发生。 如果一个 goroutine 使用 notetsleep 来提前唤醒，则必须等待调用 noteclear，
// 直到可以确定没有其他 goroutine 正在调用 notewakeup。
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g.
//  notesleep/notetsleep 通常在 g0 上调用，notetsleepg 类似于 notetsleep 但会在用户 g 上调用。
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	// 基于 futex 的实现将其视为 uint32 key (linux)，而基于 sema 实现则将其视为 M* waitm。 (darwin)，以前作为 union 使用，但 union 会打破精确 GC。
	key uintptr
}

// 函数类型
type funcval struct {
	fn uintptr
	// variable-size, fn-specific data here
	// 变长大小，fn 的数据在应在 fn 之后
}

// 接口类型，有方法的接口
type iface struct {
	tab  *itab
	data unsafe.Pointer
}

// 接口类型，空接口
type eface struct {
	_type *_type
	data  unsafe.Pointer
}

// interface转eface
func efaceOf(ep *interface{}) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//  guintptr, muintptr 和 puintptr 均用于绕过 write barriers。释放当前 P 时避免 write barriers 尤为重要，因为 GC 认为 STW，
// 并且意外的 write barriers 不会与 GC 同步，这可能导致半执行的 write barriers : 标记了对象但未将其排队。如果 GC 跳过对象并在排
// 队之前完成，它将错误地释放对象。
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
// 我们尝试使用仅在不持有运行P的情况下才调用的特殊赋值函数，但随后对特定内存字节的某些更新会遇到 write barriers，而某些则不会。这打
// 破了 write barriers 阴影检查模式，这也很可怕：拥有一个被 GC 完全忽略的字节比拥有一个只被很少更新的字节更好。
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//  Gs 和 Ps 始终可以通过 allgs 和 allp 列表或（从分配到添加到列表之前）栈变量中的真实指针访问。
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.
// 总是可以通过来自 allm 或 freem 的真实指针来访问 Ms。与 Gs 和 Ps 不同，我们确实释放Ms，因此，无论如何都不能持有 muintptr 跨越安全点，这一点很重要。

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//  guintptr 拥有一个 goroutine 指针，但被定义为 uintptr 来绕过 write barriers 。在 Gobuf goroutine 状态和不使用P进行操作的调度列表中使用。
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//  Gobuf.g goroutine 指针几乎总是由汇编代码更新。在少数几个地方，它会通过 Go 代码进行更新-func save-必须将其视为uintptr，以避免在不好的时候
// 发出 write barrier 。无需弄清楚如何发出汇编操作中缺少的 write barrier ，我们将字段的类型更改为uintptr，这样它根本不需要 write barrier 。
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
//  Goroutine 结构在 allg 列表中呈现，并且从未释放。这样可以避免回收 goroutine 结构。永远不会仅有 Gobuf.g 包含对 goroutine 的引用：首先在 allg 中
// 拥有 goroutine。 Goroutine 指针也保存在 TLS 等非 GC 可见的位置，因此我看不到它们移动。如果确实要在 GC 中开始移动数据，则需要从备用的 arena 分配
//  goroutine 结构。使用 guintptr 不会使这个问题更严重。
type guintptr uintptr

// guintptr转化为*g
//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

// 设置*g，*g转化为guintptr
//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

// cas原子操作，Compare And Swap
//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//  setGNoWB 当使用 guintptr 不可行时，在没有 write barrier 下执行 *gp = new。setGNoWB: set G no write barrier。
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
// 1. Never hold an muintptr locally across a safe point.
//
// 2. Any muintptr in the heap must be owned by the M itself so it can
//    ensure it is not in use when the last true *m is released.
//  muintptr 是一个 *m 指针，不受 GC 的追踪，因为我们要释放 M，所以有一些在 muintptr 上的额外限制：
// 1. 永不在 safe point 之外局部持有一个 muintptr 。
// 2. 任何堆上的 muintptr 必须被 M 自身持有，进而保证它不会在最后一个 *m 指针被释放时使用。
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//  setMNoWB 当使用 muintptr 不可行时，在没有 write barrier 下执行 *mp = new。setGNoWB: set G no write barrier.
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

// gobuf记录与协程切换相关信息
type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	// sp, pc 和 g 偏移量均在 libmach 中写死。
	//
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	//  ctxt 对于 GC 非常特殊，它可能是一个在堆上分配的 funcval，因此 GC 需要追踪它，但是它需要从汇编中设置和清除，因此很难使用写屏障。
	// 然而 ctxt 是一个实时保存的、存活的寄存器，且我们只在真实的寄存器和 gobuf 之间进行交换。因此我们将其视为栈扫描时的一个 root，从而
	// 汇编中保存或恢复它不需要写屏障。它仍然作为指针键入，以便来自Go的任何其他写入获得写入障碍。
	sp   uintptr        // sp 寄存器
	pc   uintptr        // pc 寄存器
	g    guintptr       // g 指针
	ctxt unsafe.Pointer // 这个似乎是用来辅助 gc 的
	ret  sys.Uintreg    // 作用 ？ panic.go 中 recovery 函数有设置为 1
	lr   uintptr        // 这是在 arm 上用的寄存器，不用关心
	bp   uintptr        // for GOEXPERIMENT=framepointer // 开启 GOEXPERIMENT=framepointer ，才会有这个
}

// sudog represents a g in a wait list, such as for sending/receiving
// on a channel.
//  sudog 表示了一个等待队列中的 g，例如在一个 channel 中进行发送和接受。
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
// sudog 是必要的，因为 g <-> 同步对象之间的关系是多对多。一个 g 可以在多个等待列表上，因此可以有很多的 sudog 为一个 g 服务；
// 并且很多 g 可能在等待同一个同步对象，因此也会有很多 sudog 为一个同步对象服务。
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
// 所有的 sudog 分配在一个特殊的池中。使用 acquireSudog 和 releaseSudog 来分配并释放它们。
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.
	// 下面的字段由这个 sudog 阻塞的通道的 hchan.lock 进行保护。shrinkstack (收缩堆) 依赖于它服务于 sudog 相关的 channel 操作。

	g *g

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	//  isSelect 表示 g 正在参与一个 select，因此 g.selectDone 必须以 CAS 的方式来避免唤醒时候的 data race。
	isSelect bool
	next     *sudog
	prev     *sudog
	elem     unsafe.Pointer // data element (may point to stack) // 数据元素（可能指向栈）

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.
	// 下面的字段永远不会并发的被访问。对于 channel waitlink 只会被 g 访问，对于 semaphores，所有的字段（包括上面的）只会在持有 semaRoot 锁时被访问。

	acquiretime int64
	releasetime int64
	ticket      uint32
	parent      *sudog // semaRoot binary tree // semaRoot 二叉树，参见sema.go
	waitlink    *sudog // g.waiting list or semaRoot // g.waiting 列表或 semaRoot
	waittail    *sudog // semaRoot
	c           *hchan // channel
}

// libcall
type libcall struct {
	fn   uintptr // 函数
	n    uintptr // number of parameters // 参数个数
	args uintptr // parameters // 参数
	r1   uintptr // return values // 返回值
	r2   uintptr
	err  uintptr // error number // 错误码
}

// describes how to handle callback
// 描述了如何处理回调
type wincallbackcontext struct {
	gobody       unsafe.Pointer // go function to call // 要调度的 go 函数
	argsize      uintptr        // callback arguments size (in bytes) // 回调参数大小（字节）
	restorestack uintptr        // adjust stack on return by (in bytes) (386 only) // 调整返回时的堆栈
	cleanstack   bool
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit data structures on either side.
//  Stack 描述了 Go 的执行栈，栈的区间为 [lo, hi)，在栈两边没有任何隐式数据结构。因此 Go 的执行栈由运行时管理，本质上分配在堆中，比 ulimit -s 大
type stack struct {
	lo uintptr
	hi uintptr
}

// G
type g struct {
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the C stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	// Stack 参数
	// stack 描述了实际的栈内存：[stack.lo, stack.hi)
	// stackguard0 是对比 Go 栈增长的 prologue 的栈指针，如果 sp 寄存器比 stackguard0 小（由于栈往低地址方向增长），会触发栈拷贝和调度
	// 通常情况下：stackguard0 = stack.lo + StackGuard，但被抢占时会变为 StackPreempt。
	// stackguard1 是对比 C 栈增长的 prologue 的栈指针，当位于 g0 和 gsignal 栈上时，值为 stack.lo + StackGuard
	// 在其他栈上值为 ~0 用于触发 morestackc (并 crash) 调用
	//
	//  prologue (序言) 函数是函数开头的几行代码，它们准备了堆栈和寄存器以供在函数内使用。 epilogue (尾声) 函数出现在函数的末尾，并将堆栈和寄存器恢复到调用函数之前的状态。
	//  prologue/epilogue 参见：https://en.wikipedia.org/wiki/Function_prologue
	//
	// 编译器会在有栈溢出风险的函数开头加如 一些代码（也就是prologue），会比较 SP (栈寄存器,指向栈顶) 和 stackguard0，如果 SP 的值更小，说明当前 g 的栈要用完了，
	// 有溢出风险，需要调用 morestack_noctxt 函数来扩栈，morestack_noctxt()->morestack()->newstack() ，newstack中会处理抢占(preempt)。参见 asm_amd64.s,stack.go
	stack       stack   // offset known to runtime/cgo // 当前g使用的栈空间, 有lo和hi两个成员
	stackguard0 uintptr // offset known to liblink //  stackguard0 = stack.lo + StackGuard ，检查栈空间是否足够的值, 低于这个值会扩张栈, 用于 GO 的 stack overlow的检测
	stackguard1 uintptr // offset known to liblink //  stackguard1 = stack.lo + StackGuard ，检查栈空间是否足够的值, 低于这个值会扩张栈, 用于 C 的 stack overlow的检测

	_panic         *_panic         // innermost panic - offset known to liblink // 内部 panic ，偏移量用于 liblink
	_defer         *_defer         // innermost defer // 内部 defer
	m              *m              // current m; offset known to arm liblink // 当前 g 对应的 m ; 偏移量对 arm liblink 透明
	sched          gobuf           // goroutine 的现场，g 的调度数据, 当 g 中断时会保存当前的 pc 和 sp 等值到这里, 恢复运行时会使用这里的值
	syscallsp      uintptr         // if status==Gsyscall, syscallsp = sched.sp to use during gc // 如果 status==Gsyscall, 则 syscallsp = sched.sp 并在 GC 期间使用
	syscallpc      uintptr         // if status==Gsyscall, syscallpc = sched.pc to use during gc // 如果 status==Gsyscall, 则 syscallpc = sched.pc 并在 GC 期间使用
	stktopsp       uintptr         // expected sp at top of stack, to check in traceback // 期望 sp 位于栈顶，用于回溯检查
	param          unsafe.Pointer  // passed parameter on wakeup // wakeup 唤醒时候传递的参数
	atomicstatus   uint32          //  g 的当前状态，原子性
	stackLock      uint32          // sigprof/scang lock; TODO: fold in to atomicstatus // sigprof/scang锁，将会归入到atomicstatus
	goid           int64           // goroutine ID
	schedlink      guintptr        // 下一个 g , 当 g 在链表结构中会使用
	waitsince      int64           // approx time when the g become blocked // g 阻塞的时间
	waitreason     waitReason      // if status==Gwaiting // 如果 status==Gwaiting，则记录等待的原因
	preempt        bool            // preemption signal, duplicates stackguard0 = stackpreempt // 抢占信号， g 是否被抢占中， stackguard0 = stackPreempt 的副本
	paniconfault   bool            // panic (instead of crash) on unexpected fault address // 发生 fault panic （不崩溃）的地址
	preemptscan    bool            // preempted g does scan for gc // 抢占式 g 会执行 GC scan
	gcscandone     bool            // g has scanned stack; protected by _Gscan bit in status // g 执行栈已经 scan 了；此此段受 _Gscan 位保护
	gcscanvalid    bool            // false at start of gc cycle, true if G has not run since last scan; TODO: remove? // 在 gc 周期开始时为 false，如果自上次 scan 以来G没有运行，则为 true
	throwsplit     bool            // must not split stack // 必须不能进行栈分段
	raceignore     int8            // ignore race detection events // 忽略 race 检查事件
	sysblocktraced bool            // StartTrace has emitted EvGoInSyscall about this goroutine //  StartTrace 已经出发了此 goroutine 的 EvGoInSyscall
	sysexitticks   int64           // cputicks when syscall has returned (for tracing) // 当 syscall 返回时的 cputicks（用于跟踪）
	traceseq       uint64          // trace event sequencer // 跟踪事件排序器
	tracelastp     puintptr        // last P emitted an event for this goroutine // 最后一个为此 goroutine 触发事件的 P
	lockedm        muintptr        //  g 是否要求要回到这个 M 执行, 有的时候 g 中断了恢复会要求使用原来的 M 执行
	sig            uint32          // 信号，参见 defs_linux_arm64.go : siginfo
	writebuf       []byte          // 写缓存
	sigcode0       uintptr         // 参见 siginfo
	sigcode1       uintptr         // 参见 siginfo
	sigpc          uintptr         // 产生信号时的PC
	gopc           uintptr         // pc of go statement that created this goroutine // 当前创建 goroutine go 语句的 pc 寄存器
	ancestors      *[]ancestorInfo // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors) // 创建此 goroutine 的 ancestor (祖先) goroutine 的信息(debug.tracebackancestors 调试用)
	startpc        uintptr         // pc of goroutine function // goroutine 函数的 pc 寄存器
	racectx        uintptr         // 竟态上下文
	waiting        *sudog          // sudog structures this g is waiting on (that have a valid elem ptr); in lock order // 如果 g 发生阻塞（且有有效的元素指针）sudog 会将当前 g 按锁住的顺序组织起来
	cgoCtxt        []uintptr       // cgo traceback context // cgo 回溯上下文
	labels         unsafe.Pointer  // profiler labels // 分析器标签
	timer          *timer          // cached timer for time.Sleep // 为 time.Sleep 缓存的计时器
	selectDone     uint32          // are we participating in a select and did someone win the race? // 我们是否正在参与 select 且某个 goroutine 胜出

	// Per-G GC state

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	//  gcAssistBytes 是该 G 在分配的字节数这一方面的的 GC 辅助 credit (信誉)
	// 如果该值为正，则 G 已经存入了在没有 assisting 的情况下分配了 gcAssistBytes 字节，如果该值为负，则 G 必须在 scan work 中修正这个值
	// 我们以字节为单位进行追踪，一遍快速更新并检查 malloc 热路径中分配的债务（分配的字节）。assist ratio 决定了它与 scan work 债务的对应关系
	gcAssistBytes int64
}

// M
type m struct {
	g0      *g     // goroutine with scheduling stack // 用于执行调度指令的 goroutine， 用于调度的特殊 g , 调度和执行系统调用时会切换到这个 g
	morebuf gobuf  // gobuf arg to morestack //  morestack 的 gobuf 参数
	divmod  uint32 // div/mod denominator for arm - known to liblink

	// Fields not known to debuggers.
	// debugger 不知道的字段
	procid        uint64         // for debuggers, but offset not hard-coded // 用于 debugger，偏移量不是写死的
	gsignal       *g             // signal-handling g // 用于 debugger，偏移量不是写死的
	goSigStack    gsignalStack   // Go-allocated signal handling stack // Go 分配的 signal handling 栈
	sigmask       sigset         // storage for saved signal mask // 用于保存 saved signal mask
	tls           [6]uintptr     // thread-local storage (for x86 extern register) // thread-local storage (对 x86 而言为额外的寄存器)
	mstartfn      func()         // M启动函数
	curg          *g             // current running goroutine // 当前运行的用户 g
	caughtsig     guintptr       // goroutine running during fatal signal // goroutine 在 fatal signal 中运行
	p             puintptr       // attached p for executing go code (nil if not executing go code) // 执行 go 代码时持有的 p (如果没有执行则为 nil)
	nextp         puintptr       // 下一个p， 唤醒 M 时,  M 会拥有这个 P
	oldp          puintptr       // the p that was attached before executing a syscall // 执行系统调用之前绑定的 p
	id            int64          // ID
	mallocing     int32          // 是否正在分配内存
	throwing      int32          // 是否正在抛出异常
	preemptoff    string         // if != "", keep curg running on this m // 如果不为空串 ""，继续让当前 g 运行在该 M 上
	locks         int32          // M的锁
	dying         int32          // 是否正在死亡，参见startpanic_m
	profilehz     int32          // cpu profiling rate
	spinning      bool           // m is out of work and is actively looking for work // m 当前没有运行 work 且正处于寻找 work 的活跃状态
	blocked       bool           // m is blocked on a note // m 阻塞在一个 note 上
	inwb          bool           // m is executing a write barrier // m 在执行write barrier
	newSigstack   bool           // minit on C thread called sigaltstack // C 线程上的 minit 是否调用了 signalstack
	printlock     int8           // print 锁，参见 print.go printlock/printunlock
	incgo         bool           // m is executing a cgo call // m 正在执行 cgo 调用
	freeWait      uint32         // if == 0, safe to free g0 and delete m (atomic) // 如果为 0，安全的释放 g0 并删除 m (原子操作)
	fastrand      [2]uint32      // 快速随机
	needextram    bool           // 需要额外的 m
	traceback     uint8          // 回溯
	ncgocall      uint64         // number of cgo calls in total // 总共的 cgo 调用数
	ncgo          int32          // number of cgo calls currently in progress // 正在进行的 cgo 调用数
	cgoCallersUse uint32         // if non-zero, cgoCallers in use temporarily // 如果非零，则表示 cgoCaller 正在临时使用
	cgoCallers    *cgoCallers    // cgo traceback if crashing in cgo call // cgo 调用崩溃的 cgo 回溯
	park          note           //  M 休眠时使用的信号量, 唤醒 M 时会通过它唤醒
	alllink       *m             // on allm // 在 allm 上，将所有的 m 链接起来
	schedlink     muintptr       // 下一个 m , 当 m 在链表结构中会使用
	mcache        *mcache        // 分配内存时使用的本地分配器, 和 p.mcache 一样(拥有 P 时会复制过来)
	lockedg       guintptr       // 表示与当前 M 锁定的那个 G 。运行时系统会把 一个 M 和一个 G 锁定，一旦锁定就只能双方相互作用，不接受第三者。g.lockedm 的对应值
	createstack   [32]uintptr    // stack that created this thread.// 当前线程创建的栈
	lockedExt     uint32         // tracking for external LockOSThread // 外部 LockOSThread 追踪，LockOSThread/UnlockOSThread
	lockedInt     uint32         // tracking for internal lockOSThread // 内部 lockOSThread 追踪，lockOSThread/unlockOSThread
	nextwaitm     muintptr       // next m waiting for lock // 内部 lockOSThread 追踪
	waitunlockf   unsafe.Pointer // todo go func(*g, unsafe.pointer) bool // 参见proc.go gopark
	waitlock      unsafe.Pointer // 参见proc.go gopark
	waittraceev   byte           // 参见proc.go gopark
	waittraceskip int            // 参见proc.go gopark
	startingtrace bool           // 开始trace，见trace.go StartTrace
	syscalltick   uint32         // 每次系统调用时增加
	thread        uintptr        // thread handle // 线程句柄
	freelink      *m             // on sched.freem // 在 sched.freem 上

	// these are here because they are too large to be on the stack
	// of low-level NOSPLIT functions.
	// 下面这些字段因为它们太大而不能放在低级的 NOSPLIT 函数的堆栈上。
	libcall   libcall  // 调用信息
	libcallpc uintptr  // for cpu profiler // 用于 cpu profiler
	libcallsp uintptr  // libcall SP
	libcallg  guintptr // libcall G
	syscall   libcall  // stores syscall parameters on windows // 存储 windows 上系统调用的参数

	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	vdsoPC uintptr // PC for traceback while in VDSO call

	mOS
}

// P
type p struct {
	lock mutex // 锁

	id          int32      // ID
	status      uint32     // one of pidle/prunning/... // 状态
	link        puintptr   // p 的链表
	schedtick   uint32     // incremented on every scheduler call // 每次调度程序调用时增加
	syscalltick uint32     // incremented on every system call // 每次系统调用时增加
	sysmontick  sysmontick // last tick observed by sysmon // sysmon观察到的最后一个tick，会记录
	m           muintptr   // back-link to associated m (nil if idle) // 反向链接到相关的M（如果空闲则为nil）
	mcache      *mcache    // mcache: 每个P的内存缓存，不需要加锁
	racectx     uintptr    // 竟态上下文

	deferpool    [5][]*_defer // pool of available defer structs of different sizes (see panic.go) // defer池，拥有不同的尺寸
	deferpoolbuf [5][32]*_defer

	// Cache of goroutine ids, amortizes accesses to runtime·sched.goidgen.
	// goroutine ID的缓存将分摊对runtime.sched.goidgen的访问。
	goidcache    uint64
	goidcacheend uint64

	// Queue of runnable goroutines. Accessed without lock.
	// 可运行goroutine队列。 无锁访问。
	//  runqhead 和 runqtail 都是 uint32 ，不用担心越界问题，超过最大值之后，会变为 0 ，并且 0 - ^uint32(0) = 1 ，也就是说取长度也不会有问题。
	runqhead uint32        // 队头，runqhead 在后头跟，指向第一个可以使用，应该从这里取出
	runqtail uint32        // 队尾，runqtail 在队列前面走，指向第一个空槽，应该插入到这里
	runq     [256]guintptr // 运行队列
	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	// runnext（如果不是nil）是当前G准备好的可运行G，如果正在运行的G的时间片中还有剩余时间，则应在下一个运行，而不是在runq中的G来运行。
	// 它将继承当前时间片中剩余的时间。如果将一组goroutine锁定为“通信等待”模式，则将其设置为一个单元进行调度，并消除了（可能较大的）调度
	// 延迟，而这种延迟可能是由于将就绪的goroutine添加到运行队列的末尾而引起的。
	runnext guintptr

	// Available G's (status == Gdead)
	// 可用的G，状态为Gdead
	gFree struct {
		gList
		n int32
	}

	sudogcache []*sudog // sudog缓存，初始化为： sudogbuf[:0]
	sudogbuf   [128]*sudog

	tracebuf traceBufPtr // trace缓存，64Kb

	// traceSweep indicates the sweep events should be traced.
	// This is used to defer the sweep start event until a span
	// has actually been swept.
	// traceSweep指示清扫事件是否被trace。这用于推迟清扫开始事件，直到实际扫过一个span为止。
	traceSweep bool
	// traceSwept and traceReclaimed track the number of bytes
	// swept and reclaimed by sweeping in the current sweep loop.
	// traceSwept和traceReclaimed跟踪通过在当前清扫循环中进行清扫来清除和回收的字节数。
	traceSwept, traceReclaimed uintptr

	palloc persistentAlloc // per-P to avoid mutex // 分配器，每个P一个，避免加锁

	// Per-P GC state
	// 每个P的GC状态
	gcAssistTime         int64            // Nanoseconds in assistAlloc // gcAassistAlloc中计时
	gcFractionalMarkTime int64            // Nanoseconds in fractional mark worker // GC Mark计时
	gcBgMarkWorker       guintptr         // 标记G
	gcMarkWorkerMode     gcMarkWorkerMode // 标记模式

	// gcMarkWorkerStartTime is the nanotime() at which this mark
	// worker started.
	gcMarkWorkerStartTime int64 // mark worker启动时间

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	// gcw是此P的GC工作缓冲区高速缓存。工作缓冲区由写屏障填充，由辅助mutator(赋值器)耗尽，并放置在某些GC状态转换上。
	gcw gcWork //  GC 的本地工作队列，灰色对象管理

	// wbBuf is this P's GC write barrier buffer.
	//
	// TODO: Consider caching this in the running G.
	// wbBuf 是当前 P 的 GC 的 write barrier 缓存
	wbBuf wbBuf

	runSafePointFn uint32 // if 1, run sched.safePointFn at next safe point // 如果为 1, 则在下一个 safe-point 运行 sched.safePointFn

	pad cpu.CacheLinePad
}

// 调度
type schedt struct {
	// accessed atomically. keep at top to ensure alignment on 32-bit systems.
	// 原子访问，放到顶部，确保在32位系统上对齐（8字节）。
	goidgen  uint64 // go runtime ID生成器，原子自增，在newproc1和oneNewExtraM中使用了
	lastpoll uint64 // 上一次轮询的时间（nanotime）

	lock mutex // 锁

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().
	// 当增加nmidle，nmidlelocked，nmsys或nmfreed时，请确保调用checkdead()。

	midle        muintptr // idle m's waiting for work // 空闲的 M 队列
	nmidle       int32    // number of idle m's waiting for work // 当前等待工作的空闲 M 计数
	nmidlelocked int32    // number of locked m's waiting for work // 当前等待工作的被 lock 的 M 计数
	mnext        int64    // number of m's that have been created and next M ID // 已经被创建的 M 的数量，下一个 M 的 ID
	maxmcount    int32    // maximum number of m's allowed (or die) // 最大M的数量
	nmsys        int32    // number of system m's not counted for deadlock // 系统 M 的数量， 在 deadlock 中不计数
	nmfreed      int64    // cumulative number of freed m's	// 释放的 M 的累计数量

	ngsys uint32 // number of system goroutines; updated atomically // 系统调用 goroutine 的数量，原子更新

	pidle      puintptr // idle p's // 空闲的P
	npidle     uint32   // 空闲的P的数目
	nmspinning uint32   // See "Worker thread parking/unparking" comment in proc.go. // 处于spinning的M的数目

	// Global runnable queue.
	runq     gQueue // 全局的 G 运行队列
	runqsize int32  // 全局的 G 运行队列大小

	// disable controls selective disabling of the scheduler.
	//
	// Use schedEnableUser to control this.
	//
	// disable is protected by sched.lock.
	// disable控制选择性禁用调度程序。使用schedEnableUser进行控制。受sched.lock保护。
	disable struct {
		// user disables scheduling of user goroutines.
		user     bool   // 用户禁用用户的goroutines调度
		runnable gQueue // pending runnable Gs // 等待的可运行的G队列
		n        int32  // length of runnable // runnable的长度
	}

	// Global cache of dead G's.
	// dead G全局缓存，已退出的 goroutine 对象缓存下来，避免每次创建 goroutine 时都重新分配内存，可参考proc.go中的gfput,gfget
	gFree struct {
		lock    mutex // 锁
		stack   gList // Gs with stacks // 有堆栈的G
		noStack gList // Gs without stacks // 没有堆栈的G
		n       int32 // stack和noStack中的总数
	}

	// Central cache of sudog structs.
	sudoglock  mutex  // sudog缓存的锁
	sudogcache *sudog // sudog缓存

	// Central pool of available defer structs of different sizes.
	deferlock mutex      // defer缓存的锁
	deferpool [5]*_defer // defer缓存

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	freem *m // freem是设置 m.exited 时等待释放的 m 列表。通过 m.freelink 链接。

	gcwaiting  uint32 // gc is waiting to run // GC等待运行
	stopwait   int32  // 需要停止P的数目
	stopnote   note   // stopwait睡眠唤醒事件
	sysmonwait uint32 // 等待系统监控
	sysmonnote note   // sysmonwait睡眠唤醒事件

	// safepointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	// 如果设置了p.runSafePointFn，则应在下一个GC安全点的每个P上调用safepointFn。
	safePointFn   func(*p) // 安全点函数
	safePointWait int32    // 等待safePointFn执行
	safePointNote note     // safePointWait睡眠唤醒事件

	profilehz int32 // cpu profiling rate // CPU

	procresizetime int64 // nanotime() of last change to gomaxprocs // 上一次修改gomaxprocs的时间，参见proc.go中的procresize
	totaltime      int64 // ∫gomaxprocs dt up to procresizetime // 总时间
}

// Values for the flags field of a sigTabT.
// sigTabT的标志字段的值。
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // add SA_ONSTACK to libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
// 函数定义
type _func struct {
	entry   uintptr // start pc // 启动PC
	nameoff int32   // function name // 函数名

	args        int32  // in/out args size // 参数大小
	deferreturn uint32 // offset of a deferreturn block from entry, if any. // offset of a deferreturn block from entry, if any.

	pcsp      int32
	pcfile    int32
	pcln      int32
	npcdata   int32
	funcID    funcID  // set for certain special runtime functions // 为某些特殊的运行时函数设置
	_         [2]int8 // unused
	nfuncdata uint8   // must be last // 函数数据，必须在最后面
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
// 内联函数，* Func可以是* _func或* funcinl，它们由第一个uintptr区分。
type funcinl struct {
	zero  uintptr // set to 0 to distinguish from _func // 设置为0，用以区分_func
	entry uintptr // entry of the real (the "outermost") frame.
	name  string
	file  string
	line  int
}

// layout of Itab known to compilers
// allocated in non-garbage-collected memory
// Needs to be in sync with
// ../cmd/compile/internal/gc/reflect.go:/^func.dumptypestructs.
// 接口类型iface的成员itab ， interface table
type itab struct {
	inter *interfacetype // 接口类型
	_type *_type         // _type数据类型
	hash  uint32         // copy of _type.hash. Used for type switches. // hash方法
	_     [4]byte        //
	fun   [1]uintptr     // variable sized. fun[0]==0 means _type does not implement inter. // 函数，当fun[0]==0，_type没有实现该接口，当有实现接口时，fun存放了第一个接口方法的地址，其他方法一次往下存放。
}

// Lock-free stack node.
// // Also known to export_test.go.
// Lock-free堆栈节点
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

//  强制GC状态
type forcegcstate struct {
	lock mutex
	g    *g
	idle uint32 // 是否处于空闲，也就是强制GC没有开始
}

// startup_random_data holds random bytes initialized at startup. These come from
// the ELF AT_RANDOM auxiliary vector (vdso_linux_amd64.go or os_linux_386.go).
// startup_random_data包含启动时初始化的随机字节。 这些来自ELF AT_RANDOM辅助向量（vdso_linux_amd64.go或os_linux_386.go）
var startupRandomData []byte

// extendRandom extends the random numbers in r[:n] to the whole slice r.
// Treats n<0 as n==0.
// extendRandom将r[:n]中的随机数扩展到整个切片r。将n < 0视为n == 0。
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < sys.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in freedefer.
// _defer在延迟调用列表中保留的一个条目。如果在此处添加字段，请添加代码以在freedefer中清除它。
type _defer struct {
	siz     int32    // 参数的大小
	started bool     // 是否执行过了
	sp      uintptr  // SP sp at time of defer
	pc      uintptr  // PC
	fn      *funcval // 函数指针
	_panic  *_panic  // panic that is running defer // 正在运行defer的_panic
	link    *_defer  // 指针
}

// A _panic holds information about an active panic.
// _panic包含有关激活的panic的信息。
//
// This is marked go:notinheap because _panic values must only ever
// live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
// argp和link字段是堆栈指针，但是在堆栈增长过程中不需要特殊处理：因为它们是指针类型的，并且_panic值仅存在于堆栈中，所以定期进行堆栈指针调整即可解决它们。
//
//go:notinheap
type _panic struct {
	argp      unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink // 延迟调用参数的指针
	arg       interface{}    // argument to panic // 参数
	link      *_panic        // link to earlier panic // 链接到更早的_panic，按时间倒序链表，先进后出
	recovered bool           // whether this panic is over // 是否recovered
	aborted   bool           // the panic was aborted // 是否aborted
}

// stack traces
// 堆栈帧
type stkframe struct {
	fn       funcInfo   // function being run // 运行函数
	pc       uintptr    // program counter within fn // fn中的程序计数器
	continpc uintptr    // program counter where execution can continue, or 0 if not // 可以继续执行的程序计数器，否则为0
	lr       uintptr    // program counter at caller aka link register // 调用方又称为链接寄存器处的程序计数器
	sp       uintptr    // stack pointer at pc // PC的堆栈指针
	fp       uintptr    // stack pointer at caller aka frame pointer // 调用者又称帧指针处的堆栈指针
	varp     uintptr    // top of local variables // 局部变量的顶部
	argp     uintptr    // pointer to function arguments // 指向函数参数的指针
	arglen   uintptr    // number of bytes at argp	// argp的字节数
	argmap   *bitvector // force use of this argmap // 强制使用此argmap
}

// ancestorInfo records details of where a goroutine was started.
// ancestorInfo记录goroutine在何处启动的详细信息。祖先的信息，调用者信息。
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid int64     // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

// 追踪日志打印的一些flag
const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at g that called into it
)

// The maximum number of frames we print for a traceback
// 追踪打印日志最大栈帧
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
// waitReason表明goroutine停止原因
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGGIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonTimerGoroutineIdle                      // "timer goroutine (idle)"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGGIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonTimerGoroutineIdle:    "timer goroutine (idle)",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

// 全局的一些对象
var (
	allglen    uintptr      // 所有G的数目
	allm       *m           // 所有的M
	allp       []*p         // len(allp) == gomaxprocs; may change at safe points, otherwise immutable // 可能在安全区更改，否则不变
	allpLock   mutex        // Protects P-less reads of allp and all writes // allp的锁
	gomaxprocs int32        // GOMAXPROCS
	ncpu       int32        // CPU数目
	forcegc    forcegcstate // 强制GC
	sched      schedt       // 调度
	newprocs   int32        // GOMAXPROCS函数设置，startTheWorld处理

	// Information about what cpu features are available.
	// Packages outside the runtime should not use these
	// as they are not an external api.
	// Set on startup in asm_{386,amd64,amd64p32}.s
	// CPU特征信息
	processorVersionInfo uint32 //  cpu flags，启动是在 asm_amd64.s 中的 runtime·rt0_go 中调用
	isIntel              bool   // 是否是 Intel cpu ，启动是在 asm_amd64.s 中的 runtime·rt0_go 中调用
	//  rdtsc 指令是得到CPU自启动以后的运行周期
	//  lfenceBeforeRdtsc 表示是否在 rdtsc 指令之前是否需要 LFENCE 指令。否则是 MFENCE 指令。
	//  SFENCE : 在 SFENCE 指令前的写操作当必须在 SFENCE 指令后的写操作前完成。写串行化。
	//  LFENCE ：在 lfence 指令前的读操作当必须在 LFENCE 指令后的读操作前完成。读串行化。
	//  MFENCE ：在 mfence 指令前的读写操作当必须在 MFENCE 指令后的读写操作前完成。读写都串行化。
	lfenceBeforeRdtsc bool

	goarm                uint8 // set by cmd/link on arm systems
	framepointer_enabled bool  // set by cmd/link
)

// Set by the linker so the runtime can determine the buildmode.
// 由链接器设置，以便运行时可以确定构建模式。
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)
