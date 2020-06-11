// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

var buildVersion = sys.TheVersion

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.\
// 调度程序的工作是在工作线程上分发准备运行的goroutine。
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
//
// 主要的概念是：
// G: Goroutine，即我们在 Go 程序中使用 go 关键字创建的执行体；
// M: Machine，或 worker thread，即传统意义上进程的线程；
// P: processor，一种执行 Go 代码被要求资源。M 必须关联一个 P 才能执行 Go 代码，但它可以被阻塞或在一个系统调用中没有关联的 P。
//
// Design doc at https://golang.org/s/go11sched.

// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
// 工作线程的 park/unpark (暂止/复始)
//
// 我们需要在保持足够的运行 worker thread 来利用有效硬件并发资源，和 park 运行过多的 worker thread 来节约 CPU 能耗之间进行权衡。
// 这个权衡并不简单，有以下两点原因：
// 1. 调度器状态是有意分布的（具体而言，是一个 per-P 的 work 队列），因此在快速路径（fast path）计算出全局谓词 (global predicates) 是不可能的。
// 2. 为了获得最佳的线程管理，我们必须知道未来的情况（当一个新的 goroutine 会在不久的将来 ready，不再 park 一个 worker thread）
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
// 这三种被驳回的方法很糟糕：
// 1.集中式管理所有调度器状态（会将限制可扩展性）。
// 2. 直接切换 goroutine。也就是说，当我们 ready 一个新的 goroutine 时，让出一个 P，unpark 一个线程并切换到这个线程运行 goroutine。因为 ready 的 goroutine 线程可能
// 在下一个瞬间 out of work，从而导致线程 thrashing（当计算机虚拟内存饱和时会发生thrashing，最终导致分页调度状态不再变化。这个状态会一直持续，直到用户关闭一些运行的应用
// 或者活跃进程释放一些虚拟内存资源），因此我们需要 park 这个线程。同样，我们希望在相同的线程内保存维护 goroutine，这种方式还会摧毁计算的局部性原理。 并引入额外的延迟。
// 3.任何时候 ready 一个 goroutine 时也存在一个空闲的 P 时，都 unpark 一个额外的线程，但不进行切换。因为额外线程会在没有检查任何 work 的情况下立即 park ，最终导致大量
// 线程的parking/unparking。
// 3.每当我们准备好goroutine并且有一个空闲的P时，请启动一个附加线程，但是不要进行切换。这将导致过多的线程停止/启动，因为其他线程将立即停止而没有发现任何要做的工作。
//
// The current approach:
// We unpark an additional thread when we ready a goroutine if (1) there is an
// idle P and there are no "spinning" worker threads. A worker thread is considered
// spinning if it is out of local work and did not find work in global run queue/
// netpoller; the spinning state is denoted in m.spinning and in sched.nmspinning.
// Threads unparked this way are also considered spinning; we don't do goroutine
// handoff so such threads are out of work initially. Spinning threads do some
// spinning looking for work in per-P run queues before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning state
// and then parks.
// 目前方法：
// 如果存在一个空闲的 P 并且没有 spinning 状态的工作线程，当 ready 一个 goroutine 时，就 unpark 一个额外的线程。如果一个工作线程的本地队列里没有 work ，且在全局运行队列
// 或 netpoller 中也没有 work，则称这个工作线程被称之为 spinning；spinning 状态由 sched.nmspinning 和 m.spinning 表示。这种方式下被 unpark 的线程同样也成为 spinning，
// 我们也不对这种线程进行 goroutine 切换，因此这类线程最初就是 out of work。spinning 线程会在 park 前，从 per-P 中运行队列中寻找 work。如果一个 spinning 进程发现 work，
// 就会将自身切换出 spinning 状态，并且开始执行。如果它没有发现 work 则会将自己带 spinning 转状态然后进行 park。
//
// If there is at least one spinning thread (sched.nmspinning>1), we don't unpark
// new threads when readying goroutines. To compensate for that, if the last spinning
// thread finds work and stops spinning, it must unpark a new spinning thread.
// This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism utilization.
// 如果至少有一个 spinning 进程（sched.nmspinning>1），则 ready 一个 goroutine 时，不会去 unpark 一个新的线程。作为补偿，如果最后一个 spinning 线程发现 work
// 并且停止 spinning，则必须 unpark 一个新的 spinning 线程。这个方法消除了不合理的线程 unpark 峰值，且同时保证最终的最大 CPU 并行度利用率。
//
// The main implementation complication is that we need to be very careful during
// spinning->non-spinning thread transition. This transition can race with submission
// of a new goroutine, and either one part or another needs to unpark another worker
// thread. If they both fail to do that, we can end up with semi-persistent CPU
// underutilization. The general pattern for goroutine readying is: submit a goroutine
// to local work queue, #StoreLoad-style memory barrier, check sched.nmspinning.
// The general pattern for spinning->non-spinning transition is: decrement nmspinning,
// #StoreLoad-style memory barrier, check all per-P work queues for new work.
// Note that all this complexity does not apply to global run queue as we are not
// sloppy about thread unparking when submitting to global queue. Also see comments
// for nmspinning manipulation.
// 主要的实现复杂性为当进行 spinning->non-spinning 线程转换时必须非常小心。这种转换在提交一个新的 goroutine ，并且任何一个部分都需要 unpark 另一个工作线程会发生竞争。
// 如果双方均失败，则会以半静态 CPU 利用不足而结束。ready 一个 goroutine 的通用范式为：提交一个 goroutine 到 per-P 的局部 work 队列，#StoreLoad-style 内存屏障，检查
//  sched.nmspinning。从 spinning->non-spinning 转换的一般模式为：减少 nmspinning, #StoreLoad-style 内存屏障，在所有 per-P 工作队列检查新的 work。注意，此种复杂性
// 并不适用于全局工作队列，因为我们不会蠢到当给一个全局队列提交 work 时进行线程 unpark。更多细节参见 nmspinning 操作注释。

var (
	m0           m // 主M，asm_amd64.s中runtime·rt0_go初始化
	g0           g // 主G，asm_amd64.s中runtime·rt0_go初始化
	raceprocctx0 uintptr
)

// 链接函数runtime_init为runtime.init
//go:linkname runtime_init runtime.init
func runtime_init()

// 链接函数main_init为main.init
//go:linkname main_init main.init
func main_init()

// main_init_done is a signal used by cgocallbackg that initialization
// has been completed. It is made before _cgo_notify_runtime_init_done,
// so all cgo calls can rely on it existing. When main_init is complete,
// it is closed, meaning cgocallbackg can reliably receive from it.
//  main_init_done 是 cgocallbackg 使用的信号，表明初始化已完成。它在 _cgo_notify_runtime_init_done 之前进行，因此所有 cgo 调用都可以依赖它的存在。
//  main_init 完成后，它将关闭，这意味着 cgocallbackg 可以可靠地从中接收。
var main_init_done chan bool

// 链接函数main_main为main.main
//go:linkname main_main main.main
func main_main()

// mainStarted indicates that the main M has started.
// mainStarted 表示主 M 是否已经开始运行。
var mainStarted bool

// runtimeInitTime is the nanotime() at which the runtime started.
// runtimeInitTime 是运行时启动的 nanotime()
var runtimeInitTime int64

// Value to use for signal mask for newly created M's.
// 用于新创建的 M 的信号掩码 signal mask 的值。
var initSigmask sigset

// The main goroutine.
//  主 goroutine，也就是runtime·mainPC
func main() {
	// 获取当前的G, G为TLS(Thread Local Storage)
	g := getg()

	// Racectx of m0->g0 is used only as the parent of the main goroutine.
	// It must not be used for anything else.
	//  m0->g0  的racectx仅用作主 goroutine的父代。不得将其用于其他任何用途。
	g.m.g0.racectx = 0

	// Max stack size is 1 GB on 64-bit, 250 MB on 32-bit.
	// Using decimal instead of binary GB and MB because
	// they look nicer in the stack overflow failure message.
	// 执行栈的最大限制： 1GB on 64-bit， 250 MB on 32-bit。使用十进制而不是二进制GB和MB，因为它们在堆栈溢出失败消息中好看些。
	if sys.PtrSize == 8 {
		maxstacksize = 1000000000
	} else {
		maxstacksize = 250000000
	}

	// Allow newproc to start new Ms.
	// 表示main goroutine启动了，接下来允许 newproc 启动新的 m
	mainStarted = true

	if GOARCH != "wasm" { // no threads on wasm yet, so no sysmon // 1.11 新引入的 web assembly, 目前 wasm 不支持线程，无系统监控
		// 启动系统后台监控 (定期 GC，并发任务调度)
		systemstack(func() {
			newm(sysmon, nil)
		})
	}

	// Lock the main goroutine onto this, the main OS thread,
	// during initialization. Most programs won't care, but a few
	// do require certain calls to be made by the main thread.
	// Those can arrange for main.main to run in the main thread
	// by calling runtime.LockOSThread during initialization
	// to preserve the lock.
	// 将主 goroutine 锁在主 OS 线程下进行初始化工作。大部分程序并不关心这一点，但是有一些图形库（基本上属于 cgo 调用）会要求在主线程下进行初始化工作。
	// 即便是在 main.main 下仍然可以通过公共方法 runtime.LockOSThread 来强制将一些特殊的需要主 OS 线程的调用锁在主 OS 线程下执行初始化
	lockOSThread()

	// 执行 runtime.main 函数的 G 必须是绑定在 m0 上的
	if g.m != &m0 {
		throw("runtime.main not on m0")
	}

	//  执行初始化运行时
	runtime_init() // must be before defer //  defer 必须在此调用结束后才能使用
	if nanotime() == 0 {
		throw("nanotime returning zero")
	}

	// Defer unlock so that runtime.Goexit during init does the unlock too.
	// 延迟解锁，以便init期间的runtime.Goexit也会执行解锁。
	needUnlock := true
	defer func() {
		if needUnlock {
			unlockOSThread()
		}
	}()

	// Record when the world started.
	// 记录程序的启动时间
	runtimeInitTime = nanotime()

	// 启动垃圾回收器后台操作
	gcenable()

	main_init_done = make(chan bool)
	if iscgo {
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		if GOOS != "windows" {
			if _cgo_setenv == nil {
				throw("_cgo_setenv missing")
			}
			if _cgo_unsetenv == nil {
				throw("_cgo_unsetenv missing")
			}
		}
		if _cgo_notify_runtime_init_done == nil {
			throw("_cgo_notify_runtime_init_done missing")
		}
		// Start the template thread in case we enter Go from
		// a C-created thread and need to create a new thread.
		// 启动模板线程来处理从 C 创建的线程进入 Go 时需要创建一个新的线程的情况。
		startTemplateThread()
		cgocall(_cgo_notify_runtime_init_done, nil)
	}

	// 执行 main_init，进行间接调用，因为链接器在设定运行时的时候不知道 main 包的地址
	fn := main_init // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	fn()
	close(main_init_done)

	needUnlock = false
	unlockOSThread()

	// 如果是基础库则不需要执行 main 函数了
	if isarchive || islibrary {
		// A program compiled with -buildmode=c-archive or c-shared
		// has a main, but it is not executed.
		// 使用-buildmode=c-archive或c-shared编译的程序具有main函数，但不会执行。
		return
	}
	// 执行用户 main 包中的 main 函数，处理为非间接调用，因为链接器在设定运行时不知道 main 包的地址
	fn = main_main // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	fn()

	// race 相关
	if raceenabled {
		racefini()
	}

	// Make racy client program work: if panicking on
	// another goroutine at the same time as main returns,
	// let the other goroutine finish printing the panic trace.
	// Once it does, it will exit. See issues 3934 and 20018.\
	// 使客户端程序正常工作：如果在其他 goroutine 上 panic 、与此同时 main 返回，也让其他 goroutine 能够完成 panic trace 的打印。打印完成后，立即退出。
	// 见 issue 3934 和 20018
	if atomic.Load(&runningPanicDefers) != 0 {
		// Running deferred functions should not take long.
		// 运行 defer 函数应该不会花太长时间。
		for c := 0; c < 1000; c++ {
			if atomic.Load(&runningPanicDefers) == 0 {
				break
			}
			Gosched()
		}
	}
	if atomic.Load(&panicking) != 0 {
		gopark(nil, nil, waitReasonPanicWait, traceEvGoStop, 1)
	}

	// 退出执行，返回退出状态码
	exit(0)

	// 如果 exit 没有被正确实现，则下面的代码能够强制退出程序，因为 *nil (nil deref) 会崩溃。
	for {
		var x *int32
		*x = 0
	}
}

// os_beforeExit is called from os.Exit(0).
// 在 os.Exit(0) 调用os_beforeExit。 src/os/proc.go:Exit
//go:linkname os_beforeExit os.runtime_beforeExit
func os_beforeExit() {
	if raceenabled {
		racefini()
	}
}

// start forcegc helper goroutine
// 启动 forcegc helper goroutine
func init() {
	// 启动 goroutine 执行强制GC
	go forcegchelper()
}

//
func forcegchelper() {
	forcegc.g = getg()
	for {
		lock(&forcegc.lock)
		if forcegc.idle != 0 {
			throw("forcegc: phase error")
		}
		atomic.Store(&forcegc.idle, 1)
		//  park 当前 G
		goparkunlock(&forcegc.lock, waitReasonForceGGIdle, traceEvGoBlock, 1)
		// this goroutine is explicitly resumed by sysmon
		// 该 goroutine 由 sysmon 显式恢复
		if debug.gctrace > 0 {
			println("GC forced")
		}
		// Time-triggered, fully concurrent.
		//  GC 触发类型为：gcTriggerTime，完全并发。
		gcStart(gcTrigger{kind: gcTriggerTime, now: nanotime()})
	}
}

//go:nosplit

// Gosched yields the processor, allowing other goroutines to run. It does not
// suspend the current goroutine, so execution resumes automatically.
//  Gosched 会让出当前的 P，并允许其他 goroutine 运行。它不会挂起当前的 goroutine，因此执行会被自动恢复。
func Gosched() {
	checkTimeouts()
	mcall(gosched_m)
}

// goschedguarded yields the processor like gosched, but also checks
// for forbidden states and opts out of the yield in those cases.
//  goschedguarded 像 Gosched 一样会让出当前的 P，但是也会检测禁止状态，在这些禁止状态下，让出当前的 P。哪些禁止的状态，可以参见: goschedguarded_m
//go:nosplit
func goschedguarded() {
	mcall(goschedguarded_m)
}

// Puts the current goroutine into a waiting state and calls unlockf.
// If unlockf returns false, the goroutine is resumed.
// unlockf must not access this G's stack, as it may be moved between
// the call to gopark and the call to unlockf.
// Reason explains why the goroutine has been parked.
// It is displayed in stack traces and heap dumps.
// Reasons should be unique and descriptive.
// Do not re-use reasons, add new ones.
// 将当前 goroutine 置于等待状态并调用 unlockf 。如果 unlockf 返回 false ，则恢复 goroutine 。 unlockf 一定不能访问此 G 的栈，因为它可能在调用 gopark 和
// 调用 unlockf 之间移动。Reason 说明了 goroutine 停止的原因。它在 stack traces 和 heap dumps 中显示。Reasons 应具是唯一的切可描述性的。不要重复使用 Reasons，
// 请添加新的 Reasons 。
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer, reason waitReason, traceEv byte, traceskip int) {
	if reason != waitReasonSleep {
		checkTimeouts() // timeouts may expire while two goroutines keep the scheduler busy // 2个 goroutines 保持频繁调度，可能超时
	}
	mp := acquirem()
	gp := mp.curg
	status := readgstatus(gp)
	// 只有 _Grunning 和 _Gscanrunning 状态的 G ， 才能 park
	if status != _Grunning && status != _Gscanrunning {
		throw("gopark: bad g status")
	}
	mp.waitlock = lock
	mp.waitunlockf = *(*unsafe.Pointer)(unsafe.Pointer(&unlockf))
	gp.waitreason = reason
	mp.waittraceev = traceEv
	mp.waittraceskip = traceskip
	releasem(mp)
	// can't do anything that might move the G between Ms here.
	// 不能做任何可能在 M 之间移动 G 的操作。
	mcall(park_m) // 切换到 waiting 状态并重新进入调度循环
}

// Puts the current goroutine into a waiting state and unlocks the lock.
// The goroutine can be made runnable again by calling goready(gp).
// 将当前 goroutine 置于等待状态并解锁 lock。通过调用 goready(gp) 可让 goroutine 再次 runnable。
func goparkunlock(lock *mutex, reason waitReason, traceEv byte, traceskip int) {
	gopark(parkunlock_c, unsafe.Pointer(lock), reason, traceEv, traceskip)
}

// 将 gp 标记为 ready 来运行
func goready(gp *g, traceskip int) {
	systemstack(func() {
		ready(gp, traceskip, true)
	})
}

// 获取 sudog ， go:nosplit 跳过栈溢出检测
//go:nosplit
func acquireSudog() *sudog {
	// Delicate dance: the semaphore implementation calls
	// acquireSudog, acquireSudog calls new(sudog),
	// new calls malloc, malloc can call the garbage collector,
	// and the garbage collector calls the semaphore implementation
	// in stopTheWorld.
	// Break the cycle by doing acquirem/releasem around new(sudog).
	// The acquirem/releasem increments m.locks during new(sudog),
	// which keeps the garbage collector from being invoked.
	// 微妙的舞蹈： semaphore 实现调用 acquireSudog， acquireSudog 调用 new(sudog), new(sudog) 调用 malloc， malloc 调用 GC，GC 在 STW 中调用 semaphore 实现。
	// 通过围绕 new(sudog) 进行 acquirem/releasem 来打破循环。 在 new(sudog) 期间，acquirem/releasem 会递增 m.locks，这可以防止 GC 被调用。
	mp := acquirem()
	pp := mp.p.ptr()
	// 如果没有 sudogcache
	if len(pp.sudogcache) == 0 {
		lock(&sched.sudoglock)
		// First, try to grab a batch from central cache.
		// 首先从 central cache 获取一批，容量到一半，也就是 128/2
		for len(pp.sudogcache) < cap(pp.sudogcache)/2 && sched.sudogcache != nil {
			s := sched.sudogcache
			sched.sudogcache = s.next
			s.next = nil
			pp.sudogcache = append(pp.sudogcache, s)
		}
		unlock(&sched.sudoglock)
		// If the central cache is empty, allocate a new one.
		// 如果 central cache 也没有，分配一个
		if len(pp.sudogcache) == 0 {
			pp.sudogcache = append(pp.sudogcache, new(sudog))
		}
	}
	// 取出 sudogcache 最后一个并返回
	n := len(pp.sudogcache)
	s := pp.sudogcache[n-1]
	pp.sudogcache[n-1] = nil
	pp.sudogcache = pp.sudogcache[:n-1]
	if s.elem != nil {
		throw("acquireSudog: found s.elem != nil in cache")
	}
	releasem(mp)
	return s
}

// 释放 sudog ， go:nosplit 跳过栈溢出检测
//go:nosplit
func releaseSudog(s *sudog) {
	// 一些 assert 检测
	if s.elem != nil {
		throw("runtime: sudog with non-nil elem")
	}
	if s.isSelect {
		throw("runtime: sudog with non-false isSelect")
	}
	if s.next != nil {
		throw("runtime: sudog with non-nil next")
	}
	if s.prev != nil {
		throw("runtime: sudog with non-nil prev")
	}
	if s.waitlink != nil {
		throw("runtime: sudog with non-nil waitlink")
	}
	if s.c != nil {
		throw("runtime: sudog with non-nil c")
	}
	gp := getg()
	if gp.param != nil {
		throw("runtime: releaseSudog with non-nil gp.param")
	}
	mp := acquirem() // avoid rescheduling to another P // 避免重新调度到其他 P 上
	pp := mp.p.ptr()
	// 如果 sudogcache 满了（128）
	if len(pp.sudogcache) == cap(pp.sudogcache) {
		// Transfer half of local cache to the central cache.
		// 将一半 local cache 传输到 central cache 。也就是 P 的 sudogcache 到 sched.sudogcache
		var first, last *sudog
		for len(pp.sudogcache) > cap(pp.sudogcache)/2 {
			n := len(pp.sudogcache)
			p := pp.sudogcache[n-1]
			pp.sudogcache[n-1] = nil
			pp.sudogcache = pp.sudogcache[:n-1]
			if first == nil {
				first = p
			} else {
				last.next = p
			}
			last = p
		}
		// 先取出一半，再加入到 sched.sudogcache ，而不是一个一个的加，减少锁粒度
		lock(&sched.sudoglock)
		last.next = sched.sudogcache
		sched.sudogcache = first
		unlock(&sched.sudoglock)
	}
	pp.sudogcache = append(pp.sudogcache, s)
	releasem(mp)
}

// funcPC returns the entry PC of the function f.
// It assumes that f is a func value. Otherwise the behavior is undefined.
// CAREFUL: In programs with plugins, funcPC can return different values
// for the same function (because there are actually multiple copies of
// the same function in the address space). To be safe, don't use the
// results of this function in any == expression. It is only safe to
// use the result as an address at which to start executing code.
//  funcPC 返回函数 f 的入口 PC。它假设 f 是一个 func 值。否则行为是未定义的。小心：在包含插件的程序中，funcPC 可以对相同的函数返回不同的值
// （因为在地址空间中相同的函数可能有多个副本）。为安全起见，不要在任何 == 表达式中使用此函数。它只在作为地址用于执行代码时是安全的。
//go:nosplit
func funcPC(f interface{}) uintptr {
	//  add(unsafe.Pointer(&f), sys.PtrSize) 就是获取 eface.data 的数据，也就是转为 interface{} 之前真实的数据
	//  **(**uintptr) 两级解指针，也就是获取原对象结构中第一个 Field 指向的值
	//  funcval ?? return *funcval.fn
	return **(**uintptr)(add(unsafe.Pointer(&f), sys.PtrSize))
}

// called from assembly
// 从汇编调用
func badmcall(fn func(*g)) {
	throw("runtime: mcall called on m->g0 stack")
}

func badmcall2(fn func(*g)) {
	throw("runtime: mcall function returned")
}

func badreflectcall() {
	panic(plainError("arg size to reflect.call more than 1GB"))
}

var badmorestackg0Msg = "fatal: morestack on g0\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackg0() {
	sp := stringStructOf(&badmorestackg0Msg)
	write(2, sp.str, int32(sp.len))
}

var badmorestackgsignalMsg = "fatal: morestack on gsignal\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackgsignal() {
	sp := stringStructOf(&badmorestackgsignalMsg)
	write(2, sp.str, int32(sp.len))
}

//go:nosplit
func badctxt() {
	throw("ctxt != 0")
}

// 判断当前 G 是否锁住 OS thread
func lockedOSThread() bool {
	gp := getg()
	return gp.lockedm != 0 && gp.m.lockedg != 0
}

var (
	allgs    []*g  // 全部的 g
	allglock mutex //  allgs 的锁
)

// 将 G 加入到 allgs ，只有 _Gidle 状态的才能加入
func allgadd(gp *g) {
	if readgstatus(gp) == _Gidle {
		throw("allgadd: bad status Gidle")
	}

	lock(&allglock)
	allgs = append(allgs, gp)
	allglen = uintptr(len(allgs))
	unlock(&allglock)
}

const (
	// Number of goroutine ids to grab from sched.goidgen to local per-P cache at once.
	// 16 seems to provide enough amortization, but other than that it's mostly arbitrary number.
	// 一次从 schedule.go 进入本地 per-P 缓存的 goroutine ID 的数量。 16 似乎可以提供足够的摊销，但除此之外，它几乎是任意数。
	_GoidCacheBatch = 16
)

// cpuinit extracts the environment variable GODEBUG from the environment on
// Unix-like operating systems and calls internal/cpu.Initialize.
// cpuinit 在 Unix 系列的操作系统上提取环境变量 GODEBUGCPU，并调用 internal/cpu.initialize
func cpuinit() {
	const prefix = "GODEBUG="
	var env string

	switch GOOS {
	case "aix", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd", "solaris", "linux":
		cpu.DebugOptions = true

		// Similar to goenv_unix but extracts the environment value for
		// GODEBUG directly.
		// TODO(moehrmann): remove when general goenvs() can be called before cpuinit()
		n := int32(0)
		for argv_index(argv, argc+1+n) != nil {
			n++
		}

		for i := int32(0); i < n; i++ {
			p := argv_index(argv, argc+1+i)
			s := *(*string)(unsafe.Pointer(&stringStruct{unsafe.Pointer(p), findnull(p)}))

			if hasPrefix(s, prefix) {
				env = gostring(p)[len(prefix):]
				break
			}
		}
	}

	cpu.Initialize(env)

	// Support cpu feature variables are used in code generated by the compiler
	// to guard execution of instructions that can not be assumed to be always supported.
	// 支持 CPU 特性的变量由编译器生成的代码来阻止指令的执行，从而不能假设总是支持的
	x86HasPOPCNT = cpu.X86.HasPOPCNT
	x86HasSSE41 = cpu.X86.HasSSE41

	arm64HasATOMICS = cpu.ARM64.HasATOMICS
}

// The bootstrap sequence is:
//
//	call osinit
//	call schedinit
//	make & queue new G
//	call runtime·mstart
//
// The new G calls runtime·main.
// 启动顺序
// 调用 osinit
// 调用 schedinit
// make & queue new G
// 调用 runtime·mstart
// 创建 G 的调用 runtime·main.
//
// 初始化sched, 核心部分
func schedinit() {
	// raceinit must be the first call to race detector.
	// In particular, it must be done before mallocinit below calls racemapshadow.
	// raceinit 是作为 race detector(探测器) ，必须是的首个调用，特别是：必须在 调用 mallocinit 函数之前，在 racemapshadow函数之后调用
	// 获取当前 G
	_g_ := getg() // _g_ = g0
	if raceenabled {
		_g_.racectx, raceprocctx0 = raceinit()
	}

	// 最大系统线程数量（即 M），参考标准库 runtime/debug.SetMaxThreads
	sched.maxmcount = 10000

	tracebackinit()    // 初始化 traceback
	moduledataverify() // 模块数据验证，负责检查链接器符号，以确保所有结构体的正确性
	stackinit()        // 栈初始化，复用管理链表
	mallocinit()       // 内存分配器初始化
	mcommoninit(_g_.m) // 初始化当前 M
	cpuinit()          // must run before alginit // 必须在 alginit 之前运行
	alginit()          // maps must not be used before this call //  maps 不能在此调用之前使用，从 CPU 指令集初始化散列算法
	modulesinit()      // provides activeModules // 模块链接，提供 activeModules
	typelinksinit()    // uses maps, activeModules // 使用 maps, activeModules
	itabsinit()        // uses activeModules // 初始化 interface table，使用 activeModules

	msigsave(_g_.m) // 设置signal mask
	initSigmask = _g_.m.sigmask

	goargs()         // 初始化命令行用户参数
	goenvs()         // 初始化环境变量
	parsedebugvars() // 初始化debug参数，处理 GODEBUG、GOTRACEBACK 调试相关的环境变量设置
	gcinit()         // gc初始化

	// 网络的上次轮询时间
	sched.lastpoll = uint64(nanotime())
	// 设置procs， 根据cpu核数和环境变量GOMAXPROCS， 优先环境变量
	procs := ncpu
	if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
		procs = n
	}
	// 调整 P 的数量，这时所有 P 均为新建的 P，因此不能返回有本地任务的 P
	if procresize(procs) != nil {
		throw("unknown runnable goroutine during bootstrap")
	}

	// For cgocheck > 1, we turn on the write barrier at all times
	// and check all pointer writes. We can't do this until after
	// procresize because the write barrier needs a P.
	// 对于 cgocheck>1 ，我们始终打开 write barrier 并检查所有指针写。 我们要等到 procresize 后才能执行此操作，因为写障碍需要一个P。
	if debug.cgocheck > 1 {
		writeBarrier.cgo = true
		writeBarrier.enabled = true
		for _, p := range allp {
			p.wbBuf.reset()
		}
	}

	if buildVersion == "" {
		// Condition should never trigger. This code just serves
		// to ensure runtime·buildVersion is kept in the resulting binary.
		// 该条件永远不会被触发，此处只是为了防止 buildVersion 被编译器优化移除掉。
		buildVersion = "unknown"
	}
}

//  dump(转储) G 的状态，就是打印 G 的状态
func dumpgstatus(gp *g) {
	_g_ := getg()
	print("runtime: gp: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
	print("runtime:  g:  g=", _g_, ", goid=", _g_.goid, ",  g->atomicstatus=", readgstatus(_g_), "\n")
}

// 检查 m 的数量是否太多
func checkmcount() {
	// sched lock is held
	// 此时 sched 是锁住的
	if mcount() > sched.maxmcount {
		print("runtime: program exceeds ", sched.maxmcount, "-thread limit\n")
		throw("thread exhaustion")
	}
}

// 通用初始化 M
func mcommoninit(mp *m) {
	_g_ := getg()

	// g0 stack won't make sense for user (and is not necessary unwindable).
	// 检查当前 g 是否是 g0，g0 栈对用户而言是没有意义的（且不是不可避免的）
	if _g_ != _g_.m.g0 {
		callers(1, mp.createstack[:])
	}

	// 锁住调度器
	lock(&sched.lock)
	// 确保线程数量不会太多而溢出
	if sched.mnext+1 < sched.mnext {
		throw("runtime: thread ID overflow")
	}
	// mnext 表示当前 m 的数量，还表示下一个 m 的 id
	mp.id = sched.mnext
	// 增加 m 的数量
	sched.mnext++
	// 检测 m 的数量
	checkmcount()

	// 用于 fastrand 快速取随机数
	mp.fastrand[0] = 1597334677 * uint32(mp.id)
	mp.fastrand[1] = uint32(cputicks())
	if mp.fastrand[0]|mp.fastrand[1] == 0 {
		mp.fastrand[1] = 1
	}

	// 初始化 gsignal，用于处理 m 上的信号。
	mpreinit(mp)
	// gsignal 的运行栈边界处理
	if mp.gsignal != nil {
		mp.gsignal.stackguard1 = mp.gsignal.stack.lo + _StackGuard
	}

	// Add to allm so garbage collector doesn't free g->m
	// when it is just in a register or thread-local storage.
	// 添加到 allm 中，从而当它刚保存到寄存器或本地线程存储时候 GC 不会释放 g->m
	// 每一次调用都会将 allm 给 alllink，给完之后自身被 mp 替换，在下一次的时候又给 alllink ，从而形成链表
	mp.alllink = allm

	// NumCgoCall() iterates over allm w/o schedlock,
	// so we need to publish it safely.
	// NumCgoCall() 会在没有使用 schedlock 时遍历 allm，因此我们需要安全的修改。
	// 等价于 allm = mp
	atomicstorep(unsafe.Pointer(&allm), unsafe.Pointer(mp))
	//  m 的通用初始化完成，解锁调度器
	unlock(&sched.lock)

	// Allocate memory to hold a cgo traceback if the cgo call crashes.
	// 分配内存来保存当 cgo 调用崩溃时候的回溯
	if iscgo || GOOS == "solaris" || GOOS == "windows" {
		mp.cgoCallers = new(cgoCallers)
	}
}

// Mark gp ready to run.
//  将 gp 标记为 ready 来运行
func ready(gp *g, traceskip int, next bool) {
	//  trace 相关
	if trace.enabled {
		traceGoUnpark(gp, traceskip)
	}

	status := readgstatus(gp)

	// Mark runnable.
	// 标记为 _Grunnable
	_g_ := getg()
	_g_.m.locks++ // disable preemption because it can be holding p in a local var // 禁止抢占，因为它可以在局部变量中保存 p
	// 只有 _Gwaiting 和 _Gscanwaiting 状态才能变为 _Grunnable
	if status&^_Gscan != _Gwaiting {
		dumpgstatus(gp)
		throw("bad g->status in ready")
	}

	// status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
	// 状态为 Gwaiting 或 Gscanwaiting, 标记 Grunnable 并将其放入运行队列 runq
	casgstatus(gp, _Gwaiting, _Grunnable)
	runqput(_g_.m.p.ptr(), gp, next)
	// 如果没有空闲的P 并且 spinning M， 则唤醒一个P， wakep 会启动M
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
		wakep()
	}
	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in Case we've cleared it in newstack // 在 newstack 中清除了抢占请求的情况下恢复抢占请求
		_g_.stackguard0 = stackPreempt
	}
}

// freezeStopWait is a large value that freezetheworld sets
// sched.stopwait to in order to request that all Gs permanently stop.
//  freezeStopWait 是一个很大的值， freezetheworld 将 sched.stopwait 设置为该值，以请求所有G永久停止。
const freezeStopWait = 0x7fffffff

// freezing is set to non-zero if the runtime is trying to freeze the
// world.
// 如果运行时试图冻结世界，则将 freezing 设置为非零。
var freezing uint32

// Similar to stopTheWorld but best-effort and can be called several times.
// There is no reverse operation, used during crashing.
// This function must not lock any mutexes.
// 与 stopTheWorld 相似，但是尽力而为，可以多次调用。 没有在 crash 期间使用的反向操作。此函数不得锁定任何互斥锁。
func freezetheworld() {
	atomic.Store(&freezing, 1)
	// stopwait and preemption requests can be lost
	// due to races with concurrently executing threads,
	// so try several times
	// 由于并发执行线程的 race ，stopwait 和抢占请求可能会丢失，因此请尝试几次
	for i := 0; i < 5; i++ {
		// this should tell the scheduler to not start any new goroutines
		// 告诉调度程序不要启动任何新的 goroutines
		sched.stopwait = freezeStopWait
		atomic.Store(&sched.gcwaiting, 1)
		// this should stop running goroutines
		// 停止运行 goroutines
		if !preemptall() {
			break // no running goroutines // 没有运行的 goroutines
		}
		usleep(1000)
	}
	// to be sure // 确保
	usleep(1000)
	preemptall()
	usleep(1000)
}

// 是否是 _Gscan 状态
func isscanstatus(status uint32) bool {
	if status == _Gscan {
		throw("isscanstatus: Bad status Gscan")
	}
	return status&_Gscan == _Gscan
}

// All reads and writes of g's status go through readgstatus, casgstatus
// castogscanstatus, casfrom_Gscanstatus.
//  g 状态的所有读取和写入都通过 readgstatus ， casgstatus ， castogscanstatus ， casfrom_Gscanstatus 进行。
//  readgstatus 读取 g 的状态
//go:nosplit
func readgstatus(gp *g) uint32 {
	return atomic.Load(&gp.atomicstatus)
}

// Ownership of gcscanvalid:
//
// If gp is running (meaning status == _Grunning or _Grunning|_Gscan),
// then gp owns gp.gcscanvalid, and other goroutines must not modify it.
//
// Otherwise, a second goroutine can lock the scan state by setting _Gscan
// in the status bit and then modify gcscanvalid, and then unlock the scan state.
//
// Note that the first condition implies an exception to the second:
// if a second goroutine changes gp's status to _Grunning|_Gscan,
// that second goroutine still does not have the right to modify gcscanvalid.

// The Gscanstatuses are acting like locks and this releases them.
// If it proves to be a performance hit we should be able to make these
// simple atomic stores but for now we are going to throw if
// we see an inconsistent state.
// gcscanvalid的所有权：
// 如果gp正在运行（ 意味着 status== _Grunning 或 _Grunning | _Gscan），则 gp 拥有 gp.gcscanvalid ，其他 goroutine 不得对其进行修改。
// 否则，第二个 goroutine 可以通过在状态位中设置 _Gscan 锁定扫描状态，然后修改 gcscanvalid ，然后解锁扫描状态。
// 请注意，第一个条件暗示了第二个条件的例外：如果第二个 goroutine 将 gp 的状态更改为_Grunning | _Gscan，则该第二个 goroutine 仍然无权修改 gcscanvalid 。
// Gscanstatuses 的行为就像锁，这会释放它们。 如果证明对性能有影响，我们应该能够制造这些简单的原子存储，但是现在，如果看到不一致的状态，我们将抛出该异常。
// 在 _Gscanrunnable ， _Gscanwaiting ， _Gscanrunning ， _Gscansyscall 状态中去掉的 _Gscan 标志位
func casfrom_Gscanstatus(gp *g, oldval, newval uint32) {
	success := false

	// Check that transition is valid.
	// 检查转换是否有效。
	//
	switch oldval {
	default:
		print("runtime: casfrom_Gscanstatus bad oldval gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus:top gp->status is not in scan state")
	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscanrunning,
		_Gscansyscall:
		if newval == oldval&^_Gscan {
			success = atomic.Cas(&gp.atomicstatus, oldval, newval)
		}
	}
	if !success {
		print("runtime: casfrom_Gscanstatus failed gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus: gp->status is not in scan state")
	}
}

// This will return false if the gp is not in the expected status and the cas fails.
// This acts like a lock acquire while the casfromgstatus acts like a lock release.
// 如果 gp 不在预期状态并且 cas 失败，则返回false。这就像获取锁一样，而 casfromgstatus 就像释放锁一样。
// 在 _Gscanrunnable ， _Gscanwaiting ， _Gscanrunning ， _Gscansyscall 状态中加上的 _Gscan 标志位
func castogscanstatus(gp *g, oldval, newval uint32) bool {
	switch oldval {
	case _Grunnable,
		_Grunning,
		_Gwaiting,
		_Gsyscall:
		if newval == oldval|_Gscan {
			return atomic.Cas(&gp.atomicstatus, oldval, newval)
		}
	}
	print("runtime: castogscanstatus oldval=", hex(oldval), " newval=", hex(newval), "\n")
	throw("castogscanstatus")
	panic("not reached")
}

// If asked to move to or from a Gscanstatus this will throw. Use the castogscanstatus
// and casfrom_Gscanstatus instead.
// casgstatus will loop if the g->atomicstatus is in a Gscan status until the routine that
// put it in the Gscan state is finished.
// 如果要求移动到 Gscanstatus 或从 Gscanstatus 移动，则会抛出该错误。 请改用 castogscanstatus 和 casfrom_Gscanstatus 。
// 如果 g->atomicstatus 处于 Gscan 状态，则 casgstatus 将循环运行，直到 routine 设置 Gscan 状态结束为止。
//go:nosplit
func casgstatus(gp *g, oldval, newval uint32) {
	if (oldval&_Gscan != 0) || (newval&_Gscan != 0) || oldval == newval {
		systemstack(func() {
			print("runtime: casgstatus: oldval=", hex(oldval), " newval=", hex(newval), "\n")
			throw("casgstatus: bad incoming values")
		})
	}

	if oldval == _Grunning && gp.gcscanvalid {
		// If oldvall == _Grunning, then the actual status must be
		// _Grunning or _Grunning|_Gscan; either way,
		// we own gp.gcscanvalid, so it's safe to read.
		// gp.gcscanvalid must not be true when we are running.
		// 如果 oldvall == _Grunning，则实际状态必须为 _Grunning 或 _Grunning | _Gscan ；无论哪种情况，我们都拥有 gp.gcscanvalid ，因此可以安全读取。
		// 当我们运行时，gp.gcscanvalid不能为true。
		systemstack(func() {
			print("runtime: casgstatus ", hex(oldval), "->", hex(newval), " gp.status=", hex(gp.atomicstatus), " gp.gcscanvalid=true\n")
			throw("casgstatus")
		})
	}

	// See https://golang.org/cl/21503 for justification of the yield delay.
	// 查看 https://golang.org/cl/21503 来证明延迟 yield
	const yieldDelay = 5 * 1000
	var nextYield int64

	// loop if gp->atomicstatus is in a scan state giving
	// GC time to finish and change the state to oldval.
	// 如果 gp->atomicstatus 处于扫描状态，则循环，使GC有时间完成并将状态更改为oldval。也就是直到 gp.atomicstatus == oldval
	for i := 0; !atomic.Cas(&gp.atomicstatus, oldval, newval); i++ {
		if oldval == _Gwaiting && gp.atomicstatus == _Grunnable {
			throw("casgstatus: waiting for Gwaiting but is Grunnable")
		}
		// Help GC if needed.
		// if gp.preemptscan && !gp.gcworkdone && (oldval == _Grunning || oldval == _Gsyscall) {
		// 	gp.preemptscan = false
		// 	systemstack(func() {
		// 		gcphasework(gp)
		// 	})
		// }
		// But meanwhile just yield.
		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			for x := 0; x < 10 && gp.atomicstatus != oldval; x++ {
				procyield(1)
			}
		} else {
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}
	// CAS 成功了
	if newval == _Grunning {
		gp.gcscanvalid = false
	}
}

// casgstatus(gp, oldstatus, Gcopystack), assuming oldstatus is Gwaiting or Grunnable.
// Returns old status. Cannot call casgstatus directly, because we are racing with an
// async wakeup that might come in from netpoll. If we see Gwaiting from the readgstatus,
// it might have become Grunnable by the time we get to the cas. If we called casgstatus,
// it would loop waiting for the status to go back to Gwaiting, which it never will.
//  casgstatus(gp，oldstatus，Gcopystack)，假设 oldstatus 是 Gwaiting 或 Grunnable 。返回旧状态。无法直接调用 casgstatus ，因为可能
// 与通过 netpoll 进行异步唤醒产生 race。如果我们从 readgstatus 看到了Gwaiting ，则在到达cas时它可能已变为 Grunnable 。如果我们调用
// casgstatus，它将循环等待状态返回到 Gwaiting ，这永远不会。
//go:nosplit
func casgcopystack(gp *g) uint32 {
	for {
		oldstatus := readgstatus(gp) &^ _Gscan
		if oldstatus != _Gwaiting && oldstatus != _Grunnable {
			throw("copystack: bad status, not Gwaiting or Grunnable")
		}
		if atomic.Cas(&gp.atomicstatus, oldstatus, _Gcopystack) {
			return oldstatus
		}
	}
}

// scang blocks until gp's stack has been scanned.
// It might be scanned by scang or it might be scanned by the goroutine itself.
// Either way, the stack scan has completed when scang returns.
//  scang 会阻塞，直到已扫描 gp 的堆栈。它可能由 scang 扫描，或者可能由 goroutine 本身扫描。无论哪种方式，当 scang 返回时，堆栈扫描都已完成。
func scang(gp *g, gcw *gcWork) {
	// Invariant; we (the caller, markroot for a specific goroutine) own gp.gcscandone.
	// Nothing is racing with us now, but gcscandone might be set to true left over
	// from an earlier round of stack scanning (we scan twice per GC).
	// We use gcscandone to record whether the scan has been done during this round.
	// 不变；我们（调用者，特定 goroutine 的 markroot ）拥有 gp.gcscandone 。现在，没有什么可以与我们竞争，但是 gcscandone 可能被前一回合栈扫描设置
	// 为true（每个 GC 扫描两次）。 我们使用 gcscandone 记录在此回合中是否已完成扫描。
	gp.gcscandone = false

	// See https://golang.org/cl/21503 for justification of the yield delay.
	// 查看 https://golang.org/cl/21503 来证明延迟 yield
	const yieldDelay = 10 * 1000
	var nextYield int64

	// Endeavor to get gcscandone set to true,
	// either by doing the stack scan ourselves or by coercing gp to scan itself.
	// gp.gcscandone can transition from false to true when we're not looking
	// (if we asked for preemption), so any time we lock the status using
	// castogscanstatus we have to double-check that the scan is still not done.
	// 尝试通过我们进行堆栈扫描或强制 gp 进行自身扫描将 gcscandone 设置为 true 。 gp.gcscandone 可能一不留神（如果请求抢占）从false转换为true，
	// 因此，每当我们使用 castogscanstatus 锁定状态时，都必须仔细检查扫描是否仍未完成。
loop:
	for i := 0; !gp.gcscandone; i++ {
		switch s := readgstatus(gp); s {
		default:
			dumpgstatus(gp)
			throw("stopg: invalid status")

		case _Gdead:
			// No stack.
			//  _Gdead 状态 没有栈
			gp.gcscandone = true
			break loop

		case _Gcopystack:
		// Stack being switched. Go around again.
		// 栈在切换，再来一遍

		case _Grunnable, _Gsyscall, _Gwaiting:
			// Claim goroutine by setting scan bit.
			// Racing with execution or readying of gp.
			// The scan bit keeps them from running
			// the goroutine until we're done.
			// 通过设置扫描位来声明 goroutine 。 与执行或就绪 gp 产生竞争。扫描位使它们无法运行 goroutine ，直到扫描完成。
			if castogscanstatus(gp, s, s|_Gscan) {
				if !gp.gcscandone {
					scanstack(gp, gcw)
					gp.gcscandone = true
				}
				restartg(gp)
				break loop
			}

		case _Gscanwaiting:
		// newstack is doing a scan for us right now. Wait.
		// newstack正在扫描，等待

		case _Grunning:
			// Goroutine running. Try to preempt execution so it can scan itself.
			// The preemption handler (in newstack) does the actual scan.
			// Goroutine运行。 尝试抢占执行，以便它可以进行自我扫描。抢占处理程序（在 newstack 中）进行实际扫描。

			// Optimization: if there is already a pending preemption request
			// (from the previous loop iteration), don't bother with the atomics.
			// 优化：如果已经有一个待处理的抢占请求（来自上一个循环迭代），不需要原子操作。
			if gp.preemptscan && gp.preempt && gp.stackguard0 == stackPreempt {
				break
			}

			// Ask for preemption and self scan.
			// 请求抢占，自我扫描
			if castogscanstatus(gp, _Grunning, _Gscanrunning) {
				if !gp.gcscandone {
					gp.preemptscan = true
					gp.preempt = true
					gp.stackguard0 = stackPreempt
				}
				casfrom_Gscanstatus(gp, _Gscanrunning, _Grunning)
			}
		}

		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			procyield(10)
		} else {
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}

	gp.preemptscan = false // cancel scan request if no longer needed // 如果不再需要取消扫描请求
}

// The GC requests that this routine be moved from a scanmumble state to a mumble state.
// GC 请求将此 routine 从扫描状态更改为非扫描状态。
// 重新启动 g
func restartg(gp *g) {
	s := readgstatus(gp)
	switch s {
	default:
		dumpgstatus(gp)
		throw("restartg: unexpected status")

	case _Gdead:
	// ok

	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscansyscall:
		// 去掉 _Gscan 位
		casfrom_Gscanstatus(gp, s, s&^_Gscan)
	}
}

// stopTheWorld stops all P's from executing goroutines, interrupting
// all goroutines at GC safe points and records reason as the reason
// for the stop. On return, only the current goroutine's P is running.
// stopTheWorld must not be called from a system stack and the caller
// must not hold worldsema. The caller must call startTheWorld when
// other P's should resume execution.
//
// stopTheWorld is safe for multiple goroutines to call at the
// same time. Each will execute its own stop, and the stops will
// be serialized.
//
// This is also used by routines that do stack dumps. If the system is
// in panic or being exited, this may not reliably stop all
// goroutines.
//
//  stopTheWorld 从正在执行的 goroutine 中停止所有的 P，在 GC 安全点 safe point 中断所有 goroutine 并记录中断的原因。结果就是，只有当前 goroutine
// 的 P 正在运行。stopTheWorld 不能再系统栈上调用，调用方也不能持有 worldsema。调用方必须在其他 P 在应该恢复执行的时候调用 startTheWorld。
//
//  stopTheWorld 在多个 goroutine 间同时调用时是安全的。每个 goroutine 都会执行自己的 stop，所有的 stop 都会被有序的执行。
//
// 这个函数也会被执行 stack dump 的 routine 使用。如果系统处于 panic 或 exit 状态，这可能无法可靠地停止所有的 goroutine。
func stopTheWorld(reason string) {
	// 获取 worldsema
	semacquire(&worldsema)
	getg().m.preemptoff = reason
	systemstack(stopTheWorldWithSema)
}

// startTheWorld undoes the effects of stopTheWorld.
// startTheWorld 撤销 stopTheWorld 的影响
func startTheWorld() {
	systemstack(func() { startTheWorldWithSema(false) })
	// worldsema must be held over startTheWorldWithSema to ensure
	// gomaxprocs cannot change while worldsema is held.
	// worldsema 必须在 startTheWorldWithSema 期间被持有，以确保 gomaxprocs 保持不变。
	// 释放 worldsema
	semrelease(&worldsema)
	getg().m.preemptoff = ""
}

// Holding worldsema grants an M the right to try to stop the world
// and prevents gomaxprocs from changing concurrently.
// 持有 worldsema 授予 M 尝试 STW 的权利，并阻止 gomaxprocs 被并发更改。
var worldsema uint32 = 1

// stopTheWorldWithSema is the core implementation of stopTheWorld.
// The caller is responsible for acquiring worldsema and disabling
// preemption first and then should stopTheWorldWithSema on the system
// stack:
// stopTheWorldWithSema 是 stopTheWorld 的核心实现。调用方负责获取 worldsema 并禁止抢占，然后再系统栈上调用 stopTheWorldWithSema：
//
//	semacquire(&worldsema, 0)
//	m.preemptoff = "reason"
//	systemstack(stopTheWorldWithSema)
//
// When finished, the caller must either call startTheWorld or undo
// these three operations separately:
// 当完成时，调用方必须调用 startTheWorld ，或者分别撤销刚才的三个操作：
//
//	m.preemptoff = ""
//	systemstack(startTheWorldWithSema)
//	semrelease(&worldsema)
//
// It is allowed to acquire worldsema once and then execute multiple
// startTheWorldWithSema/stopTheWorldWithSema pairs.
// Other P's are able to execute between successive calls to
// startTheWorldWithSema and stopTheWorldWithSema.
// Holding worldsema causes any other goroutines invoking
// stopTheWorld to block.
//
//  获取 worldsema 后可以多次执行 startTheWorldWithSema/stopTheWorldWithSema 对；其他的 P 可以在连续调用 startTheWorldWithSema 和 stopTheWorldWithSema 间进行执行。
// 持有 worldsema 会导致其他 goroutine 调用的 stopTheWorld 阻塞。
func stopTheWorldWithSema() {
	_g_ := getg() // 因为在g0栈运行，所以_g_ = g0

	// If we hold a lock, then we won't be able to stop another M
	// that is blocked trying to acquire the lock.
	// 如果我们持有锁，那么我们将无法停止另一个试图获取锁而阻塞的M。
	if _g_.m.locks > 0 {
		throw("stopTheWorld: holding locks")
	}

	// 锁住调度器
	lock(&sched.lock)
	sched.stopwait = gomaxprocs       //  gomaxprocs 即 p 的数量，需要等待所有的 p 停下来
	atomic.Store(&sched.gcwaiting, 1) // 设置 gcwaiting 标志，表示我们正在等待着垃圾回收
	preemptall()                      // 设置抢占标记，希望处于运行之中的 goroutine 停下来
	// stop current P
	// 停止当前的 P
	_g_.m.p.ptr().status = _Pgcstop // Pgcstop is only diagnostic. // Pgcstop 只用于诊断.
	sched.stopwait--
	// try to retake all P's in Psyscall status
	// 尝试抢占所有在 Psyscall 状态的 P
	for _, p := range allp {
		s := p.status
		//通过修改 p 的状态为 _Pgcstop 抢占那些处于系统调用之中的 goroutine
		if s == _Psyscall && atomic.Cas(&p.status, s, _Pgcstop) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			sched.stopwait--
		}
	}
	// stop idle P's
	// 停止 空闲 P
	for {
		// 修改 idle 队列中 p 的状态为 _Pgcstop ，这样就不会被工作线程拿去使用了
		p := pidleget()
		if p == nil {
			break
		}
		p.status = _Pgcstop
		sched.stopwait--
	}
	wait := sched.stopwait > 0
	unlock(&sched.lock)

	// wait for remaining P's to stop voluntarily
	// 等待剩余的 P 主动停止
	if wait {
		for {
			// wait for 100us, then try to re-preempt in case of any races
			// 等待 100us, 然后尝试重新抢占，从而防止竞争
			if notetsleep(&sched.stopnote, 100*1000) {
				noteclear(&sched.stopnote)
				break
			}
			preemptall() // 循环中反复设置抢占标记
		}
	}

	// sanity checks
	// 健康性检测
	bad := ""
	// 此时 stopwait 应该 = 0， 并且状态应该为 _Pgcstop
	if sched.stopwait != 0 {
		bad = "stopTheWorld: not stopped (stopwait != 0)"
	} else {
		for _, p := range allp {
			if p.status != _Pgcstop {
				bad = "stopTheWorld: not stopped (status != _Pgcstop)"
			}
		}
	}
	if atomic.Load(&freezing) != 0 {
		// Some other thread is panicking. This can cause the
		// sanity checks above to fail if the panic happens in
		// the signal handler on a stopped thread. Either way,
		// we should halt this thread.
		// 其他线程 panic。如果在已经停止的线程上的信号处理程序中发生 panic，则可能导致上面的健全性检查失败。无论如何，我们都应该停止该线程。
		lock(&deadlock)
		lock(&deadlock)
	}
	if bad != "" {
		throw(bad)
	}
}

// startTheWorldWithSema 是 startTheWorld 的核心实现
func startTheWorldWithSema(emitTraceEvent bool) int64 {
	_g_ := getg()

	_g_.m.locks++ // disable preemption because it can be holding p in a local var // 禁用抢占，因为本地变量可以保留 p
	// 如果 netpoll 初始化了，将 netpoll 就绪的 goroutines 插入到调度器中
	if netpollinited() {
		list := netpoll(false) // non-blocking // 非阻塞
		injectglist(&list)
	}
	// 锁住调度器
	lock(&sched.lock)

	// 看是否需要调整 P 数量
	procs := gomaxprocs
	if newprocs != 0 {
		procs = newprocs
		newprocs = 0
	}
	// 调整 P 的数量， 返回可运行的 P 的列表
	p1 := procresize(procs)
	sched.gcwaiting = 0        //  取消 GC 等待运行
	if sched.sysmonwait != 0 { // 取消 等待系统监控
		sched.sysmonwait = 0
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)

	for p1 != nil {
		p := p1
		p1 = p1.link.ptr()
		if p.m != 0 {
			// 如果有绑定的 M ，设置下一个调度的是 p ，并唤醒 m
			mp := p.m.ptr()
			p.m = 0
			if mp.nextp != 0 {
				throw("startTheWorld: inconsistent mp->nextp")
			}
			mp.nextp.set(p)
			notewakeup(&mp.park)
		} else {
			// Start M to run P.  Do not start another M below.
			// 启动 M 来运行 P 。请勿在下面启动另一个M。
			newm(nil, p)
		}
	}

	// Capture start-the-world time before doing clean-up tasks.
	// 在执行清理任务之前，请先记录启动时间。
	startTime := nanotime()
	if emitTraceEvent {
		traceGCSTWDone()
	}

	// Wakeup an additional proc in case we have excessive runnable goroutines
	// in local queues or in the global queue. If we don't, the proc will park itself.
	// If we have lots of excessive work, resetspinning will unpark additional procs as necessary.
	// 如果我们在本地队列或全局队列中有过多的可运行的 goroutine，则唤醒一个额外的 p 。如果我们不这样做，那么 p 就会停止。
	// 如果我们有大量过多的工作，resetspinning 函数则在必要时 unpark (复始) 额外的 P 。
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
		wakep()
	}

	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in case we've cleared it in newstack // 恢复抢占请求，以防我们在新堆栈中将其清除
		_g_.stackguard0 = stackPreempt
	}

	return startTime
}

// Called to start an M.
//
// This must not split the stack because we may not even have stack
// bounds set up yet.
//
// May run during STW (because it doesn't have a P yet), so write
// barriers are not allowed.
// 启动 M ， M 的入口函数
// 该函数不允许分段栈，因为我们甚至还没有设置栈的边界。它可能会在 STW 阶段运行（因为它还没有 P），所以 write barrier 也是不允许的
//
//go:nosplit
//go:nowritebarrierrec
func mstart() {
	_g_ := getg()

	// 确定执行栈的边界。通过检查 g 执行占的边界来确定是否为系统栈
	osStack := _g_.stack.lo == 0
	if osStack {
		// Initialize stack bounds from system stack.
		// Cgo may have left stack size in stack.hi.
		// minit may update the stack bounds.
		// 根据系统栈初始化执行栈的边界。cgo 可能会离开 stack.hi 。minit 可能会更新栈的边界
		size := _g_.stack.hi
		if size == 0 {
			size = 8192 * sys.StackGuardMultiplier
		}
		_g_.stack.hi = uintptr(noescape(unsafe.Pointer(&size)))
		_g_.stack.lo = _g_.stack.hi - size + 1024
	}
	// Initialize stack guards so that we can start calling
	// both Go and C functions with stack growth prologues.
	// 初始化堆栈守卫，以便我们可以使用堆栈增长 prologue (序言) 开始调用Go和C函数。
	_g_.stackguard0 = _g_.stack.lo + _StackGuard
	_g_.stackguard1 = _g_.stackguard0
	// 启动 M
	mstart1()

	// Exit this thread.
	// 退出线程
	if GOOS == "windows" || GOOS == "solaris" || GOOS == "plan9" || GOOS == "darwin" || GOOS == "aix" {
		// Window, Solaris, Darwin, AIX and Plan 9 always system-allocate
		// the stack, but put it in _g_.stack before mstart,
		// so the logic above hasn't set osStack yet.
		// Window，Solaris，Darwin，AIX和Plan 9始终对栈进行系统分配，但将其放在mstart之前的_g_.stack中，因此上述逻辑尚未设置osStack。
		osStack = true
	}
	// 退出线程
	mexit(osStack)
}

func mstart1() {
	_g_ := getg()

	// 检查当前执行的 g 是不是 g0
	if _g_ != _g_.m.g0 {
		throw("bad runtime·mstart")
	}

	// Record the caller for use as the top of stack in mcall and
	// for terminating the thread.
	// We're never coming back to mstart1 after we call schedule,
	// so other calls can reuse the current frame.
	// 这里会记录前一个调用者的状态， 包含 PC , SP 以及其他信息。这份记录会当作最初栈 (top stack)，给之后的 mcall 调用，也用来结束那个线程。
	// 接下來在 mstart1 调用到 schedule 之后就再也不会回到这个地方了，所以其他调用可以重用当前帧。

	// 借助编译器的帮助获取 PC 和 SP , 然后在 save 中更新当前 G 的 sched (type gobuf) 的一些成员， 保存调用者的 pc 和 sp ，让日后其他执行者执行 gogo 函数的时候使用。
	save(getcallerpc(), getcallersp())
	asminit() // 初始化汇编，但是 amd64 架构下不需要执行任何代码就立刻返回，其他像是 arm、386 才有一些需在这里设定一些 CPU 相关的內容。
	minit()   // 初始化m 包括信号栈和信号掩码，procid

	// Install signal handlers; after minit so that minit can
	// prepare the thread to be able to handle the signals.
	// 设置信号 handler ；在 minit 之后，因为 minit 可以准备处理信号的的线程
	if _g_.m == &m0 {
		// 在当前的 goroutine 的所属执行者是 m0 的情況下进入 mstartm0 函数，正式启动在此之前的 signal 处理设定，其中最关键的是 initsig 函数。
		mstartm0()
	}

	// 执行启动函数
	if fn := _g_.m.mstartfn; fn != nil {
		fn()
	}

	// 如果当前 m 并非 m0，则要求绑定 p
	if _g_.m != &m0 {
		acquirep(_g_.m.nextp.ptr())
		_g_.m.nextp = 0
	}

	// 彻底准备好，开始调度，永不返回
	schedule()
}

// mstartm0 implements part of mstart1 that only runs on the m0.
//
// Write barriers are allowed here because we know the GC can't be
// running yet, so they'll be no-ops.
//
// mstartm0 实现了一部分 mstart1，只运行在 m0 上。允许 write barrier，因为我们知道 GC 此时还不能运行，因此他们没有操作。
//go:yeswritebarrierrec
func mstartm0() {
	// Create an extra M for callbacks on threads not created by Go.
	// An extra M is also needed on Windows for callbacks created by
	// syscall.NewCallback. See issue #6751 for details.
	// 创建一个额外的 M 处理 non-Go 线程（cgo 调用中产生的线程）的回调，并且只创建一个。windows 上也需要额外 M 来处理 syscall.NewCallback 产生的回调，见 issue #6751
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		cgoHasExtraM = true
		newextram()
	}
	// 初始化信号。
	initsig(false)
}

// mexit tears down and exits the current thread.
//
// Don't call this directly to exit the thread, since it must run at
// the top of the thread stack. Instead, use gogo(&_g_.m.g0.sched) to
// unwind the stack to the point that exits the thread.
//
// It is entered with m.p != nil, so write barriers are allowed. It
// will release the P before exiting.
//  mexit 销毁并退出当前线程。请不要直接调用来退出线程，因为它必须在线程栈顶上运行。相反，请使用 gogo(&_g_.m.g0.sched) 将栈移动到退出线程的位置。
//  _g_.m.g0.sched 在 mstart1 中 save 保存的。当调用时，m.p != nil，因此允许写屏障。在退出前它会释放当前绑定的 P 。
//
//go:yeswritebarrierrec
func mexit(osStack bool) {
	g := getg()
	m := g.m

	if m == &m0 {
		// This is the main thread. Just wedge it.
		//
		// On Linux, exiting the main thread puts the process
		// into a non-waitable zombie state. On Plan 9,
		// exiting the main thread unblocks wait even though
		// other threads are still running. On Solaris we can
		// neither exitThread nor return from mstart. Other
		// bad things probably happen on other platforms.
		//
		// We could try to clean up this M more before wedging
		// it, but that complicates signal handling.

		// 主线程
		//
		// 在 linux 中，退出主线程会导致进程变为僵尸进程。在 plan 9 中，退出主线程将取消阻塞等待，其他线程仍在运行。在 Solaris 中
		// 我们既不能 exitThread 也不能返回到 mstart 中。其他系统上可能发生别的糟糕的事情。
		// 我们可以尝试退出之前清理当前 M ，但信号处理非常复杂。
		handoffp(releasep())       // 让出当前的 P
		lock(&sched.lock)          // 锁住调度器
		sched.nmfreed++            // 更新统计信息，释放的 M 数量
		checkdead()                // 检测死锁
		unlock(&sched.lock)        // 解锁调度器
		notesleep(&m.park)         // park 主线程，在此阻塞
		throw("locked m0 woke up") // 锁住的 m0 不会被唤醒
	}

	sigblock() // 阻塞信号
	unminit()  //  M 反初始化，与 minit 相对

	// Free the gsignal stack.
	// 释放 gsignal 栈
	if m.gsignal != nil {
		stackfree(m.gsignal.stack)
	}

	// Remove m from allm.
	// 从 allm 移除 m
	lock(&sched.lock)
	for pprev := &allm; *pprev != nil; pprev = &(*pprev).alllink {
		if *pprev == m {
			*pprev = m.alllink
			goto found
		}
	}
	// 如果没找到则是异常状态，说明 allm 管理出错
	throw("m not found in allm")
found:
	// 如果不是 OS 栈，osStack 在 mstart 中设置的
	if !osStack {
		// Delay reaping m until it's done with the stack.
		//
		// If this is using an OS stack, the OS will free it
		// so there's no need for reaping.
		// 延迟回收 m 直到完成栈处理。如果使用的是 OS 栈，则 OS 会释放它，因此无需收获。
		atomic.Store(&m.freeWait, 1)
		// Put m on the free list, though it will not be reaped until
		// freeWait is 0. Note that the free list must not be linked
		// through alllink because some functions walk allm without
		// locking, so may be using alllink.
		// 将 m 放在空闲列表上，尽管直到 freeWait 为 0 才可以回收它。请注意，空闲列表一定不能通过 alllink 链接，
		// 因为某些函数遍历 allm 时没有加锁，因此可能正在使用 alllink 。
		m.freelink = sched.freem
		sched.freem = m
	}
	unlock(&sched.lock)

	// Release the P.
	// 让出当前的 P
	handoffp(releasep())
	// After this point we must not have write barriers.
	// 从此刻开始必须没有 write barrier

	// Invoke the deadlock detector. This must happen after
	// handoffp because it may have started a new M to take our
	// P's work.
	// 调用 deadlock detector (死锁探测器)。 必须在让出 P 之后执行，因为它可能启动一个新的 M 并拿走 P 的 work 。
	lock(&sched.lock)
	sched.nmfreed++
	checkdead() // 检测死锁
	unlock(&sched.lock)

	if osStack {
		// Return from mstart and let the system thread
		// library free the g0 stack and terminate the thread.
		// 从 mstart 返回，让系统线程库释放 g0 栈并终止线程。
		return
	}

	// mstart is the thread's entry point, so there's nothing to
	// return to. Exit the thread directly. exitThread will clear
	// m.freeWait when it's done with the stack and the m can be
	// reaped.
	//  mstart 是线程地入口函数，因此没有什么需要返回。栈处理完成后， exitThread 将清除 m.freeWait ，并且回收 m 。
	exitThread(&m.freeWait)
}

// forEachP calls fn(p) for every P p when p reaches a GC safe point.
// If a P is currently executing code, this will bring the P to a GC
// safe point and execute fn on that P. If the P is not executing code
// (it is idle or in a syscall), this will call fn(p) directly while
// preventing the P from exiting its state. This does not ensure that
// fn will run on every CPU executing Go code, but it acts as a global
// memory barrier. GC uses this as a "ragged barrier."
//
// The caller must hold worldsema.
// 当 P 到达 GC 安全点时，forEachP 为每个 P 调用fn(p)。 如果 P 当前正在执行代码，这会将 P 带到 GC 安全点并在该 P 上执行 fn 。
// 如果 P 此时没有执行代码（空闲或者在系统调用），这会直接调用 fn(p) ，同时防止P退出其状态。 这不能确保 fn 将在每个CPU上运行执行
//  Go 代码的，但是它会充当全局 memory barrier 。 GC 将其用作"ragged (粗糙的) barrier"。
// 调用者必须持有worldsema。
//
//go:systemstack
func forEachP(fn func(*p)) {
	// 获取当前 goroutine 的 m
	mp := acquirem()
	_p_ := getg().m.p.ptr()

	// 锁住调度器
	lock(&sched.lock)
	// 没有达到安全点
	if sched.safePointWait != 0 {
		throw("forEachP: sched.safePointWait != 0")
	}
	sched.safePointWait = gomaxprocs - 1
	sched.safePointFn = fn

	// Ask all Ps to run the safe point function.
	// 要求所有 Ps 运行安全点函数。
	for _, p := range allp {
		// 当前的 p 不执行，后面有执行
		if p != _p_ {
			// 标记一下，则在下一个 safe-point 运行 sched.safePointFn
			atomic.Store(&p.runSafePointFn, 1)
		}
	}
	// 告诉所有 goroutine ，它们已被抢占，应该停止。
	preemptall()

	// Any P entering _Pidle or _Psyscall from now on will observe
	// p.runSafePointFn == 1 and will call runSafePointFn when
	// changing its status to _Pidle/_Psyscall.
	// 从此刻开始进入 _Pidle 或 _Psyscall 的任何 P 都将观察 p.runSafePointFn == 1 ，并且在将其状态更改为 _Pidle/_Psyscall 时调用 runSafePointFn 。

	// Run safe point function for all idle Ps. sched.pidle will
	// not change because we hold sched.lock.
	// 对所有空闲 P 执行安全点函数。 sched.pidle 不会更改，因为我们持有 sched.lock 。
	for p := sched.pidle.ptr(); p != nil; p = p.link.ptr() {
		if atomic.Cas(&p.runSafePointFn, 1, 0) {
			fn(p)
			sched.safePointWait--
		}
	}

	wait := sched.safePointWait > 0
	unlock(&sched.lock)

	// Run fn for the current P.
	// 当前 p 执行 fn
	fn(_p_)

	// Force Ps currently in _Psyscall into _Pidle and hand them
	// off to induce safe point function execution.
	// 将当前在 _Psyscall 中的 P 强制进入 _Pidle 并将其让出以引发安全点函数执行。
	for _, p := range allp {
		s := p.status
		if s == _Psyscall && p.runSafePointFn == 1 && atomic.Cas(&p.status, s, _Pidle) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			// 将 P 从系统调用中移开或将 M 锁定
			handoffp(p)
		}
	}

	// Wait for remaining Ps to run fn.
	// 等待剩余的 P 运行 fn 。
	if wait {
		for {
			// Wait for 100us, then try to re-preempt in
			// case of any races.
			//
			// Requires system stack.
			// 等待 100us, 然后尝试重新抢占，从而防止竞争。需要系统栈。
			if notetsleep(&sched.safePointNote, 100*1000) {
				noteclear(&sched.safePointNote)
				break
			}
			preemptall() // 循环中反复设置抢占标记
		}
	}

	// 此时 sched.safePointWait 应该 = 0
	if sched.safePointWait != 0 {
		throw("forEachP: not done")
	}
	// 此时 p.runSafePointFn 应该 = 0
	for _, p := range allp {
		if p.runSafePointFn != 0 {
			throw("forEachP: P did not run fn")
		}
	}

	// 加锁，清空safePointFn
	lock(&sched.lock)
	sched.safePointFn = nil
	unlock(&sched.lock)
	releasem(mp)
}

// runSafePointFn runs the safe point function, if any, for this P.
// This should be called like
//  runSafePointFn 为此 P 运行安全点函数（如果有的话）。因该像下面这样调用
//
//     if getg().m.p.runSafePointFn != 0 {
//         runSafePointFn()
//     }
//
// runSafePointFn must be checked on any transition in to _Pidle or
// _Psyscall to avoid a race where forEachP sees that the P is running
// just before the P goes into _Pidle/_Psyscall and neither forEachP
// nor the P run the safe-point function.
// 必须在向 _Pidle 或 _Psyscall 进行任何转变时检查 runSafePointFn ，以避免发生 race ，其中 forEachP 认为 P 在 P 进入 _Pidle/_Psyscall 之前刚好正在运行，
// 并且 forEachP 和 P 都不执行安全点函数。
func runSafePointFn() {
	p := getg().m.p.ptr()
	// Resolve the race between forEachP running the safe-point
	// function on this P's behalf and this P running the
	// safe-point function directly.
	// 解决 forEachP 在 P 的代表上执行安全点函数 和 P 直接执行安全按点函数 之间的 race 。原子操作 p.runSafePointFn
	if !atomic.Cas(&p.runSafePointFn, 1, 0) {
		return
	}
	sched.safePointFn(p) // 执行安全点函数
	lock(&sched.lock)
	// 执行之后，更新 safePointWait ，如果 == 0 则唤醒safePointNote
	sched.safePointWait--
	if sched.safePointWait == 0 {
		notewakeup(&sched.safePointNote)
	}
	unlock(&sched.lock)
}

// When running with cgo, we call _cgo_thread_start
// to start threads for us so that we can play nicely with
// foreign code.
// 使用 cgo 运行时，我们调用 _cgo_thread_start 为我们启动线程，以便我们可以很好地使用外来代码。
var cgoThreadStart unsafe.Pointer

type cgothreadstart struct {
	g   guintptr
	tls *uint64
	fn  unsafe.Pointer
}

// Allocate a new m unassociated with any thread.
// Can use p for allocation context if needed.
// fn is recorded as the new m's m.mstartfn.
// 分配一个不与任何线程相关联新的 M 。如果有必要可以用 p 来分配上下文。 fn 是新 M 的启动函数(m.mstartfn)。
//
// This function is allowed to have write barriers even if the caller
// isn't because it borrows _p_.
// 这个函数允许写障碍，即使调用方不是写障碍，因为它借用_p_。
//
//go:yeswritebarrierrec
func allocm(_p_ *p, fn func()) *m {
	_g_ := getg()
	_g_.m.locks++ // disable GC because it can be called from sysmon // 禁止 GC 因为可以被 sysmon 调用
	if _g_.m.p == 0 {
		acquirep(_p_) // temporarily borrow p for mallocs in this function // 临时借用 _p_ 在此函数中用来分配内存， _p_ 和 当前 m 关联
	}

	// Release the free M list. We need to do this somewhere and
	// this may free up a stack we can use.
	// 释放空闲的 M 列表。 我们需要在某个地方执行此操作，这可能会释放可用的栈。
	if sched.freem != nil {
		lock(&sched.lock)
		var newList *m
		// 将 freeWait == 0 时才能安全的释放 g0 ， 否则继续串起来，不删除。 mexit 中设置放不是系统栈的时候设置 freeWait
		for freem := sched.freem; freem != nil; {
			if freem.freeWait != 0 {
				next := freem.freelink
				freem.freelink = newList
				newList = freem
				freem = next
				continue
			}
			//  freeWait == 0 时候，安全的释放 g0
			stackfree(freem.g0.stack)
			freem = freem.freelink
		}
		sched.freem = newList
		unlock(&sched.lock)
	}

	// 创建 M
	mp := new(m)
	mp.mstartfn = fn
	mcommoninit(mp) //  M 通用初始化

	// In case of cgo or Solaris or Darwin, pthread_create will make us a stack.
	// Windows and Plan 9 will layout sched stack on OS stack.
	// 对于 cgo 或 Solaris 或 Darwin ， pthread_create 将使为我们创建栈。 Windows 和 Plan 9 将在操作系统栈上安排 sched stack 。
	if iscgo || GOOS == "solaris" || GOOS == "windows" || GOOS == "plan9" || GOOS == "darwin" {
		mp.g0 = malg(-1)
	} else {
		mp.g0 = malg(8192 * sys.StackGuardMultiplier)
	}
	mp.g0.m = mp

	// 如果当前 p == 当前 m 绑定的 p ，则取消关联当前 p 和 m
	if _p_ == _g_.m.p.ptr() {
		releasep()
	}
	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in case we've cleared it in newstack // 在 newstack 中清除了抢占请求的情况下恢复抢占请求
		_g_.stackguard0 = stackPreempt
	}

	return mp
}

// needm is called when a cgo callback happens on a
// thread without an m (a thread not created by Go).
// In this case, needm is expected to find an m to use
// and return with m, g initialized correctly.
// Since m and g are not set now (likely nil, but see below)
// needm is limited in what routines it can call. In particular
// it can only call nosplit functions (textflag 7) and cannot
// do any scheduling that requires an m.
// 当 cgo 回调发生在没有 m 的线程（不是Go创建的线程）上时，将调用 needm 。在这种情况下，需要 needm 找到要使用的m并返回正确初始化的 m 和 g。
// 由于现在尚未设置 m 和 g （类似于nil，但请参见下文），因此 needm 在可以调用的 routines 中受到限制。特别是，它只能调用 nosplit 函数(textflag 7)，
// 而不能执行任何需要m的调度。
//
// In order to avoid needing heavy lifting here, we adopt
// the following strategy: there is a stack of available m's
// that can be stolen. Using compare-and-swap
// to pop from the stack has ABA races, so we simulate
// a lock by doing an exchange (via Casuintptr) to steal the stack
// head and replace the top pointer with MLOCKED (1).
// This serves as a simple spin lock that we can use even
// without an m. The thread that locks the stack in this way
// unlocks the stack by storing a valid stack head pointer.
// 为了避免繁重的工作，我们采取以下策略：有一个 m 的栈可以盗用。使用 CAS 从此栈中弹出会产生 ABA race，因此我们通过进行一次交换（通过 Casuintptr）来模拟锁，
// 以窃取栈头并将顶部指针替换为 MLOCKED (1)。 这是一个简单的自旋锁，即使没有 m 也可以使用。以这种方式锁定栈的线程通过存储有效的堆栈头指针来解锁栈。
//
// CAS操作可能带来ABA问题，因为CAS操作需要在操作值的时候，检查值有没有发生变化，如果没有发发生变化则更新。如果一个值原理是A，变成了B，又变成了A，那么使用CAS进行
// 检查时会认为它的值没有变化，但是实际上却变了。
//
// In order to make sure that there is always an m structure
// available to be stolen, we maintain the invariant that there
// is always one more than needed. At the beginning of the
// program (if cgo is in use) the list is seeded with a single m.
// If needm finds that it has taken the last m off the list, its job
// is - once it has installed its own m so that it can do things like
// allocate memory - to create a spare m and put it on the list.
// 为了确保总是有一个 m 结构可被盗用，我们保持总比需求的多一个。在程序的开头（如果正在使用cgo），列表以单个 m 作为种子。 如果 needm 发现已从列表中删除了最后一个m，
// 则其工作是创建备用m并将其放在列表中，一旦获取了自己的 m 以使其可以执行诸如分配内存之类的工作。
//
// Each of these extra m's also has a g0 and a curg that are
// pressed into service as the scheduling stack and current
// goroutine for the duration of the cgo callback.
// 每个额外的 m 还具有一个 g0 和一个 curg ，它们在 cgo 回调期间作为调度栈和当前 goroutine 被压入服务。
//
// When the callback is done with the m, it calls dropm to
// put the m back on the list.
// 用 m 完成回调时，它将调用 dropm 将 m 重新放置在列表中。
//go:nosplit
func needm(x byte) {
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		// Can happen if C/C++ code calls Go from a global ctor.
		// Can also happen on Windows if a global ctor uses a
		// callback created by syscall.NewCallback. See issue #6751
		// for details.
		//
		// Can not throw, because scheduler is not initialized yet.
		// 如果 C/C++ 代码从全局 ctor(构造函数) 调用 Go ，则可能发生。 如果全局 ctor 使用 syscall.NewCallback 创建的回调，则也可能在Windows上发生。
		// 无法抛出，因为调度程序尚未初始化。
		write(2, unsafe.Pointer(&earlycgocallback[0]), int32(len(earlycgocallback)))
		exit(1)
	}

	// Lock extra list, take head, unlock popped list.
	// nilokay=false is safe here because of the invariant above,
	// that the extra list always contains or will soon contain
	// at least one m.
	// 锁定额外的 m 列表，获取 head ，解锁弹出的列表。 nilokay=false 在这里是安全的，因为上面是不变的，额外列表始终包含或将很快包含至少一个m。
	mp := lockextra(false)

	// Set needextram when we've just emptied the list,
	// so that the eventual call into cgocallbackg will
	// allocate a new m for the extra list. We delay the
	// allocation until then so that it can be done
	// after exitsyscall makes sure it is okay to be
	// running at all (that is, there's no garbage collection
	// running right now).
	// 清空列表后，请设置 needextram ，以便最终调用 cgocallbackg 会为额外的列表分配新的 m 。我们将分配推迟到那时，以便可以在 exitsyscall 之后完成，
	// 确保完全可以运行（也就是说，现在没有垃圾收集在运行）。
	mp.needextram = mp.schedlink == 0 // 判断是否额外的 m 用完了
	extraMCount--                     // 额外的 M 递减
	unlockextra(mp.schedlink.ptr())   // 解锁

	// Save and block signals before installing g.
	// Once g is installed, any incoming signals will try to execute,
	// but we won't have the sigaltstack settings and other data
	// set up appropriately until the end of minit, which will
	// unblock the signals. This is the same dance as when
	// starting a new m to run Go code via newosproc.
	// 设置 g 之前，请保存阻塞信号。一旦设置了 g ，任何传入的信号都将尝试执行，但是在 minit 结束之前，我们尚未设置 sigaltstack 和其他数据，
	// 这将取消阻塞信号。 这与通过 newosproc 启动新的 m 以运行 Go 代码时的一样。
	msigsave(mp) // 保存 当前 m 的 signal mask 到 mp.sigmask
	sigblock()   // 阻塞所有信号

	// Install g (= m->g0) and set the stack bounds
	// to match the current stack. We don't actually know
	// how big the stack is, like we don't know how big any
	// scheduling stack is, but we assume there's at least 32 kB,
	// which is more than enough for us.
	// 设置当前的 g (= m->g0) 并设置栈边界以匹配当前堆栈。实际上，我们不知道栈有多大，就像我们不知道任何调度栈有多大一样，但是我们假设至少有32 kB，对于我们来说足够了。
	setg(mp.g0)
	_g_ := getg()
	_g_.stack.hi = uintptr(noescape(unsafe.Pointer(&x))) + 1024
	_g_.stack.lo = uintptr(noescape(unsafe.Pointer(&x))) - 32*1024
	_g_.stackguard0 = _g_.stack.lo + _StackGuard

	// Initialize this thread to use the m.
	// 初始线程来舒勇 m
	asminit() // 初始化汇编，但是 amd64 架构下不需要执行任何代码就立刻返回，其他像是 arm、386 才有一些需在这里设定一些 CPU 相关的內容。
	minit()   // 初始化m 包括信号栈和信号掩码，procid

	// mp.curg is now a real goroutine.
	//  mp.curg 现在不是真正的 goroutine
	casgstatus(mp.curg, _Gdead, _Gsyscall) // 设置为 _Gsyscall 状态
	atomic.Xadd(&sched.ngsys, -1)          // 更新统计信息
}

var earlycgocallback = []byte("fatal error: cgo callback before cgo call\n")

// newextram allocates m's and puts them on the extra list.
// It is called with a working local m, so that it can do things
// like call schedlock and allocate.
//  newextram 分配一个 m 并将其放入 extra 列表中。它会被工作中的本地 m 调用，因此它能够做一些调用 schedlock 和 allocate 类似的事情。
func newextram() {
	// 交换 extraMWaiters 和 0，原子性。 extraMWaiters = 0, c = extraMWaiters
	c := atomic.Xchg(&extraMWaiters, 0)
	if c > 0 {
		for i := uint32(0); i < c; i++ {
			oneNewExtraM()
		}
	} else {
		// Make sure there is at least one extra M.
		// 确保至少有一个额外的 M 。
		mp := lockextra(true)
		unlockextra(mp)
		if mp == nil {
			oneNewExtraM()
		}
	}
}

// oneNewExtraM allocates an m and puts it on the extra list.
//  oneNewExtraM 分配一个 m 并将其放入 extra list 中
func oneNewExtraM() {
	// Create extra goroutine locked to extra m.
	// The goroutine is the context in which the cgo callback will run.
	// The sched.pc will never be returned to, but setting it to
	// goexit makes clear to the traceback routines where
	// the goroutine stack ends.
	// 创建额外 goroutine 锁定到额外 m 上。 goroutine 是将在其中运行 cgo 回调的上下文。 sched.pc 将永远不会返回，但是将其设置为 goexit ，
	// 可以使 goroutine 栈结束的回溯例程清晰可见。
	mp := allocm(nil, nil)
	gp := malg(4096)
	gp.sched.pc = funcPC(goexit) + sys.PCQuantum
	gp.sched.sp = gp.stack.hi
	gp.sched.sp -= 4 * sys.RegSize // extra space in case of reads slightly beyond frame
	gp.sched.lr = 0
	gp.sched.g = guintptr(unsafe.Pointer(gp))
	gp.syscallpc = gp.sched.pc
	gp.syscallsp = gp.sched.sp
	gp.stktopsp = gp.sched.sp
	gp.gcscanvalid = true
	gp.gcscandone = true
	// malg returns status as _Gidle. Change to _Gdead before
	// adding to allg where GC can see it. We use _Gdead to hide
	// this from tracebacks and stack scans since it isn't a
	// "real" goroutine until needm grabs it.
	// malg 返回状态为 _Gidle。 在添加到 GC 可以看到的 allg 之前，更改为 _Gdead。 我们使用 _Gdead 将其隐藏在回溯和栈扫描中，因为在需要之前，它不是真正的 goroutine。
	casgstatus(gp, _Gidle, _Gdead)
	gp.m = mp
	mp.curg = gp
	mp.lockedInt++
	// m 和 g 绑定
	mp.lockedg.set(gp)
	gp.lockedm.set(mp)
	gp.goid = int64(atomic.Xadd64(&sched.goidgen, 1))
	if raceenabled {
		gp.racectx = racegostart(funcPC(newextram) + sys.PCQuantum)
	}
	// put on allg for garbage collector
	// 放到 allg 中，可以被 gc 处理
	allgadd(gp)

	// gp is now on the allg list, but we don't want it to be
	// counted by gcount. It would be more "proper" to increment
	// sched.ngfree, but that requires locking. Incrementing ngsys
	// has the same effect.
	//  gp 现在在 allg 列表中，但我们不希望 gcount 将其计入。 增加 sched.ngfree (sched.gFree.n) 会更合适，但这需要锁定。 递增 ngsys 具有相同的效果。
	atomic.Xadd(&sched.ngsys, +1)

	// Add m to the extra list.
	// 把 m 添加到 extra list
	mnext := lockextra(true)
	mp.schedlink.set(mnext) // 设置为 下一个 m
	extraMCount++
	unlockextra(mp)
}

// dropm is called when a cgo callback has called needm but is now
// done with the callback and returning back into the non-Go thread.
// It puts the current m back onto the extra list.
// 当 cgo 回调调用了 needm 时，将调用 dropm ，但现在已通过该回调完成并返回非 Go 线程。它将当前 m 返回到额外列表中。
//
// The main expense here is the call to signalstack to release the
// m's signal stack, and then the call to needm on the next callback
// from this thread. It is tempting to try to save the m for next time,
// which would eliminate both these costs, but there might not be
// a next time: the current thread (which Go does not control) might exit.
// If we saved the m for that thread, there would be an m leak each time
// such a thread exited. Instead, we acquire and release an m on each
// call. These should typically not be scheduling operations, just a few
// atomics, so the cost should be small.
// 这里的主要开销是对 signalstack 的调用以释放 m 的信号栈，然后在该线程的下一个回调中对 needm 的调用。尝试为下次使用将 m 保存是很诱人的，
// 这样可以消除这两个开销，但是可能不会这个下一次调用 (needm)：当前线程（Go不能控制）可能会退出。如果我们为该线程保存了 m ，则每次退出该线
// 程时都会发生 m 泄漏。 相反，我们在每次调用中获取并释放一个 m 。 这些通常不应该是调度操作，而只是几个原子，因此成本应该很小。
//
// TODO(rsc): An alternative would be to allocate a dummy pthread per-thread
// variable using pthread_key_create. Unlike the pthread keys we already use
// on OS X, this dummy key would never be read by Go code. It would exist
// only so that we could register at thread-exit-time destructor.
// That destructor would put the m back onto the extra list.
// This is purely a performance optimization. The current version,
// in which dropm happens on each cgo call, is still correct too.
// We may have to keep the current version on systems with cgo
// but without pthreads, like Windows.
// 一种替代方法是使用 pthread_key_create 分配一个虚拟 pthread 每个线程的变量。 与我们已经在 OS X 上使用过的 pthread keys 不同，该 key 永远
// 不会被 Go 代码读取。 它只会存在，以便我们可以注册一个在线程退出时的析构函数。该析构函数会将 m 放回到额外的列表中。这纯粹是性能优化。 当前版本
// （每个cgo调用中都发生dropm）仍然是正确的。 我们可能必须在具有 cgo 但没有 pthreads 的系统上保留当前版本，例如Windows。
func dropm() {
	// Clear m and g, and return m to the extra list.
	// After the call to setg we can only call nosplit functions
	// with no pointer manipulation.
	// 清除 m 和 g ，然后将 m 返回到额外列表。在调用 setg 之后，我们只能在没有指针操作的情况下调用 nosplit 函数。
	mp := getg().m

	// Return mp.curg to dead state.
	// 将 mp.curg 返回到 _Gdead 状态
	casgstatus(mp.curg, _Gsyscall, _Gdead)
	atomic.Xadd(&sched.ngsys, +1)

	// Block signals before unminit.
	// Unminit unregisters the signal handling stack (but needs g on some systems).
	// Setg(nil) clears g, which is the signal handler's cue not to run Go handlers.
	// It's important not to try to handle a signal between those two steps.
	// 在 unminit 之前阻塞信号。 Unminit 取消注册信号处理栈（但在某些系统上需要g）。 Setg(nil) 清除 g ，这是信号处理程序不运行 Go 处理程序的暗示。
	// 重要的是不要尝试在这两个步骤之间处理信号。
	sigmask := mp.sigmask // 记录下信号掩码
	sigblock()            // 阻塞信号
	unminit()             //  M 反初始化，与 minit 相对

	// 把 m 添加到 extra list
	mnext := lockextra(true)
	extraMCount++
	mp.schedlink.set(mnext) // 设置为 下一个 m

	setg(nil) // 设置当前 g 为 nil

	// Commit the release of mp.
	// 提交 mp 的释放。
	unlockextra(mp)

	msigrestore(sigmask) // 设置当前线程的信号掩码为 sigmask
}

// A helper function for EnsureDropM.
//  EnsureDropM 的帮助函数
func getm() uintptr {
	return uintptr(unsafe.Pointer(getg().m))
}

var extram uintptr     // 额外列表头，如果为 0 ，则表示没有；如果为 1 ，则表示锁住了；其他表示未加锁，并且有 m
var extraMCount uint32 // Protected by lockextra // 通过 lockextra 函数来保护
var extraMWaiters uint32

// lockextra locks the extra list and returns the list head.
// The caller must unlock the list by storing a new list head
// to extram. If nilokay is true, then lockextra will
// return a nil list head if that's what it finds. If nilokay is false,
// lockextra will keep waiting until the list head is no longer nil.
//  lockextra 锁定额外列表并返回列表头。 调用者必须通过将新的列表头存储到 extram 来解锁列表。如果 nilokay 为 true ，则 lockextra 将
// 返回一个 nil 列表头（如果找到的就是 nil 的话）。 如果 nilokay 为 false ，lockextra 将一直等待直到列表头不再为 nil 。
//  nilokay 是否可以返回 nil 的 m
//go:nosplit
func lockextra(nilokay bool) *m {
	const locked = 1 // 锁主列表头的标记

	incr := false // extraMWaiters 是否增加了的标记
	for {
		// 获取列表头
		old := atomic.Loaduintptr(&extram)
		if old == locked { // 如果列表头为 locked(1)， 则表示锁住了，让出 cpu
			//  SYS_sched_yield 系统调用，会让出当前线程的CPU占有权，然后把线程放到静态优先队列的尾端，然后一个新的线程会占用CPU，见： sys_linux_amd64.s
			yield := osyield
			yield()
			continue
		}
		// 如果没有 m，并且不可以返回 nil 的 m ， 休眠一会儿继续 ，直到找到 m
		if old == 0 && !nilokay {
			if !incr {
				// Add 1 to the number of threads
				// waiting for an M.
				// This is cleared by newextram.
				// 在等待 M 的线程数上加 1 。 newextram 清除。
				atomic.Xadd(&extraMWaiters, 1)
				incr = true
			}
			usleep(1) // 休眠 1 微秒，最终调用 SYS_nanosleep 系统调用，见： sys_linux_amd64.s
			continue
		}
		//  cas 原子操作，如 extram == old 相等， extram == locked(1)，也就是锁住 extram ，然后返回 old ， 当 nilokay = true 的时候，m 可能为 0
		if atomic.Casuintptr(&extram, old, locked) {
			return (*m)(unsafe.Pointer(old))
		}
		yield := osyield
		yield()
		continue
	}
}

// 解锁 extram
//go:nosplit
func unlockextra(mp *m) {
	atomic.Storeuintptr(&extram, uintptr(unsafe.Pointer(mp)))
}

// execLock serializes exec and clone to avoid bugs or unspecified behaviour
// around exec'ing while creating/destroying threads.  See issue #19546.
// execLock 序列化 exec 和 clone 以避免在创建/销毁线程时执行错误或未指定的行为。见 issue #19546。
var execLock rwmutex

// newmHandoff contains a list of m structures that need new OS threads.
// This is used by newm in situations where newm itself can't safely
// start an OS thread.
//  newmHandoff 包含需要新 OS 线程的 m 的列表。在 newm 本身无法安全启动 OS 线程的情况下，newm 会使用它。
var newmHandoff struct {
	lock mutex

	// newm points to a list of M structures that need new OS
	// threads. The list is linked through m.schedlink.
	//  newm 指向需要新 OS 线程的 M 结构列表。 该列表通过 m.schedlink 链接。
	newm muintptr

	// waiting indicates that wake needs to be notified when an m
	// is put on the list.
	//  waiting 表示当 m 列入列表时需要通知唤醒。templateThread 中等待 wake
	waiting bool
	wake    note

	// haveTemplateThread indicates that the templateThread has
	// been started. This is not protected by lock. Use cas to set
	// to 1.
	// haveTemplateThread 表示 templateThread 已经启动。没有锁保护，使用 cas 设置为 1。
	haveTemplateThread uint32
}

// Create a new m. It will start off with a call to fn, or else the scheduler.
// fn needs to be static and not a heap allocated closure.
// May run with m.p==nil, so write barriers are not allowed.
// 创建一个新的 M 。它会启动并调用 fn 或调度器。fn 必须是静态、非堆上分配的闭包。可能运行m.p == nil，因此不允许 write barrier 。
//go:nowritebarrierrec
func newm(fn func(), _p_ *p) {
	// 分配一个 m
	mp := allocm(_p_, fn)
	// 设置 p 用于后续绑定
	mp.nextp.set(_p_)
	// 设置 signal mask
	mp.sigmask = initSigmask
	if gp := getg(); gp != nil && gp.m != nil && (gp.m.lockedExt != 0 || gp.m.incgo) && GOOS != "plan9" {
		// We're on a locked M or a thread that may have been
		// started by C. The kernel state of this thread may
		// be strange (the user may have locked it for that
		// purpose). We don't want to clone that into another
		// thread. Instead, ask a known-good thread to create
		// the thread for us.
		//
		// This is disabled on Plan 9. See golang.org/issue/22227.
		//
		// TODO: This may be unnecessary on Windows, which
		// doesn't model thread creation off fork.
		// 我们处于一个锁定的 M 或可能由 C 启动的线程。这个线程的内核状态可能很奇怪（用户可能已将其锁定）。我们不想将其克隆到另一个线程。
		// 相反，请求一个已知状态良好的线程来创建给我们的线程。在 plan9 上禁用，见 golang.org/issue/22227
		lock(&newmHandoff.lock)
		if newmHandoff.haveTemplateThread == 0 {
			throw("on a locked thread with no template thread")
		}
		// 添加 m 到 mp.schedlink
		mp.schedlink = newmHandoff.newm
		newmHandoff.newm.set(mp)
		// 如果加入之后需要唤醒 templateThread ， 则唤醒之
		if newmHandoff.waiting {
			newmHandoff.waiting = false
			// 唤醒 templateThread ， templateThread 会启动所有的 newmHandoff.newm ，然后继续休眠
			notewakeup(&newmHandoff.wake)
		}
		unlock(&newmHandoff.lock)
		return
	}
	newm1(mp)
}

// 创建 m 的核心实现
func newm1(mp *m) {
	// 判断是否是 cgo
	if iscgo {
		var ts cgothreadstart
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		ts.g.set(mp.g0)                                // 设置 g0
		ts.tls = (*uint64)(unsafe.Pointer(&mp.tls[0])) // 设置 tls
		ts.fn = unsafe.Pointer(funcPC(mstart))         // 设置函数为 mstart
		if msanenabled {
			msanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}
		execLock.rlock()                                   // Prevent process clone. // 防止进程 clone 。
		asmcgocall(_cgo_thread_start, unsafe.Pointer(&ts)) // 启动
		execLock.runlock()
		return
	}
	execLock.rlock() // Prevent process clone. // 防止进程 clone 。
	newosproc(mp)
	execLock.runlock()
}

// startTemplateThread starts the template thread if it is not already
// running.
//
// The calling thread must itself be in a known-good state.
// 如果模板线程尚未运行，则 startTemplateThread 将启动它。调用线程本身必须处于已知良好状态。
func startTemplateThread() {
	if GOARCH == "wasm" { // no threads on wasm yet
		return
	}
	// 已经启动过，返回； 没有启动过，设置为 1 ，下次直接返回
	if !atomic.Cas(&newmHandoff.haveTemplateThread, 0, 1) {
		return
	}
	newm(templateThread, nil)
}

// templateThread is a thread in a known-good state that exists solely
// to start new threads in known-good states when the calling thread
// may not be in a good state.
//
// Many programs never need this, so templateThread is started lazily
// when we first enter a state that might lead to running on a thread
// in an unknown state.
//
// templateThread runs on an M without a P, so it must not have write
// barriers.
//
//  templateThread 是独自存在处于已知良好状态的线程，仅当调用线程可能不是良好状态时下才启动良好状态的新线程。
// 许多程序从不需要它，因此当我们第一次进入可能导致在未知状态运行线程时，templateThread会延迟启动。
//  templateThread 在没有 P 的 M 上运行，因此它必须没有写障碍。
//go:nowritebarrierrec
func templateThread() {
	lock(&sched.lock)
	sched.nmsys++ // 更新统计信息，系统 M 的数量
	checkdead()   // 检测死锁
	unlock(&sched.lock)

	// 一直循环，如果有 newmHandoff.newm 则启动，否则休眠等待
	for {
		lock(&newmHandoff.lock)
		for newmHandoff.newm != 0 {
			// 获取 newmHandoff.newm
			newm := newmHandoff.newm.ptr()
			newmHandoff.newm = 0
			unlock(&newmHandoff.lock)
			// 遍历 newm.schedlink 链表，并启动 m
			for newm != nil {
				next := newm.schedlink.ptr()
				newm.schedlink = 0
				newm1(newm)
				newm = next
			}
			lock(&newmHandoff.lock)
		}
		newmHandoff.waiting = true
		noteclear(&newmHandoff.wake)
		unlock(&newmHandoff.lock)
		// 等待 newmHandoff.wake ， newm 中会唤醒
		notesleep(&newmHandoff.wake)
	}
}

// Stops execution of the current m until new work is available.
// Returns with acquired P.
// 停止执行当前的 m ， 直到有新工作可用。 返回时， m 有关联的 p 。
func stopm() {
	_g_ := getg()

	if _g_.m.locks != 0 {
		throw("stopm holding locks")
	}
	if _g_.m.p != 0 {
		throw("stopm holding p")
	}
	if _g_.m.spinning {
		throw("stopm spinning")
	}

	// 将 m 放回到 空闲列表中，因为我们马上就要 park 了
	lock(&sched.lock)
	mput(_g_.m)
	unlock(&sched.lock)
	notesleep(&_g_.m.park)      //  park 当前的 M，在此阻塞，直到被 unpark
	noteclear(&_g_.m.park)      // 清除 unpark 的 note
	acquirep(_g_.m.nextp.ptr()) // 此时已经被 unpark，说明有任务要执行，立即 acquire P ，将 P 与当前的 M 关联
	_g_.m.nextp = 0
}

// 设置当前的 m 处于 spinning 中
func mspinning() {
	// startm's caller incremented nmspinning. Set the new M's spinning.
	//  startm 的调用者增加了 nmspinning 。 设置新的 M 为 spinning。 startm 将 mspinning 设置为启动函数了。
	getg().m.spinning = true
}

// Schedules some M to run the p (creates an M if necessary).
// If p==nil, tries to get an idle P, if no idle P's does nothing.
// May run with m.p==nil, so write barriers are not allowed.
// If spinning is set, the caller has incremented nmspinning and startm will
// either decrement nmspinning or set m.spinning in the newly started M.
// 调度一些 M 来运行 p （必要时创建 M ）。如果 p==nil ，则尝试获取一个空闲 P ，如果没有空闲 P 则不执行任何操作。可能执行 m.p==nil ，因此不允许写入障碍。
// 如果设置了 spinning ，则调用方已将 nmspinning 递增，并且 startm 将递减 nmspinning 或在新启动的 M 中设置 m.spinning 。
//go:nowritebarrierrec
func startm(_p_ *p, spinning bool) {
	lock(&sched.lock)
	if _p_ == nil {
		_p_ = pidleget()
		if _p_ == nil {
			unlock(&sched.lock)
			if spinning {
				// The caller incremented nmspinning, but there are no idle Ps,
				// so it's okay to just undo the increment and give up.
				// 调用方增加了 nmspinning ，但是没有空闲的 P ，因此可以取消增量并放弃。
				if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
					throw("startm: negative nmspinning")
				}
			}
			return
		}
	}
	// 获取空闲的 M
	mp := mget()
	unlock(&sched.lock)
	if mp == nil {
		var fn func()
		if spinning {
			// The caller incremented nmspinning, so set m.spinning in the new M.
			// 调用者增加了 nmspinning ，因此在新 M 中设置 m.spinning 。
			fn = mspinning // mspinning 函数会设置 m.spinning = true
		}
		newm(fn, _p_)
		return
	}
	//  mp 为获取空闲的 M ，这里做健康性检测
	if mp.spinning {
		throw("startm: m is spinning")
	}
	if mp.nextp != 0 {
		throw("startm: m has p")
	}
	if spinning && !runqempty(_p_) {
		throw("startm: p has runnable gs")
	}
	// The caller incremented nmspinning, so set m.spinning in the new M.
	// 调用者增加了 nmspinning ，因此在新 M 中设置 m.spinning 。
	mp.spinning = spinning
	mp.nextp.set(_p_)    // 设置下一个调用的 p
	notewakeup(&mp.park) // 并唤醒 m ， m 在 mput 的时候已经睡眠了
}

// Hands off P from syscall or locked M.
// Always runs without a P, so write barriers are not allowed.
// 从 syscall 或 locked M 让出 P ，（启动 M 执行让出的 P）。总是在没有 P 下运行，所以不允许 write barrier
//go:nowritebarrierrec
func handoffp(_p_ *p) {
	// handoffp must start an M in any situation where
	// findrunnable would return a G to run on _p_.
	// findrunnable 返回 G 来运行在 _p_ 的时候， handoffp 在任何情况下都必须启动一个 M 。

	// if it has local work, start it straight away
	// 如果本地队列或全局队列有任务(G)，直接开始
	if !runqempty(_p_) || sched.runqsize != 0 {
		startm(_p_, false)
		return
	}
	// if it has GC work, start it straight away
	// 如果还有 GC 工作，直接开始
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(_p_) {
		startm(_p_, false)
		return
	}
	// no local work, check that there are no spinning/idle M's,
	// otherwise our help is not required
	// 如果本地队列没有任务，检测是否没有 spinning/idle 的 M，否则不需要我们帮助
	// 如果没有 spinning 和 idle 的 M， 则增加 sched.nmspinning 并启动 m ，此处 startm 的第二个参数 spinning 设置为 true 。
	if atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) == 0 && atomic.Cas(&sched.nmspinning, 0, 1) { // TODO: fast atomic
		startm(_p_, true)
		return
	}
	lock(&sched.lock)
	if sched.gcwaiting != 0 {
		// 表示我们正在等待着垃圾回收 ，并递减需要
		_p_.status = _Pgcstop // 将当前的 p 的状态变为 _Pgcstop
		sched.stopwait--      // 递减等待需要停止的 P
		if sched.stopwait == 0 {
			notewakeup(&sched.stopnote) // 如果都停止了， 唤醒下。 stopTheWorld -> stopTheWorldWithSema 中在等待
		}
		unlock(&sched.lock)
		return
	}
	// 如果需要运行安全点函数，则运行之
	if _p_.runSafePointFn != 0 && atomic.Cas(&_p_.runSafePointFn, 1, 0) {
		sched.safePointFn(_p_)
		sched.safePointWait--
		if sched.safePointWait == 0 { // 如果都执行了安全点函数， 唤醒下。 gcMarkDone -> forEachP 和 gcMarkTermination -> forEachP 中在等待
			notewakeup(&sched.safePointNote)
		}
	}
	// 如果全局队列中有任务(G)， 则启动 m
	if sched.runqsize != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}
	// If this is the last running P and nobody is polling network,
	// need to wakeup another M to poll network.
	// 如果这是最后运行的 P 并且没有人正在轮询网络，则需要唤醒另一个 M 来轮询网络。
	// 所有的 P 都在空闲，sched.lastpoll == 0 (表示没有在 M 轮询网络？？)时候，唤醒一个 M 来轮询网络。 findrunnable 可能偷走了 netpoll， 然后设置的 sched.lastpoll = 0
	if sched.npidle == uint32(gomaxprocs-1) && atomic.Load64(&sched.lastpoll) != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}
	pidleput(_p_) // 将 p 放到空闲队列
	unlock(&sched.lock)
}

// Tries to add one more P to execute G's.
// Called when a G is made runnable (newproc, ready).
// 尝试将一个或多个 P 唤醒来执行 G 。当 G 被（newproc, ready）标记为 _Grunnable 时调用该函数。
func wakep() {
	// be conservative about spinning threads
	// 对 spinning 线程保守一些，必要时只增加一个，如果失败，则立即返回
	if !atomic.Cas(&sched.nmspinning, 0, 1) {
		return
	}
	// 第一个参数为 _p_  为nil， 如果没有空闲的 p ，将不启动 m ； 第二个参数 spinning 为 true ， 表示启动后 spinning 的状态。
	startm(nil, true)
}

// Stops execution of the current m that is locked to a g until the g is runnable again.
// Returns with acquired P.
// 停止当前正在执行锁住的 g 的 m 的执行，直到 g 重新变为 runnable ， 被唤醒。 返回时关联了 P
func stoplockedm() {
	_g_ := getg()

	// 检测互相锁定的一致性
	if _g_.m.lockedg == 0 || _g_.m.lockedg.ptr().lockedm.ptr() != _g_.m {
		throw("stoplockedm: inconsistent locking")
	}
	if _g_.m.p != 0 {
		// Schedule another M to run this p.
		// 调度其他 M 来运行此 P
		_p_ := releasep() // 取消关联 p 和当前 m
		handoffp(_p_)     // locked M 让出 P
	}
	incidlelocked(1) // 增加当前等待工作的被 lock 的 M 计数
	// Wait until another thread schedules lockedg again.
	// 等待直到其他线程可以再次调度 lockedg
	notesleep(&_g_.m.park)
	noteclear(&_g_.m.park)
	status := readgstatus(_g_.m.lockedg.ptr())
	// 此时已经被 unpark，g 的状态应该为 Grunnable 或 Gscanrunnable
	if status&^_Gscan != _Grunnable {
		print("runtime:stoplockedm: g is not Grunnable or Gscanrunnable\n")
		dumpgstatus(_g_)
		throw("stoplockedm: not runnable")
	}
	// 唤醒 M 时,  M 会拥有这个 P ，此处做关联
	acquirep(_g_.m.nextp.ptr())
	_g_.m.nextp = 0
}

// Schedules the locked m to run the locked gp.
// May run during STW, so write barriers are not allowed.
// 调度锁定的 m 来运行锁定的 gp 。可能在 STW 期间运行，因此不允许 write barriers 。
//go:nowritebarrierrec
func startlockedm(gp *g) {
	_g_ := getg()

	mp := gp.lockedm.ptr()
	// 检测互相锁定的一致性
	if mp == _g_.m {
		throw("startlockedm: locked to me")
	}
	if mp.nextp != 0 {
		throw("startlockedm: m has p")
	}
	// directly handoff current P to the locked m
	// 直接让出当前的 P 给锁定的 m
	incidlelocked(-1)
	_p_ := releasep() // 取消关联 p 和当前 m
	mp.nextp.set(_p_) // 唤醒 M 时, M 会拥有 nextp
	notewakeup(&mp.park)
	stopm()
}

// Stops the current m for stopTheWorld.
// Returns when the world is restarted.
// 为 stopTheWorld 停止当前的 m 。当 world 重启完成后返回。
func gcstopm() {
	_g_ := getg()

	// 检测 sched.gcwaiting
	if sched.gcwaiting == 0 {
		throw("gcstopm: not waiting for gc")
	}
	// 如果 m 在 spinning 状态
	if _g_.m.spinning {
		_g_.m.spinning = false
		// OK to just drop nmspinning here,
		// startTheWorld will unpark threads as necessary.
		// 递减 nmspinning， startTheWorld 将根据需要 unpark 线程。
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
			throw("gcstopm: negative nmspinning")
		}
	}
	_p_ := releasep()
	lock(&sched.lock)
	_p_.status = _Pgcstop // 将当前的 p 的状态变为 _Pgcstop
	sched.stopwait--      // 递减等待需要停止的 P
	if sched.stopwait == 0 {
		notewakeup(&sched.stopnote) // 如果都停止了， 唤醒下。 stopTheWorld -> stopTheWorldWithSema 中在等待
	}
	unlock(&sched.lock)
	stopm()
}

// Schedules gp to run on the current M.
// If inheritTime is true, gp inherits the remaining time in the
// current time slice. Otherwise, it starts a new time slice.
// Never returns.
//
// Write barriers are allowed because this is called immediately after
// acquiring a P in several places.
//
// 在当前 M 上调度 gp。 如果 inheritTime 为 true，则 gp 继承剩余的时间片。否则从一个新的时间片开始。 此函数永不返回。
// 该函数允许 write barrier 因为它是在 acquire P 之后的调用的。
//
//go:yeswritebarrierrec
func execute(gp *g, inheritTime bool) {
	_g_ := getg()

	// 将 g 正式切换为 _Grunning 状态
	casgstatus(gp, _Grunnable, _Grunning)
	gp.waitsince = 0                           // 清除等待时间，现在开始执行了
	gp.preempt = false                         // 关闭抢占
	gp.stackguard0 = gp.stack.lo + _StackGuard // 设置栈边界检测
	if !inheritTime {
		// 如果不继承时间片，则开始新的
		_g_.m.p.ptr().schedtick++
	}
	_g_.m.curg = gp // 设置当前循运行的 g
	gp.m = _g_.m    // 设置运行的g的 m

	// Check whether the profiler needs to be turned on or off.
	// 检查是否需要打开或关闭 cpu profiler 。
	hz := sched.profilehz
	if _g_.m.profilehz != hz {
		setThreadCPUProfiler(hz)
	}

	// trace
	if trace.enabled {
		// GoSysExit has to happen when we have a P, but before GoStart.
		// So we emit it here.
		if gp.syscallsp != 0 && gp.sysblocktraced {
			traceGoSysExit(gp.sysexitticks)
		}
		traceGoStart()
	}

	// 从gobuf恢复状态，开始执行，gogo 实现在 asm_amd64.s 中
	gogo(&gp.sched)
}

// Finds a runnable goroutine to execute.
// Tries to steal from other P's, get g from global queue, poll network.
// 寻找一个可运行的 goroutine 来执行。尝试从其他的 P 偷取、从本地或者全局队列中获取、pollnet 。
func findrunnable() (gp *g, inheritTime bool) {
	_g_ := getg()

	// The conditions here and in handoffp must agree: if
	// findrunnable would return a G to run, handoffp must start
	// an M.
	// 这里的条件与 handoffp 中的条件必须一致：如果 findrunnable 将返回 G 来运行，handoffp 必须启动 M 。

top:
	_p_ := _g_.m.p.ptr()
	if sched.gcwaiting != 0 {
		gcstopm() // 如果在 gc，则 park 当前 m，直到被 unpark 后回到 top
		goto top
	}
	if _p_.runSafePointFn != 0 {
		runSafePointFn() // 如果需要执行安全点函数，则执行
	}
	if fingwait && fingwake {
		if gp := wakefing(); gp != nil {
			ready(gp, 0, true)
		}
	}
	//  cgo 调用被终止，继续进入
	if *cgo_yield != nil {
		asmcgocall(*cgo_yield, nil)
	}

	// local runq
	// 取本地队列 local runq，如果已经拿到，立刻返回
	if gp, inheritTime := runqget(_p_); gp != nil {
		return gp, inheritTime
	}

	// global runq
	// 全局队列 global runq，如果已经拿到，立刻返回
	if sched.runqsize != 0 {
		lock(&sched.lock)
		gp := globrunqget(_p_, 0)
		unlock(&sched.lock)
		if gp != nil {
			return gp, false
		}
	}

	// Poll network.
	// This netpoll is only an optimization before we resort to stealing.
	// We can safely skip it if there are no waiters or a thread is blocked
	// in netpoll already. If there is any kind of logical race with that
	// blocked thread (e.g. it has already returned from netpoll, but does
	// not set lastpoll yet), this thread will do blocking netpoll below
	// anyway.
	// Poll 网络，优先级比从其他 P 中偷要高。在我们尝试去其他 P 偷之前，这个 netpoll 只是一个优化。如果没有 waiter 或 netpoll 中的线程已被阻塞，
	// 则可以安全地跳过它。如果有任何类型的逻辑竞争与被阻塞的线程（例如它已经从 netpoll 返回，但尚未设置 lastpoll），该线程无论如何都将阻塞 netpoll 。
	//  netpoll 已经初始化了，并且没有在等待 netpoll 的 g ，并且 sched.lastpoll != 0 ， 下面候可能将 sched.lastpoll 设置为 0 ，然后阻塞调用
	//  netpoll(true)，返回后才设置 lastpoll ， 如果 sched.lastpoll == 0 的话，则表示 netpoll 还在阻塞， 这时候是 netpool 没有就绪 g 的。
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && atomic.Load64(&sched.lastpoll) != 0 {
		// 轮询就绪的网络链接，查找 runnable G
		if list := netpoll(false); !list.empty() { // non-blocking
			gp := list.pop()                      // 获取一个
			injectglist(&list)                    // 将 netpool 中剩余的 runnable g 列表插入到调度器中
			casgstatus(gp, _Gwaiting, _Grunnable) // 设置状态为 _Grunnable
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			// 返回从 netpoll 中窃取到的 g
			return gp, false
		}
	}

	// Steal work from other P's.
	// 从其他 P 中窃取 work
	procs := uint32(gomaxprocs)
	if atomic.Load(&sched.npidle) == procs-1 {
		// Either GOMAXPROCS=1 or everybody, except for us, is idle already.
		// New work can appear from returning syscall/cgocall, network or timers.
		// Neither of that submits to local run queues, so no point in stealing.
		//  GOMAXPROCS=1 或除我们之外的每个 P 都空闲。 通过返回 syscall/cgocall，network 或 timers，可以找到新 P。
		// 两者都不会提交到本地运行队列，因此在窃取方面毫无意义。
		goto stop
	}
	// If number of spinning M's >= number of busy P's, block.
	// This is necessary to prevent excessive CPU consumption
	// when GOMAXPROCS>>1 but the program parallelism is low.
	//  如果 spinning 状态下 m 的数量 >= busy 状态下 p 的数量，直接进入阻塞。该步骤是有必要的，它用于当 GOMAXPROCS>>1 时
	// 但程序的并行机制很慢时昂贵的 CPU 消耗。
	if !_g_.m.spinning && 2*atomic.Load(&sched.nmspinning) >= procs-atomic.Load(&sched.npidle) {
		goto stop
	}
	// 如果 m 是 non-spinning 状态，切换为 spinning
	if !_g_.m.spinning {
		_g_.m.spinning = true
		atomic.Xadd(&sched.nmspinning, 1)
	}
	for i := 0; i < 4; i++ {
		// 随机窃取
		for enum := stealOrder.start(fastrand()); !enum.done(); enum.next() {
			if sched.gcwaiting != 0 {
				goto top // 已经进入了 GC? 回到 top ，park 当前的 m
			}
			// 如果偷了3次都偷不到，连 p.runnext (是当前G准备好的可运行G) 都窃取
			stealRunNextG := i > 2 // first look for ready queues with more than 1 g
			if gp := runqsteal(_p_, allp[enum.position()], stealRunNextG); gp != nil {
				// 窃取到了就返回
				return gp, false
			}
		}
	}

stop:

	// We have nothing to do. If we're in the GC mark phase, can
	// safely scan and blacken objects, and have work to do, run
	// idle-time marking rather than give up the P.
	// 没有任何 work 可做。如果我们在 GC mark 阶段，则可以安全的扫描并 blacken 对象，然后便有 work 可做，运行 idle-time 标记而非直接放弃当前的 P。
	if gcBlackenEnabled != 0 && _p_.gcBgMarkWorker != 0 && gcMarkWorkAvailable(_p_) {
		_p_.gcMarkWorkerMode = gcMarkWorkerIdleMode
		gp := _p_.gcBgMarkWorker.ptr()
		casgstatus(gp, _Gwaiting, _Grunnable)
		if trace.enabled {
			traceGoUnpark(gp, 0)
		}
		return gp, false
	}

	// wasm only:
	// If a callback returned and no other goroutine is awake,
	// then pause execution until a callback was triggered.
	// 仅限于 wasm 。如果一个回调返回后没有其他 goroutine 是苏醒的。则暂停执行直到回调被触发。
	if beforeIdle() {
		// At least one goroutine got woken.
		// 至少一个 goroutine 被唤醒
		goto top
	}

	// Before we drop our P, make a snapshot of the allp slice,
	// which can change underfoot once we no longer block
	// safe-points. We don't need to snapshot the contents because
	// everything up to cap(allp) is immutable.
	// 放弃当前的 P 之前，对 allp 做一个快照。一旦我们不再阻塞在 safe-point 时候，可以立刻在下面进行修改。
	// 我们不需要对内容进行快照，因为 cap(allp) 的所有内容都是不可变的。
	allpSnapshot := allp

	// return P and block
	// 准备归还 p，对调度器加锁
	lock(&sched.lock)
	// GC 或 运行安全点函数，则回到 top
	if sched.gcwaiting != 0 || _p_.runSafePointFn != 0 {
		unlock(&sched.lock)
		goto top
	}
	// 全局队列中又发现了 g
	if sched.runqsize != 0 {
		gp := globrunqget(_p_, 0)
		unlock(&sched.lock)
		return gp, false
	}
	// 取消关联 p 和当前 m
	if releasep() != _p_ {
		throw("findrunnable: wrong p")
	}
	// 将 p 放入 idle 链表
	pidleput(_p_)
	// 完成归还，解锁
	unlock(&sched.lock)

	// Delicate dance: thread transitions from spinning to non-spinning state,
	// potentially concurrently with submission of new goroutines. We must
	// drop nmspinning first and then check all per-P queues again (with
	// #StoreLoad memory barrier in between). If we do it the other way around,
	// another thread can submit a goroutine after we've checked all run queues
	// but before we drop nmspinning; as the result nobody will unpark a thread
	// to run the goroutine.
	// If we discover new work below, we need to restore m.spinning as a signal
	// for resetspinning to unpark a new worker thread (because there can be more
	// than one starving goroutine). However, if after discovering new work
	// we also observe no idle Ps, it is OK to just park the current thread:
	// the system is fully loaded so no spinning threads are required.
	// Also see "Worker thread parking/unparking" comment at the top of the file.
	// 这里要非常小心: 线程从 spinning 到 non-spinning 状态的转换，可能与新 goroutine 的提交同时发生。 我们必须首先降低 nmspinning，
	// 然后再次检查所有的 per-P 队列（并在期间伴随 #StoreLoad 内存屏障）。如果反过来，其他线程可以在我们检查了所有的队列、然后提交一个
	//  goroutine、再降低 nmspinning ，进而导致无法 unpark 一个线程来运行那个 goroutine 了。
	// 如果我们发现下面的新 work，我们需要恢复 m.spinning 作为重置的信号，以取消 park 新的工作线程（因为可能有多个饥饿的 goroutine）。
	// 但是，如果在发现新 work 后我们也观察到没有空闲 P，可以暂停当前线程。因为系统已满载，因此不需要 spinning 线程。
	// 请参考此文件顶部 "工作线程 parking/unparking" 的注释。
	wasSpinning := _g_.m.spinning // 记录下之前的状态是否为 spinning
	if _g_.m.spinning {
		//  spinning 到 non-spinning 状态的转换，并递减 sched.nmspinning
		_g_.m.spinning = false
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
			throw("findrunnable: negative nmspinning")
		}
	}

	// check all runqueues once again
	// 再次检查所有的 runqueue
	for _, _p_ := range allpSnapshot {
		// 如果这时本地队列不空
		if !runqempty(_p_) {
			// 锁住调度，重新获取空闲的 p
			lock(&sched.lock)
			_p_ = pidleget()
			unlock(&sched.lock)
			// 如果能获取到空闲的 p
			if _p_ != nil {
				//  p 与当前 m 关联
				acquirep(_p_)
				// 如果此前已经被切换为 spinning
				if wasSpinning {
					// 重新切换回 non-spinning
					_g_.m.spinning = true
					atomic.Xadd(&sched.nmspinning, 1)
				}
				// 这时候是有 work 的，回到顶部重新找 g
				goto top
			}
			// 没有空闲的 p，不需要重新找 g 了
			break
		}
	}

	// Check for idle-priority GC work again.
	// 再次检查 idle-priority GC work 。和上面重新找 runqueue 的逻辑类似
	//  gcMarkWorkAvailable 参数为 nil ，在这种情况下，它仅检查全局工作任务。
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(nil) {
		lock(&sched.lock)
		// 获取空闲的 p
		_p_ = pidleget()
		if _p_ != nil && _p_.gcBgMarkWorker == 0 {
			// 获取到的 p 没有 background mask worker， 重新放回空闲 p 列表
			pidleput(_p_)
			_p_ = nil
		}
		unlock(&sched.lock)
		// 如果能获取到空闲的 p
		if _p_ != nil {
			//  p 与当前 m 关联
			acquirep(_p_)
			// 如果此前已经被切换为 spinning
			if wasSpinning {
				// 重新切换回 non-spinning
				_g_.m.spinning = true
				atomic.Xadd(&sched.nmspinning, 1)
			}
			// Go back to idle GC check.
			// 这时候是有 work 的，回到顶部重新找 g
			goto stop
		}
	}

	// poll network
	//  poll 网络。和上面重新找 runqueue 的逻辑类似
	// netpoll 已经初始化了，并且没有在等待 netpoll 的 g ，并且 sched.lastpoll != 0 ，满足的话，会设置 sched.lastpoll = 0
	//  atomic.Xchg64(&sched.lastpoll, 0) 设置 sched.lastpoll = 0 ， 并返回原来的 sched.lastpoll
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && atomic.Xchg64(&sched.lastpoll, 0) != 0 {
		if _g_.m.p != 0 {
			throw("findrunnable: netpoll with p")
		}
		if _g_.m.spinning {
			throw("findrunnable: netpoll with spinning")
		}
		list := netpoll(true)                               // block until new work is available // 阻塞直到有新的 work
		atomic.Store64(&sched.lastpoll, uint64(nanotime())) // 存储上一次 netpool 时间
		//  netpoll 的 g list 不会空
		if !list.empty() {
			lock(&sched.lock)
			// 获取空闲的 p
			_p_ = pidleget()
			unlock(&sched.lock)
			// 如果能获取到空闲的 p
			if _p_ != nil {
				//  p 与当前 m 关联
				acquirep(_p_)
				gp := list.pop()                      // 获取一个
				injectglist(&list)                    // 将 netpool 中剩余的 runnable g 列表插入到调度器中
				casgstatus(gp, _Gwaiting, _Grunnable) // 设置状态为 _Grunnable
				if trace.enabled {
					traceGoUnpark(gp, 0)
				}
				// 返回从 netpoll 中窃取到的 g
				return gp, false
			}
			// 如果没有获取到 p ，将 netpool 中获取到的 runnable g 列表插入到调度器中
			injectglist(&list)
		}
	}
	// 确实找不到，park 当前的 m
	stopm()
	//  m unpark 后继续找
	goto top
}

// pollWork reports whether there is non-background work this P could
// be doing. This is a fairly lightweight check to be used for
// background work loops, like idle GC. It checks a subset of the
// conditions checked by the actual scheduler.
//  pollWork 报告当前 P 是否有非后台工作可以做。 这是用于后台工作循环（例如空闲GC）的相当轻量级的检查。 它检查实际调度程序检查的条件的子集。
// 在 mgcmark.go gcDrain 中有调用
func pollWork() bool {
	// 如果有全局任务队列，返回 true
	if sched.runqsize != 0 {
		return true
	}
	// 如果有本地任务队列，返回 true
	p := getg().m.p.ptr()
	if !runqempty(p) {
		return true
	}
	// 如果有 netpool 任务， 返回
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && sched.lastpoll != 0 {
		if list := netpoll(false); !list.empty() {
			injectglist(&list) // 获取之后放入调度
			return true
		}
	}
	// 没有 work
	return false
}

// 重新设置当前 m 为 spinning
func resetspinning() {
	_g_ := getg()
	// 当前的 M 不是 spinning
	if !_g_.m.spinning {
		throw("resetspinning: not a spinning m")
	}
	_g_.m.spinning = false
	nmspinning := atomic.Xadd(&sched.nmspinning, -1)
	if int32(nmspinning) < 0 {
		throw("findrunnable: negative nmspinning")
	}
	// M wakeup policy is deliberately somewhat conservative, so check if we
	// need to wakeup another P here. See "Worker thread parking/unparking"
	// comment at the top of the file for details.
	// M 的唤醒策略故意有些保守，因此请检查是否需要在此处唤醒另一个 P 。 有关详细信息，请参见文件顶部的 "Worker thread parking/unparking" 注释。
	// 如果没有 spinning 的 M，并且有空闲的 P，则唤醒 p 来执行
	if nmspinning == 0 && atomic.Load(&sched.npidle) > 0 {
		wakep()
	}
}

// Injects the list of runnable G's into the scheduler and clears glist.
// Can run concurrently with GC.
// 将 runnable g 列表插入到调度器中，并清空 glist 。可以与 gc 并发运行
func injectglist(glist *gList) {
	if glist.empty() {
		return
	}
	if trace.enabled {
		for gp := glist.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
			traceGoUnpark(gp, 0)
		}
	}
	lock(&sched.lock)
	var n int
	for n = 0; !glist.empty(); n++ {
		gp := glist.pop()
		casgstatus(gp, _Gwaiting, _Grunnable) // 标记为 _Grunnable 状态
		globrunqput(gp)                       //  gp 放到全局运行队列的尾部
	}
	unlock(&sched.lock)
	for ; n != 0 && sched.npidle != 0; n-- {
		startm(nil, false) // 启动对应多的 m 来执行，如果没有空闲的 p 了，也不会启动 p
	}
	*glist = gList{}
}

// One round of scheduler: find a runnable goroutine and execute it.
// Never returns.
// 调度器的一轮：找到 runnable goroutine 并进行执行且永不返回。
func schedule() {
	_g_ := getg()

	// 调度的时候， m 不能持有 locks
	if _g_.m.locks != 0 {
		throw("schedule: holding locks")
	}

	// 如果当前 M 锁定了某个 G ，那么应该交出P，进入休眠。等待某个 M 调度拿到 lockedg ，然后唤醒 lockedg 的 M
	if _g_.m.lockedg != 0 {
		stoplockedm()                       // 停止当前正在执行锁住的 g 的 m 的执行，直到 g 重新变为 runnable ， 被唤醒 。 返回时关联了 P
		execute(_g_.m.lockedg.ptr(), false) // Never returns.
	}

	// We should not schedule away from a g that is executing a cgo call,
	// since the cgo call is using the m's g0 stack.
	// 我们不应该调度一个正在执行 cgo 调用的 g ， 因为 cgo 在使用当前 m 的 g0 栈
	if _g_.m.incgo {
		throw("schedule: in cgo")
	}

top:
	// 如果当前 GC 需要（STW), 则调用 gcstopm 休眠当前的 M
	if sched.gcwaiting != 0 {
		gcstopm()
		goto top
	}
	// 如果有安全点函数， 则执行
	if _g_.m.p.ptr().runSafePointFn != 0 {
		runSafePointFn()
	}

	var gp *g
	var inheritTime bool
	// 如果启动 trace 或等待 trace reader
	if trace.enabled || trace.shutdown {
		// 有 trace reader 需要被唤醒则标记 _Grunnable
		gp = traceReader()
		if gp != nil {
			casgstatus(gp, _Gwaiting, _Grunnable)
			traceGoUnpark(gp, 0)
		}
	}
	//  如果当前 GC 正在标记阶段，允许置黑对象，则查找有没有待运行的 GC Worker, GC Worker 也是一个 G
	if gp == nil && gcBlackenEnabled != 0 {
		gp = gcController.findRunnableGCWorker(_g_.m.p.ptr())
	}
	// 说明不在 gc
	if gp == nil {
		// Check the global runnable queue once in a while to ensure fairness.
		// Otherwise two goroutines can completely occupy the local runqueue
		// by constantly respawning each other.
		// 每调度 61 次，就检查一次全局队列，保证公平性。否则两个 goroutine 可以通过不断地互相 respawn（重生） 一直占领本地的 runqueue
		if _g_.m.p.ptr().schedtick%61 == 0 && sched.runqsize > 0 {
			lock(&sched.lock)
			gp = globrunqget(_g_.m.p.ptr(), 1)
			unlock(&sched.lock)
		}
	}
	if gp == nil {
		// 从p的本地队列中获取
		gp, inheritTime = runqget(_g_.m.p.ptr())
		// 本地有 g ，则 m 不应该在 spinning 状态
		if gp != nil && _g_.m.spinning {
			throw("schedule: spinning with local work")
		}
	}
	if gp == nil {
		// 想尽办法找到可运行的 G ，找不到就不用返回了
		gp, inheritTime = findrunnable() // blocks until work is available
	}

	// 这个时候肯定取到 g 了

	// This thread is going to run a goroutine and is not spinning anymore,
	// so if it was marked as spinning we need to reset it now and potentially
	// start a new spinning M.
	// 该线程将运行 goroutine ，并且不再 spinning ，因此，如果将其标记为 spinning ，则需要立即将其重置并可能启动新的 spinning M 。
	if _g_.m.spinning {
		// 如果 m 是 spinning 状态，则：
		//  1. 从 spinning -> non-spinning
		//  2. 在没有 spinning 的 m 的情况下，再多创建一个新的 spinning m
		resetspinning()
	}

	// 如果禁用用户地 G 调度，并且 gp 不能够调度， 表示 gp 是用户 G ，不是系统 G
	if sched.disable.user && !schedEnabled(gp) {
		// Scheduling of this goroutine is disabled. Put it on
		// the list of pending runnable goroutines for when we
		// re-enable user scheduling and look again.
		// 禁用此 goroutine 的调度。 当我们重新启用用户调度并再次查看时，将其放在待处理的可运行 goroutine 列表中。
		lock(&sched.lock)
		// 锁住后重新检测
		if schedEnabled(gp) {
			// Something re-enabled scheduling while we
			// were acquiring the lock.
			// 当我们之前正在获取锁的时候，可能有什么重新启动了调度， 也就是锁住之前，可能哪里重新启动了用户 g 调度。
			unlock(&sched.lock)
		} else {
			// 加入到禁用掉的等待的可运行的 G 队尾
			sched.disable.runnable.pushBack(gp)
			sched.disable.n++
			unlock(&sched.lock)
			goto top
		}
	}

	// 如果 gp 锁定了 m
	if gp.lockedm != 0 {
		// Hands off own p to the locked m,
		// then blocks waiting for a new p.
		//  让出 gp 给其锁定的 m ，然后阻塞等待新的 p
		startlockedm(gp) // 调度锁定的 m 来运行锁定的 gp
		goto top
	}

	// 开始执行
	execute(gp, inheritTime)
}

// dropg removes the association between m and the current goroutine m->curg (gp for short).
// Typically a caller sets gp's status away from Grunning and then
// immediately calls dropg to finish the job. The caller is also responsible
// for arranging that gp will be restarted using ready at an
// appropriate time. After calling dropg and arranging for gp to be
// readied later, the caller can do other work but eventually should
// call schedule to restart the scheduling of goroutines on this m.
//  dropg 移除 m 与当前 goroutine m->curg（简称 gp ）之间的关联。通常，调用者将 gp 的状态设置为非 _Grunning 后立即调用 dropg 完成工作。
// 调用者也有责任在 gp 将使用 ready 重新启动时进行相关安排。在调用 dropg 并安排 gp ready 好后，调用者可以做其他工作，但最终应该调用
//  schedule 来重新启动此 m 上的 goroutine 的调度。
func dropg() {
	_g_ := getg()

	setMNoWB(&_g_.m.curg.m, nil)
	setGNoWB(&_g_.m.curg, nil)
}

//  goparkunlock 中调用 gopark 的 unlockf 参数
func parkunlock_c(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	return true
}

// park continuation on g0.
//  park(gopark) 继续在 g0 上的执行
func park_m(gp *g) {
	_g_ := getg()

	if trace.enabled {
		traceGoPark(_g_.m.waittraceev, _g_.m.waittraceskip)
	}

	// 设置为 _Gwaiting
	casgstatus(gp, _Grunning, _Gwaiting)
	// 移除 m 与当前 goroutine m->curg 之间的关联
	dropg()

	// 如果需要等待解锁
	if _g_.m.waitunlockf != nil {
		fn := *(*func(*g, unsafe.Pointer) bool)(unsafe.Pointer(&_g_.m.waitunlockf))
		ok := fn(gp, _g_.m.waitlock)
		_g_.m.waitunlockf = nil
		_g_.m.waitlock = nil
		// 解锁失败后继续执行
		if !ok {
			if trace.enabled {
				traceGoUnpark(gp, 2)
			}
			casgstatus(gp, _Gwaiting, _Grunnable)
			execute(gp, true) // Schedule it back, never returns. //
		}
	}
	// 继续调度
	schedule()
}

// gosched 实现， 将 gp 加入全剧队列，然后调用调度器。让出当前的 P 然后执行其他的 g 。
func goschedImpl(gp *g) {
	status := readgstatus(gp)
	// 此时状态应该为 _Grunning 或 _Gscanrunning
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}
	// 状态设置为 _Grunnable
	casgstatus(gp, _Grunning, _Grunnable)
	// 移除 m 与当前 goroutine m->curg 之间的关联
	dropg()
	lock(&sched.lock)
	globrunqput(gp) // 放回到全局任务队列
	unlock(&sched.lock)

	// 继续调度
	schedule()
}

// Gosched continuation on g0.
//  gosched 继续在 g0 上执行。
func gosched_m(gp *g) {
	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

// goschedguarded is a forbidden-states-avoided version of gosched_m
//  goschedguarded_m 是 gosched_m 避免禁止状态的版本
func goschedguarded_m(gp *g) {
	// 还持有锁？ 正在分配内存？ 抢占结束了？ 关联的 P 不是 _Prunning 状态？
	//  newstack 对抢占中的 g 中也有如下判断
	if gp.m.locks != 0 || gp.m.mallocing != 0 || gp.m.preemptoff != "" || gp.m.p.ptr().status != _Prunning {
		gogo(&gp.sched) // never return // 恢复调度，永不返回
	}

	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

// 抢占调用，在 newstack 中抢占时调用
func gopreempt_m(gp *g) {
	if trace.enabled {
		traceGoPreempt()
	}
	goschedImpl(gp)
}

// Finishes execution of the current goroutine.
// 完成当前 goroutine 的执行
func goexit1() {
	if raceenabled {
		racegoend()
	}
	if trace.enabled {
		traceGoEnd()
	}
	// 开始收尾工作
	mcall(goexit0)
}

// goexit continuation on g0.
//  goexit 继续在 g0 上执行
func goexit0(gp *g) {
	_g_ := getg()

	// 切换当前的 g 为 _Gdead
	casgstatus(gp, _Grunning, _Gdead)
	// 如果是系统 g ， 更新统计信息
	if isSystemGoroutine(gp, false) {
		atomic.Xadd(&sched.ngsys, -1)
	}
	// 清理
	gp.m = nil
	locked := gp.lockedm != 0
	gp.lockedm = 0
	_g_.m.lockedg = 0
	gp.paniconfault = false
	gp._defer = nil // should be true already but just in case. // 应该已经为 true，但以防万一
	gp._panic = nil // non-nil for Goexit during panic. points at stack-allocated data. //  Goexit 中 panic 则不为 nil， 指向栈分配的数据
	gp.writebuf = nil
	gp.waitreason = 0
	gp.param = nil
	gp.labels = nil
	gp.timer = nil

	if gcBlackenEnabled != 0 && gp.gcAssistBytes > 0 {
		// Flush assist credit to the global pool. This gives
		// better information to pacing if the application is
		// rapidly creating an exiting goroutines.
		// 刷新 assist credit 到全局池。如果政协在快速创建已存在的 goroutine，这可以为 pacing 提供更好的信息。
		scanCredit := int64(gcController.assistWorkPerByte * float64(gp.gcAssistBytes))
		atomic.Xaddint64(&gcController.bgScanCredit, scanCredit)
		gp.gcAssistBytes = 0
	}

	// Note that gp's stack scan is now "valid" because it has no
	// stack.
	// 请注意， gp 的栈扫描现在 “有效” ，因为它没有栈。
	gp.gcscanvalid = true
	// 移除 m 与当前 goroutine m->curg 之间的关联
	dropg()

	if GOARCH == "wasm" { // no threads yet on wasm //  wasm 目前还没有线程支持
		gfput(_g_.m.p.ptr(), gp) // 将 g 放进 gfree 链表中等待复用
		schedule()               // never returns
	}

	//  lockOSThread/unlockOSThread 调用不匹配
	if _g_.m.lockedInt != 0 {
		print("invalid m->lockedInt = ", _g_.m.lockedInt, "\n")
		throw("internal lockOSThread error")
	}
	gfput(_g_.m.p.ptr(), gp) // 将 g 放进 gfree 链表中等待复用
	if locked {
		// The goroutine may have locked this thread because
		// it put it in an unusual kernel state. Kill it
		// rather than returning it to the thread pool.
		// 该 goroutine 可能在当前线程上锁住，因为它可能导致了不正常的内核状态。这时候 kill 该线程，而非将 m 放回到线程池。

		// Return to mstart, which will release the P and exit
		// the thread.
		// 此举会返回到 mstart，从而释放当前的 P 并退出该线程
		if GOOS != "plan9" { // See golang.org/issue/22227.
			//  mstart1 调用 save 保存的，这里恢复，则结束线程
			gogo(&_g_.m.g0.sched)
		} else {
			// Clear lockedExt on plan9 since we may end up re-using
			// this thread.
			// 因为我们可能已重用此线程结束，在 plan9 上清除 lockedExt
			_g_.m.lockedExt = 0
		}
	}
	// 再次进行调度
	schedule()
}

// save updates getg().sched to refer to pc and sp so that a following
// gogo will restore pc and sp.
//
// save must not have write barriers because invoking a write barrier
// can clobber getg().sched.
// save 更新了 getg().sched 的 pc 和 sp 的指向，并允许 gogo 能够恢复到 pc 和 sp 。 save 不允许 write barrier， 因为会破坏 getg().sched 。
//
//go:nosplit
//go:nowritebarrierrec
func save(pc, sp uintptr) {
	_g_ := getg()

	_g_.sched.pc = pc
	_g_.sched.sp = sp
	_g_.sched.lr = 0
	_g_.sched.ret = 0
	_g_.sched.g = guintptr(unsafe.Pointer(_g_))
	// We need to ensure ctxt is zero, but can't have a write
	// barrier here. However, it should always already be zero.
	// Assert that.
	// 我们必须确保 ctxt 为零，但这里不允许 write barrier。 所以这里只是做一个断言。
	if _g_.sched.ctxt != nil {
		badctxt()
	}
}

// The goroutine g is about to enter a system call.
// Record that it's not using the cpu anymore.
// This is called only from the go syscall library and cgocall,
// not from the low-level system calls used by the runtime.
//  goroutine g 即将进入系统调用。记录它不再使用 cpu 了。此函数只能从 go syscall 库和 cgocall 调用，而不是从运行时使用的低级系统调用中调用。
//
// Entersyscall cannot split the stack: the gosave must
// make g->sched refer to the caller's stack segment, because
// entersyscall is going to return immediately after.
//  Entersyscall不允许分段栈：gosave 必须使得 g.sched 指的是调用者的栈段，因为 enteryscall 将在之后立即返回。
//
// Nothing entersyscall calls can split the stack either.
// We cannot safely move the stack during an active call to syscall,
// because we do not know which of the uintptr arguments are
// really pointers (back into the stack).
// In practice, this means that we make the fast path run through
// entersyscall doing no-split things, and the slow path has to use systemstack
// to run bigger things on the system stack.
// 没有任何 entersyscall 调用可以对栈分段。在对 syscall 的活动调用期间，我们无法安全地移动栈，因为我们不知道哪个 uintptr 参数确实是指针（返回栈）。
// 在实践中，这意味着我们使 fast path 通过 entersyscall 执行无分段事务，而 slow path 必须使用 systemstack 在系统堆栈上运行更大的东西。
//
// reentersyscall is the entry point used by cgo callbacks, where explicitly
// saved SP and PC are restored. This is needed when exitsyscall will be called
// from a function further up in the call stack than the parent, as g->syscallsp
// must always point to a valid stack frame. entersyscall below is the normal
// entry point for syscalls, which obtains the SP and PC from the caller.
//  reentersyscall 是 cgo 回调使用的入口点， 其中显式保存的 SP 和 PC 已恢复。当从调用栈中的函数调用 exitsyscall 而不是父函数时，需要这样做，因为
//  g.syscallsp 必须始终指向有效的堆栈帧。下面的 entersyscall 是系统调用的正常入口点，它从调用者获取 SP 和 PC。
//
// Syscall tracing:
// At the start of a syscall we emit traceGoSysCall to capture the stack trace.
// If the syscall does not block, that is it, we do not emit any other events.
// If the syscall blocks (that is, P is retaken), retaker emits traceGoSysBlock;
// when syscall returns we emit traceGoSysExit and when the goroutine starts running
// (potentially instantly, if exitsyscallfast returns true) we emit traceGoStart.
// To ensure that traceGoSysExit is emitted strictly after traceGoSysBlock,
// we remember current value of syscalltick in m (_g_.m.syscalltick = _g_.m.p.ptr().syscalltick),
// whoever emits traceGoSysBlock increments p.syscalltick afterwards;
// and we wait for the increment before emitting traceGoSysExit.
// Note that the increment is done even if tracing is not enabled,
// because tracing can be enabled in the middle of syscall. We don't want the wait to hang.
//
//go:nosplit
func reentersyscall(pc, sp uintptr) {
	_g_ := getg()

	// Disable preemption because during this function g is in Gsyscall status,
	// but can have inconsistent g->sched, do not let GC observe it.
	// 禁用抢占，因为在此行数期间 g 处于 Gsyscall 状态，但 g->sched 可能不一致，请勿让 GC 观察它。
	_g_.m.locks++

	// Entersyscall must not call any function that might split/grow the stack.
	// (See details in comment above.)
	// Catch calls that might, by replacing the stack guard with something that
	// will trip any stack check and leaving a flag to tell newstack to die.
	//  Entersyscall不得调用任何可能拆分/增加栈的函数。（请参见上面的注释中的详细信息。）
	// 捕获可能发生此调用，方法是将 stack guard 替换为会使任何堆栈检查失败的内容，并留下一个标志来通知 newstack 终止。
	_g_.stackguard0 = stackPreempt // 设置抢占
	_g_.throwsplit = true          // 必须不能栈分段， 在 newstack 中，如果发现 throwsplit 是 true ，会直接 crash 。

	// Leave SP around for GC and traceback.
	// 为 GC 和 traceback 保留 SP 。在 syscall 之后会依据这些数据恢复现场。
	save(pc, sp)
	_g_.syscallsp = sp
	_g_.syscallpc = pc
	casgstatus(_g_, _Grunning, _Gsyscall) // 设置 _Gsyscall 状态
	// 栈范围检测
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		systemstack(func() {
			print("entersyscall inconsistent ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscall")
		})
	}

	if trace.enabled {
		systemstack(traceGoSysCall)
		// systemstack itself clobbers g.sched.{pc,sp} and we might
		// need them later when the G is genuinely blocked in a
		// syscall
		//  systemstack 本身会破坏 g.sched.{pc,sp} ，稍后当 G 在系统调用中被真正阻止时，我们可能会需要它们
		save(pc, sp) // 重新保存
	}

	// 如果等待 sysmon
	if atomic.Load(&sched.sysmonwait) != 0 {
		systemstack(entersyscall_sysmon) //  entersyscall_sysmon 会唤醒 sysmon
		save(pc, sp)
	}

	// 安全点函数
	if _g_.m.p.ptr().runSafePointFn != 0 {
		// runSafePointFn may stack split if run on this stack
		// 如果在此栈上运行，则runSafePointFn可能会拆分栈
		systemstack(runSafePointFn)
		save(pc, sp)
	}

	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick
	_g_.sysblocktraced = true
	_g_.m.mcache = nil
	pp := _g_.m.p.ptr()
	pp.m = 0
	_g_.m.oldp.set(pp)
	_g_.m.p = 0
	// 设置 p 状态为 _Psyscall
	atomic.Store(&pp.status, _Psyscall)
	// 等待 gc
	if sched.gcwaiting != 0 {
		systemstack(entersyscall_gcwait)
		save(pc, sp)
	}

	_g_.m.locks--
}

// Standard syscall entry used by the go syscall library and normal cgo calls.
// 标准系统调用入口，用于 go syscall 库以及普通的 cgo 调用
//go:nosplit
func entersyscall() {
	reentersyscall(getcallerpc(), getcallersp())
}

//  entersyscall 中时唤醒 sysmon
func entersyscall_sysmon() {
	lock(&sched.lock)
	if atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
}

//  entersyscall 中时唤醒 stopTheWorldWithSema 处理抢占 p
func entersyscall_gcwait() {
	_g_ := getg()
	_p_ := _g_.m.oldp.ptr()

	lock(&sched.lock)
	// 如果需要等待 p 停止，并且处于系统调用中
	if sched.stopwait > 0 && atomic.Cas(&_p_.status, _Psyscall, _Pgcstop) {
		if trace.enabled {
			traceGoSysBlock(_p_)
			traceProcStop(_p_)
		}
		_p_.syscalltick++
		if sched.stopwait--; sched.stopwait == 0 { // 如果都停止了， 唤醒下。 stopTheWorld -> stopTheWorldWithSema 中在等待
			notewakeup(&sched.stopnote)
		}
	}
	unlock(&sched.lock)
}

// The same as entersyscall(), but with a hint that the syscall is blocking.
// 与 entersyscall() 相同，但带有系统调用被阻止的提示。
//go:nosplit
func entersyscallblock() {
	_g_ := getg()

	_g_.m.locks++ // see comment in entersyscall
	_g_.throwsplit = true
	_g_.stackguard0 = stackPreempt // see comment in entersyscall
	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick
	_g_.sysblocktraced = true
	_g_.m.p.ptr().syscalltick++

	// Leave SP around for GC and traceback.
	pc := getcallerpc()
	sp := getcallersp()
	save(pc, sp)
	_g_.syscallsp = _g_.sched.sp
	_g_.syscallpc = _g_.sched.pc
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		sp1 := sp
		sp2 := _g_.sched.sp
		sp3 := _g_.syscallsp
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp1), " ", hex(sp2), " ", hex(sp3), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}
	casgstatus(_g_, _Grunning, _Gsyscall)
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp), " ", hex(_g_.sched.sp), " ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}

	systemstack(entersyscallblock_handoff)

	// Resave for traceback during blocked call.
	// 在阻塞调用中为 traceback 保存
	save(getcallerpc(), getcallersp())

	_g_.m.locks--
}

//  entersyscallblock 中让出 p
func entersyscallblock_handoff() {
	if trace.enabled {
		traceGoSysCall()
		traceGoSysBlock(getg().m.p.ptr())
	}
	handoffp(releasep())
}

// The goroutine g exited its system call.
// Arrange for it to run on a cpu again.
// This is called only from the go syscall library, not
// from the low-level system calls used by the runtime.
//
// Write barriers are not allowed because our P may have been stolen.
//  goroutine g 退出其系统调用。为其再次安排一个 cpu 来运行。这个调用只从 go syscall 库调用，不能从运行时其他低级系统调用使用。
//  write barrier 不被允许，因为我们的 P 可能已经被偷走了。
//
//go:nosplit
//go:nowritebarrierrec
func exitsyscall() {
	_g_ := getg()

	_g_.m.locks++ // see comment in entersyscall
	//  sp 溢出了
	if getcallersp() > _g_.syscallsp {
		throw("exitsyscall: syscall frame is no longer valid")
	}

	_g_.waitsince = 0
	oldp := _g_.m.oldp.ptr()
	_g_.m.oldp = 0
	// 快速退出系统调用
	if exitsyscallfast(oldp) {
		if _g_.m.mcache == nil {
			throw("lost mcache")
		}
		if trace.enabled {
			if oldp != _g_.m.p.ptr() || _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
				systemstack(traceGoStart)
			}
		}
		// There's a cpu for us, so we can run.
		// 有 cpu 来运行
		_g_.m.p.ptr().syscalltick++
		// We need to cas the status and scan before resuming...
		// 在恢复之前，设置 g 为 _Grunning 状态并扫描
		casgstatus(_g_, _Gsyscall, _Grunning)

		// Garbage collector isn't running (since we are),
		// so okay to clear syscallsp.
		// 垃圾收集器未运行（因为我们在运行），因此可以清除 syscallsp 。
		_g_.syscallsp = 0
		_g_.m.locks--
		if _g_.preempt {
			// restore the preemption request in case we've cleared it in newstack // 恢复抢占请求，以防我们在新堆栈中将其清除
			_g_.stackguard0 = stackPreempt
		} else {
			// otherwise restore the real _StackGuard, we've spoiled it in entersyscall/entersyscallblock
			// 否则恢复在 entersyscall/entersyscallblock 中破坏掉的正常的 _StackGuard
			_g_.stackguard0 = _g_.stack.lo + _StackGuard
		}
		_g_.throwsplit = false

		if sched.disable.user && !schedEnabled(_g_) {
			// Scheduling of this goroutine is disabled.
			// 禁用此 goroutine 的调度。
			Gosched()
		}

		return
	}

	_g_.sysexitticks = 0
	if trace.enabled {
		// Wait till traceGoSysBlock event is emitted.
		// This ensures consistency of the trace (the goroutine is started after it is blocked).
		for oldp != nil && oldp.syscalltick == _g_.m.syscalltick {
			osyield()
		}
		// We can't trace syscall exit right now because we don't have a P.
		// Tracing code can invoke write barriers that cannot run without a P.
		// So instead we remember the syscall exit time and emit the event
		// in execute when we have a P.
		_g_.sysexitticks = cputicks()
	}

	_g_.m.locks--

	// Call the scheduler.
	// 调用调度器
	mcall(exitsyscall0)

	// 检测 mcache
	if _g_.m.mcache == nil {
		throw("lost mcache")
	}

	// Scheduler returned, so we're allowed to run now.
	// Delete the syscallsp information that we left for
	// the garbage collector during the system call.
	// Must wait until now because until gosched returns
	// we don't know for sure that the garbage collector
	// is not running.
	//  Scheduler 返回了，所以我们现在就可以运行。删除在系统调用期间留给垃圾收集器的 syscallsp 信息。必须等到现在，因为在 gosched 返回之前，
	// 我们不确定垃圾收集器是否在运行。
	_g_.syscallsp = 0
	_g_.m.p.ptr().syscalltick++
	_g_.throwsplit = false
}

// 快速退出系统调用
//go:nosplit
func exitsyscallfast(oldp *p) bool {
	_g_ := getg()

	// Freezetheworld sets stopwait but does not retake P's.
	//  stopwait 设置为 Freezetheworld，不重新获取 P 。
	if sched.stopwait == freezeStopWait {
		return false
	}

	// Try to re-acquire the last P.
	// 尝试重新获取上个 P (oldp) ， 将状态转为 _Pidle
	if oldp != nil && oldp.status == _Psyscall && atomic.Cas(&oldp.status, _Psyscall, _Pidle) {
		// There's a cpu for us, so we can run.
		// 有 cpu 来运行
		wirep(oldp)
		exitsyscallfast_reacquired()
		return true
	}

	// Try to get any other idle P.
	// 尝试获取其他空闲的 p
	if sched.pidle != 0 {
		var ok bool
		systemstack(func() {
			ok = exitsyscallfast_pidle()
			if ok && trace.enabled {
				if oldp != nil {
					// Wait till traceGoSysBlock event is emitted.
					// This ensures consistency of the trace (the goroutine is started after it is blocked).
					// 等待直到发出 traceGoSysBlock 事件。 这样可以确保 trace 的一致性（ goroutine 会在阻塞后启动 ）。
					for oldp.syscalltick == _g_.m.syscalltick {
						osyield() // 让出 cpu
					}
				}
				// 表示当前系统调用已完成。
				traceGoSysExit(0)
			}
		})
		if ok {
			return true
		}
	}
	return false
}

// exitsyscallfast_reacquired is the exitsyscall path on which this G
// has successfully reacquired the P it was running on before the
// syscall.
//  exitsyscallfast_reacquired 是 exitsyscall 中，当前 G 已成功重新获取了它 在syscall 之前运行的 P 。
//  exitsyscallfast 中获取到之前的 p ，后做的一些 trace
//
//go:nosplit
func exitsyscallfast_reacquired() {
	_g_ := getg()
	if _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
		if trace.enabled {
			// The p was retaken and then enter into syscall again (since _g_.m.syscalltick has changed).
			// traceGoSysBlock for this syscall was already emitted,
			// but here we effectively retake the p from the new syscall running on the same p.
			// 重新取回p，然后再次进入 syscall （因为 _g_.m.syscalltick 已更改）。 此系统调用的 traceGoSysBlock 已经发出，
			// 但是在这里，我们有效地从同一 p 上运行的新系统调用中重新获取p。
			systemstack(func() {
				// Denote blocking of the new syscall.
				// 表示阻止新的系统调用。
				traceGoSysBlock(_g_.m.p.ptr())
				// Denote completion of the current syscall.
				// 表示当前系统调用已完成。
				traceGoSysExit(0)
			})
		}
		_g_.m.p.ptr().syscalltick++
	}
}

//  exitsyscallfast 获取空闲的 p ， 如果获取到了， 则与当前的 m 关联
func exitsyscallfast_pidle() bool {
	lock(&sched.lock)
	_p_ := pidleget()
	// 如果获取的 p 不为空， 并且在等待 sysmon, 则唤醒 sysmon
	if _p_ != nil && atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	// 如果获取到了 p ， 则与当前的 m 关联
	if _p_ != nil {
		acquirep(_p_)
		return true
	}
	return false
}

// exitsyscall slow path on g0.
// Failed to acquire P, enqueue gp as runnable.
// 在 exitsyscallfast 中失败了，没办法，慢慢来。 关联当前 m 和 p 失败， 把 gp 放入全局任务队列。
//
//go:nowritebarrierrec
func exitsyscall0(gp *g) {
	_g_ := getg()

	casgstatus(gp, _Gsyscall, _Grunnable) // 设置状态为 _Grunnable
	// 移除 m 与当前 goroutine m->curg 之间的关联
	dropg()
	lock(&sched.lock)
	var _p_ *p
	// 如果允许调度 g ， 则获取空闲的 p
	if schedEnabled(_g_) {
		_p_ = pidleget()
	}
	if _p_ == nil {
		// 没有获取到 p ， 将 g 放入全局队列
		globrunqput(gp)
	} else if atomic.Load(&sched.sysmonwait) != 0 {
		// 如果获取的 p 不为空， 并且在等待 sysmon, 则唤醒 sysmon
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if _p_ != nil {
		// 如果现在还有 p，那就用这个 p 执行
		acquirep(_p_)
		execute(gp, false) // Never returns.
	}
	if _g_.m.lockedg != 0 {
		// Wait until another thread schedules gp and so m again.
		// 停止当前正在执行锁住的 g 的 m 的执行，等待被唤醒。
		stoplockedm()
		execute(gp, false) // Never returns.
	}
	// 停止 m， 等待被唤醒
	stopm()
	schedule() // Never returns.
}

//  在 fork 之前执行
func beforefork() {
	gp := getg().m.curg

	// Block signals during a fork, so that the child does not run
	// a signal handler before exec if a signal is sent to the process
	// group. See issue #18600.
	// 在 fork 期间阻塞信号，则如果信号已经发送到进程组了，则子进程不会在 exec 之前运行信号处理程序。
	gp.m.locks++
	msigsave(gp.m) // 保存 signal mask 到 gp.m..sigmask
	sigblock()

	// This function is called before fork in syscall package.
	// Code between fork and exec must not allocate memory nor even try to grow stack.
	// Here we spoil g->_StackGuard to reliably detect any attempts to grow stack.
	// runtime_AfterFork will undo this in parent process, but not in child.
	// 这个函数在 fork 之前从 syscall 包中调用。 fork 和 exec 之间的代码不得分配内存，甚至不能尝试增加堆栈。 在这里，
	// 我们破坏 g->_StackGuard 来可靠地检测任何尝试增加堆栈的尝试。
	//  runtime_AfterFork 将在父进程中撤消此操作，但在子进程中不撤消。
	//  在 newstack 中，如果发现 thisg.m.morebuf.g.ptr().stackguard0 == stackFork ，会直接 crash 。
	gp.stackguard0 = stackFork
}

// Called from syscall package before fork.
// 在 fork 之前从 syscall 包中调用。 导出为 runtime_BeforeFork 函数。src/syscall/exec_linux.go 中 forkAndExecInChild1 函数会调用。
//go:linkname syscall_runtime_BeforeFork syscall.runtime_BeforeFork
//go:nosplit
func syscall_runtime_BeforeFork() {
	systemstack(beforefork)
}

//  在 fork 之后执行，撤销 beforefork 中的修改
func afterfork() {
	gp := getg().m.curg

	// See the comments in beforefork.
	// 还原 beforefork 中栈边界的修改
	gp.stackguard0 = gp.stack.lo + _StackGuard

	msigrestore(gp.m.sigmask) // 从 gp.m.sigmask 中恢复 signal mask

	gp.m.locks--
}

// Called from syscall package after fork in parent.
// 在 fork 之后在进程中从 syscall 包中调用。 导出为 runtime_AfterFork 函数。src/syscall/exec_linux.go 中 forkAndExecInChild 函数会调用。
//go:linkname syscall_runtime_AfterFork syscall.runtime_AfterFork
//go:nosplit
func syscall_runtime_AfterFork() {
	systemstack(afterfork)
}

// inForkedChild is true while manipulating signals in the child process.
// This is used to avoid calling libc functions in case we are using vfork.
// 在子进程中处理信号时， inForkedChild 为 true 。 这是为了避免在使用 vfork 时调用 libc 函数。
var inForkedChild bool

// Called from syscall package after fork in child.
// It resets non-sigignored signals to the default handler, and
// restores the signal mask in preparation for the exec.
//
// Because this might be called during a vfork, and therefore may be
// temporarily sharing address space with the parent process, this must
// not change any global variables or calling into C code that may do so.
// 在 syscall 包 fork 之后从子进程中调用。它将不可忽视的信号重置为默认处理程序，并恢复信号掩码以准备 exec。
// 因为这可能在 vfork 期间调用，因此可能暂时与父进程共享地址空间，所以这不能更改任何全局变量或调用可能执行此操作的 C 代码。
// 导出为 runtime_AfterForkInChild 函数。src/syscall/exec_linux.go 中 forkAndExecInChild1 函数会调用。
//
//go:linkname syscall_runtime_AfterForkInChild syscall.runtime_AfterForkInChild
//go:nosplit
//go:nowritebarrierrec
func syscall_runtime_AfterForkInChild() {
	// It's OK to change the global variable inForkedChild here
	// because we are going to change it back. There is no race here,
	// because if we are sharing address space with the parent process,
	// then the parent process can not be running concurrently.
	// 可以在此处更改全局变量 inForkedChild ，因为我们将其改回来。这里没有 race，因为如果我们要与父进程共享地址空间，那么父进程将不能并行运行。
	inForkedChild = true

	// 将不可忽视的信号重置为默认处理程序
	clearSignalHandlers()

	// When we are the child we are the only thread running,
	// so we know that nothing else has changed gp.m.sigmask.
	// 当我们是子进程时，我们是唯一运行的线程，因此我们知道 gp.m.sigmask 没有其他改变。
	msigrestore(getg().m.sigmask)

	inForkedChild = false // 恢复 inForkedChild
}

// Called from syscall package before Exec.
// 从 syscall.Exec 开始前调用。导出为 runtime_BeforeExec 函数。src/syscall/exec_unix.go 中 Exec 函数会调用。
//go:linkname syscall_runtime_BeforeExec syscall.runtime_BeforeExec
func syscall_runtime_BeforeExec() {
	// Prevent thread creation during exec.
	// 在exec期间阻止创建线程。
	execLock.lock()
}

// Called from syscall package after Exec.
// 从 syscall.Exec 结束后调用。导出为 runtime_AfterExec 函数。src/syscall/exec_unix.go 中 Exec 函数会调用。
//go:linkname syscall_runtime_AfterExec syscall.runtime_AfterExec
func syscall_runtime_AfterExec() {
	// 解锁
	execLock.unlock()
}

// Allocate a new g, with a stack big enough for stacksize bytes.
// 分配一个新的 g ，该堆栈的大小足以容纳 stacksize 字节。
func malg(stacksize int32) *g {
	newg := new(g)
	// 分配栈，有些情况下不需要
	if stacksize >= 0 {
		// 将 stacksize 舍入为 2 的指数
		stacksize = round2(_StackSystem + stacksize)
		systemstack(func() {
			newg.stack = stackalloc(uint32(stacksize))
		})
		newg.stackguard0 = newg.stack.lo + _StackGuard
		newg.stackguard1 = ^uintptr(0)
	}
	return newg
}

// Create a new g running fn with siz bytes of arguments.
// Put it on the queue of g's waiting to run.
// The compiler turns a go statement into a call to this.
// Cannot split the stack because it assumes that the arguments
// are available sequentially after &fn; they would not be
// copied if a stack split occurred.
//go:nosplit
// 创建 G 运行 fn , 参数大小为 siz 。把 G 放到等待队列。编译器会将 go 语句转化为该调用。
// 这时不能将栈进行分段，因为它假设了参数在 &fn 之后顺序有效；如果 stack 进行了分段则他们不无法被拷贝。
func newproc(siz int32, fn *funcval) {
	//  add 是一个指针运算，跳过函数指针，把栈上的参数起始地址找到，见 runtime2.go 中的 funcval 类型
	argp := add(unsafe.Pointer(&fn), sys.PtrSize)
	gp := getg()
	// 获取调用方 PC 寄存器值
	pc := getcallerpc()
	// 用 g0 系统栈创建 goroutine 对象。传递的参数包括 fn 函数入口地址, argp 参数起始地址, siz 参数长度, gp(g0)，调用方 pc(goroutine)
	systemstack(func() {
		newproc1(fn, (*uint8)(argp), siz, gp, pc)
	})
}

// Create a new g running fn with narg bytes of arguments starting
// at argp. callerpc is the address of the go statement that created
// this. The new g is put on the queue of g's waiting to run.
// 创建一个运行 fn 的新 g，具有 narg 字节大小的参数，从 argp 开始。callerps 是 go 语句的起始地址。新创建的 g 会被放入 g 的队列中等待运行。
func newproc1(fn *funcval, argp *uint8, narg int32, callergp *g, callerpc uintptr) {
	// 因为是在系统栈运行所以此时的 g 为 g0
	_g_ := getg()

	// 判断下 func 的实现是否为空
	if fn == nil {
		_g_.m.throwing = -1 // do not dump full stacks
		throw("go of nil func value")
	}
	// 设置 g 对应的 m 的 locks++, 禁止抢占，因为它可以在一个局部变量中保存 p
	_g_.m.locks++ // disable preemption because it can be holding p in a local var
	siz := narg
	siz = (siz + 7) &^ 7 // 字节对齐

	// We could allocate a larger initial stack if necessary.
	// Not worth it: this is almost always an error.
	// 4*sizeof(uintreg): extra space added below
	// sizeof(uintreg): caller's LR (arm) or return address (x86, in gostartcall).
	// 必要时，可以分配并初始化一个更大的栈。
	// 不值得：这几乎总是一个错误。
	// 4*sizeof(uintreg): 在下方增加的额外空间
	// sizeof(uintreg): 调用者 LR (arm) 返回的地址 (x86 在 gostartcall 中)
	if siz >= _StackMin-4*sys.RegSize-sys.RegSize {
		throw("newproc: function arguments too large for new goroutine")
	}
	// 获取 p
	_p_ := _g_.m.p.ptr()
	// 从 g 空闲列表中，根据 p 获得一个新的 g
	newg := gfget(_p_)

	// 初始化阶段，gfget 是不可能找到 g 的，也可能运行中本来就已经耗尽了
	if newg == nil {
		// 创建一个拥有 _StackMin (2kb) 大小的栈的 g
		newg = malg(_StackMin)
		// 将新创建的 g 从 _Gidle 更新为 _Gdead 状态
		casgstatus(newg, _Gidle, _Gdead)
		// 将 Gdead 状态的 g 添加到 allg，这样 GC 不会扫描未初始化的栈
		allgadd(newg) // publishes with a g->status of Gdead so GC scanner doesn't look at uninitialized stack.
	}
	// 检查新 g 的执行栈
	if newg.stack.hi == 0 {
		throw("newproc1: newg missing stack")
	}

	// 无论是取到的 g 还是新创建的 g，都应该是 _Gdead 状态
	if readgstatus(newg) != _Gdead {
		throw("newproc1: new g is not Gdead")
	}

	// 计算运行空间大小，与 spAlign 对齐
	totalSize := 4*sys.RegSize + uintptr(siz) + sys.MinFrameSize // extra space in case of reads slightly beyond frame
	totalSize += -totalSize & (sys.SpAlign - 1)                  // align to spAlign
	// 确定 sp 和参数入栈位置
	sp := newg.stack.hi - totalSize
	spArg := sp
	// arm
	if usesLR {
		// caller's LR
		// 调用方的 LR 寄存器
		*(*uintptr)(unsafe.Pointer(sp)) = 0
		prepGoExitFrame(sp)
		spArg += sys.MinFrameSize
	}
	// 处理参数，当有参数时，将参数拷贝到 goroutine 的执行栈中
	if narg > 0 {
		// 从 argp 参数开始的位置，复制 narg 个字节到 spArg（参数拷贝）
		memmove(unsafe.Pointer(spArg), unsafe.Pointer(argp), uintptr(narg))
		// This is a stack-to-stack copy. If write barriers
		// are enabled and the source stack is grey (the
		// destination is always black), then perform a
		// barrier copy. We do this *after* the memmove
		// because the destination stack may have garbage on
		// it.
		// 栈到栈的拷贝。如果启用了 write barrier 并且 源栈为灰色（目标始终为黑色），则执行 barrier 拷贝。因为目标栈上可能有垃圾，我们在 memmove 之后执行此操作。
		// 如果需要 write barrier 并且 gc scan 未结束，
		if writeBarrier.needed && !_g_.m.curg.gcscandone {
			f := findfunc(fn.fn)
			stkmap := (*stackmap)(funcdata(f, _FUNCDATA_ArgsPointerMaps))
			if stkmap.nbit > 0 {
				// We're in the prologue, so it's always stack map index 0.
				// 我们正位于 prologue (序言) 部分，因此栈 map 索引总是 0
				bv := stackmapdata(stkmap, 0)
				// bulkBarrierBitmap执行写入障碍
				bulkBarrierBitmap(spArg, spArg, uintptr(bv.n)*sys.PtrSize, 0, bv.bytedata)
			}
		}
	}

	// 清理、创建并初始化的 g 的运行现场
	memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched))
	// 设置 newg 的 sched 成员，调度器需要依靠这些字段才能把 goroutine 调度到 CPU 上运行。
	newg.sched.sp = sp // newg 的栈顶
	newg.stktopsp = sp
	// newg.sched.pc 表示当 newg 被调度起来运行时从这个地址开始执行指令，也说是 goexit 函数的第二条指令。
	// 把 pc 设置成了 goexit 这个函数偏移 1 （ amd64 中 sys.PCQuantum 等于 1 ）的位置。
	// 这里为什么 goexit 是第二条指令？ 需要看 gostartcallfn 函数。
	newg.sched.pc = funcPC(goexit) + sys.PCQuantum // +PCQuantum so that previous instruction is in same function // +PCQuantum 从而前一个指令还在相同的函数内
	newg.sched.g = guintptr(unsafe.Pointer(newg))
	// gostartcallfn 会获取 fn 的函数地址，然后调用 gostartcall
	// gostartcall函数的主要作用有两个：
	// 调整 newg 的栈空间，把 goexit 函数的第二条指令的地址入栈，伪造成 goexit 函数调用了 fn ，从而使 fn 执行完成后执行 ret 指令时返回到 goexit 继续执行完成最后的清理工作；
	// 重新设置 newg.buf.pc 为需要执行的函数的地址，即 fn ，初始化时为 runtime.main 函数的地址。
	gostartcallfn(&newg.sched, fn) //调整 sched 成员和 newg 的栈

	// 初始化 g 的基本状态
	newg.gopc = callerpc                     //主要用于traceback
	newg.ancestors = saveAncestors(callergp) // 调试相关，追踪调用方
	// 设置 newg 的 startpc 为 fn.fn ，该成员主要用于函数调用栈的 traceback 和栈收缩
	// newg 真正从哪里开始执行并不依赖于这个成员，而是 newg.sched.pc
	newg.startpc = fn.fn
	if _g_.m.curg != nil {
		// 设置 profiler 标签
		newg.labels = _g_.m.curg.labels
	}
	// 统计 sched.ngsys
	if isSystemGoroutine(newg, false) {
		atomic.Xadd(&sched.ngsys, +1)
	}
	newg.gcscanvalid = false
	// 将 g 更换为 _Grunnable 状态
	casgstatus(newg, _Gdead, _Grunnable)

	// 分配 goid
	if _p_.goidcache == _p_.goidcacheend {
		// Sched.goidgen is the last allocated id,
		// this batch must be [sched.goidgen+1, sched.goidgen+GoidCacheBatch].
		// At startup sched.goidgen=0, so main goroutine receives goid=1.
		//   Sched.goidgen 为最后一个分配的 id，这一批必须为 [sched.goidgen+1, sched.goidgen+GoidCacheBatch]。启动时 sched.goidgen=0, 因此主 goroutine 的 goid 为 1
		// 一次分配多个 _GoidCacheBatch(16) 个ID
		_p_.goidcache = atomic.Xadd64(&sched.goidgen, _GoidCacheBatch)
		_p_.goidcache -= _GoidCacheBatch - 1
		_p_.goidcacheend = _p_.goidcache + _GoidCacheBatch
	}
	newg.goid = int64(_p_.goidcache)
	_p_.goidcache++
	if raceenabled {
		newg.racectx = racegostart(callerpc)
	}
	// trace 相关
	if trace.enabled {
		traceGoCreate(newg, newg.startpc)
	}
	// 将这里新创建的 g 放入 p 的本地队列，如果已满，则放入全局队列，true 表示放入执行队列的下一个 (_p_.runnext)，false 表示放入队尾
	runqput(_p_, newg, true)

	// 如果有空闲的 P、且 spinning 的 M 数量为 0，且主 goroutine 已经开始运行，则进行唤醒 p 。初始化阶段 mainStarted 为 false，所以 p 不会被唤醒
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 && mainStarted {
		wakep()
	}
	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in case we've cleared it in newstack // 在 newstack 中清除了抢占请求的情况下恢复抢占请求
		_g_.stackguard0 = stackPreempt
	}
}

// saveAncestors copies previous ancestors of the given caller g and
// includes infor for the current caller into a new set of tracebacks for
// a g being created.
// saveAncestors 复制先前 ancestors 到给定调用者 g 。并将当前调用者的信息包含到正在创建的 g 新的一组 tracebacks 中。
func saveAncestors(callergp *g) *[]ancestorInfo {
	// Copy all prior info, except for the root goroutine (goid 0).
	if debug.tracebackancestors <= 0 || callergp.goid == 0 {
		return nil
	}
	var callerAncestors []ancestorInfo
	if callergp.ancestors != nil {
		callerAncestors = *callergp.ancestors
	}
	n := int32(len(callerAncestors)) + 1
	if n > debug.tracebackancestors {
		n = debug.tracebackancestors
	}
	ancestors := make([]ancestorInfo, n)
	copy(ancestors[1:], callerAncestors)

	var pcs [_TracebackMaxFrames]uintptr
	npcs := gcallers(callergp, 0, pcs[:])
	ipcs := make([]uintptr, npcs)
	copy(ipcs, pcs[:])
	ancestors[0] = ancestorInfo{
		pcs:  ipcs,
		goid: callergp.goid,
		gopc: callergp.gopc,
	}

	ancestorsp := new([]ancestorInfo)
	*ancestorsp = ancestors
	return ancestorsp
}

// Put on gfree list.
// If local list is too long, transfer a batch to the global list.
// 放在 gfree 列表中。如果本地列表太长，将一批转移到全局列表。
func gfput(_p_ *p, gp *g) {
	// 放入 gfree 的不能是 _Gdead 状态
	if readgstatus(gp) != _Gdead {
		throw("gfput: bad status (not Gdead)")
	}

	stksize := gp.stack.hi - gp.stack.lo

	if stksize != _FixedStack {
		// non-standard stack size - free it.
		// 不是标准的栈大小，释放
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		gp.stackguard0 = 0
	}

	// 放回到本地列表
	_p_.gFree.push(gp)
	_p_.gFree.n++
	// 如果本地列表过多（大于等于64），转移部分到全局队列（需要加锁）
	if _p_.gFree.n >= 64 {
		lock(&sched.gFree.lock)
		for _p_.gFree.n >= 32 { // 本地只保留31个
			_p_.gFree.n--
			gp = _p_.gFree.pop()
			if gp.stack.lo == 0 {
				sched.gFree.noStack.push(gp)
			} else {
				sched.gFree.stack.push(gp)
			}
			sched.gFree.n++
		}
		unlock(&sched.gFree.lock)
	}
}

// Get from gfree list.
// If local list is empty, grab a batch from global list.
// 从 gfree 链表中获取 g 。如果 P 本地 gfree 链表为空，从调度器的全局 gfree 链表中取
func gfget(_p_ *p) *g {
retry:
	// 如果本地没有空闲 G 但是全局有
	if _p_.gFree.empty() && (!sched.gFree.stack.empty() || !sched.gFree.noStack.empty()) {
		lock(&sched.gFree.lock)
		// Move a batch of free Gs to the P.
		// 将一批空闲的 G 移动到 P
		for _p_.gFree.n < 32 {
			// Prefer Gs with stacks.
			// 倾向于有栈的 G
			gp := sched.gFree.stack.pop()
			if gp == nil {
				gp = sched.gFree.noStack.pop()
				if gp == nil {
					break
				}
			}
			sched.gFree.n--
			_p_.gFree.push(gp)
			_p_.gFree.n++
		}
		unlock(&sched.gFree.lock)
		goto retry
	}
	// 从本地获取
	gp := _p_.gFree.pop()
	if gp == nil { // 本地没有，并且从全局没有获取到
		return nil
	}
	// 拿到 g
	_p_.gFree.n--
	// 查看是否需要分配运行栈
	if gp.stack.lo == 0 {
		// Stack was deallocated in gfput. Allocate a new one.
		// 栈已被 gfput 给释放，所以需要分配一个新的栈。在 g0 上分配。
		systemstack(func() {
			gp.stack = stackalloc(_FixedStack)
		})
		gp.stackguard0 = gp.stack.lo + _StackGuard
	} else {
		if raceenabled {
			racemalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
		if msanenabled {
			msanmalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
	}
	return gp
}

// Purge all cached G's from gfree list to the global list.
// 将所有缓存 G 从 gfree 列表清除到全局列表。
func gfpurge(_p_ *p) {
	lock(&sched.gFree.lock)
	for !_p_.gFree.empty() {
		gp := _p_.gFree.pop()
		_p_.gFree.n--
		if gp.stack.lo == 0 {
			sched.gFree.noStack.push(gp)
		} else {
			sched.gFree.stack.push(gp)
		}
		sched.gFree.n++
	}
	unlock(&sched.gFree.lock)
}

// Breakpoint executes a breakpoint trap.
// 断点
func Breakpoint() {
	breakpoint()
}

// dolockOSThread is called by LockOSThread and lockOSThread below
// after they modify m.locked. Do not allow preemption during this call,
// or else the m might be different in this function than in the caller.
//  dolockOSThread 在修改 m.locked 后由 LockOSThread 和 lockOSThread 调用。在此调用期间不允许抢占，否则此函数中的 m 可能与调用者中的 m 不同。
//go:nosplit
func dolockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	_g_ := getg()
	// 绑定 m 和 g
	_g_.m.lockedg.set(_g_)
	_g_.lockedm.set(_g_.m)
}

//go:nosplit

// LockOSThread wires the calling goroutine to its current operating system thread.
// The calling goroutine will always execute in that thread,
// and no other goroutine will execute in it,
// until the calling goroutine has made as many calls to
// UnlockOSThread as to LockOSThread.
// If the calling goroutine exits without unlocking the thread,
// the thread will be terminated.
//
// All init functions are run on the startup thread. Calling LockOSThread
// from an init function will cause the main function to be invoked on
// that thread.
//
// A goroutine should call LockOSThread before calling OS services or
// non-Go library functions that depend on per-thread state.
//  LockOSThread 将当前调用的 goroutine 连接到其当前的操作系统线程。此 goroutine 将始终在该线程中执行，并且不会在其中执行其他 goroutine ，
// 直到此 goroutine 对 UnlockOSThread 和 LockOSThread 调用相同的次数。 如果此 goroutine 在没有解锁线程的情况下退出，则该线程将终止。
// 所有 init 函数都在启动线程上运行。 从 init 函数调用 LockOSThread 将导致在该线程上调用main函数。
//  goroutine在调用OS服务或依赖于每个线程状态的非Go库函数之前，应先调用LockOSThread。
func LockOSThread() {
	if atomic.Load(&newmHandoff.haveTemplateThread) == 0 && GOOS != "plan9" {
		// If we need to start a new thread from the locked
		// thread, we need the template thread. Start it now
		// while we're in a known-good state.
		// 如果我们需要从锁定的线程启动一个新线程，我们需要模板线程。当我们处于一个已知良好的状态时，立即启动它。
		startTemplateThread()
	}
	_g_ := getg()
	_g_.m.lockedExt++
	if _g_.m.lockedExt == 0 { //  UnlockOSThread 调用和 LockOSThread 不匹配
		_g_.m.lockedExt--
		panic("LockOSThread nesting overflow")
	}
	dolockOSThread()
}

//  LockOSThread 内部版本
//go:nosplit
func lockOSThread() {
	getg().m.lockedInt++
	dolockOSThread()
}

// dounlockOSThread is called by UnlockOSThread and unlockOSThread below
// after they update m->locked. Do not allow preemption during this call,
// or else the m might be in different in this function than in the caller.
// 在 UnlockOSThread 和 unlockOSThread 修改 m.locked （m.lockedInt，m.lockedExt） 后由调用 dolockOSThread 。
// 在此调用期间不允许抢占，否则此函数中的 m 可能与调用者中的 m 不同。
//go:nosplit
func dounlockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	_g_ := getg()
	if _g_.m.lockedInt != 0 || _g_.m.lockedExt != 0 {
		return
	}
	// lockedInt 和 lockedExt 都为 0 后才清除
	_g_.m.lockedg = 0
	_g_.lockedm = 0
}

//go:nosplit

// UnlockOSThread undoes an earlier call to LockOSThread.
// If this drops the number of active LockOSThread calls on the
// calling goroutine to zero, it unwires the calling goroutine from
// its fixed operating system thread.
// If there are no active LockOSThread calls, this is a no-op.
//
// Before calling UnlockOSThread, the caller must ensure that the OS
// thread is suitable for running other goroutines. If the caller made
// any permanent changes to the state of the thread that would affect
// other goroutines, it should not call this function and thus leave
// the goroutine locked to the OS thread until the goroutine (and
// hence the thread) exits.
//  UnlockOSThread 撤消对 LockOSThread 的早期调用。如果这将正在调用的 goroutine 上的活动LockOSThread调用数降为零，
// 则将正在调用的 goroutine 从其固定的操作系统线程中断开连接。 如果没有活动的 LockOSThread 调用，则为空操作。
// 在调用 UnlockOSThread 之前，调用者必须确保 OS 线程适合运行其他 goroutine 。如果调用者对线程状态进行了永久更改，从而
// 影响其他 goroutine ，则不应调用此函数，因此将 goroutine 锁定在OS线程上，直到 goroutine （因此而来的线程）退出为止。
func UnlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedExt == 0 {
		return
	}
	_g_.m.lockedExt--
	dounlockOSThread()
}

// UnlockOSThread 内部版本
//go:nosplit
func unlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedInt == 0 {
		systemstack(badunlockosthread)
	}
	_g_.m.lockedInt--
	dounlockOSThread()
}

func badunlockosthread() {
	throw("runtime: internal error: misuse of lockOSThread/unlockOSThread")
}

// 获取 g 的数量，可能不准且，并没有加锁
func gcount() int32 {
	// 所有的 g - 全局空闲的 g - 系统调用的 g
	n := int32(allglen) - sched.gFree.n - int32(atomic.Load(&sched.ngsys))
	// 再减去每个 p 中空的 g
	for _, _p_ := range allp {
		n -= _p_.gFree.n
	}

	// All these variables can be changed concurrently, so the result can be inconsistent.
	// But at least the current goroutine is running.
	// 所有这些变量都可以同时更改，因此结果可能不一致。 但是至少当前的goroutine正在运行。
	if n < 1 {
		n = 1
	}
	return n
}

// 获取 m 的数量，可能不准且，并没有加锁
func mcount() int32 {
	return int32(sched.mnext - sched.nmfreed)
}

// cpu 性能相关
var prof struct {
	signalLock uint32
	hz         int32
}

func _System()                    { _System() }
func _ExternalCode()              { _ExternalCode() }
func _LostExternalCode()          { _LostExternalCode() }
func _GC()                        { _GC() }
func _LostSIGPROFDuringAtomic64() { _LostSIGPROFDuringAtomic64() }
func _VDSO()                      { _VDSO() }

// Counts SIGPROFs received while in atomic64 critical section, on mips{,le}
// 计算在 mips{,le} 上收到的 SIGPROF 信号计数。
var lostAtomic64Count uint64

// Called if we receive a SIGPROF signal.
// Called by the signal handler, may run during STW.
// 当收到 SIGPROF 信号的时候被  signal handler 调用，可能在 STW 期间。signal_sighandler.go 中 sighandler 调用
//go:nowritebarrierrec
func sigprof(pc, sp, lr uintptr, gp *g, mp *m) {
	if prof.hz == 0 {
		return
	}

	// On mips{,le}, 64bit atomics are emulated with spinlocks, in
	// runtime/internal/atomic. If SIGPROF arrives while the program is inside
	// the critical section, it creates a deadlock (when writing the sample).
	// As a workaround, create a counter of SIGPROFs while in critical section
	// to store the count, and pass it to sigprof.add() later when SIGPROF is
	// received from somewhere else (with _LostSIGPROFDuringAtomic64 as pc).
	// 在 mips{,le} 上，在 runtime/internal/atomic 中使用自旋锁模拟64位原子。如果 SIGPROF 在程序位于临界区之内到达时，则会创建死锁（在编写示例时）。
	// 解决方法是，在临界区中创建一个 SIGPROF 计数器以存储计数，然后在从其他地方收到 SIGPROF 时将其传递给sigprof.add()（使用 _LostSIGPROFDuringAtomic64 作为pc）。
	if GOARCH == "mips" || GOARCH == "mipsle" || GOARCH == "arm" {
		if f := findfunc(pc); f.valid() {
			if hasPrefix(funcname(f), "runtime/internal/atomic") {
				lostAtomic64Count++
				return
			}
		}
	}

	// Profiling runs concurrently with GC, so it must not allocate.
	// Set a trap in case the code does allocate.
	// Note that on windows, one thread takes profiles of all the
	// other threads, so mp is usually not getg().m.
	// In fact mp may not even be stopped.
	// See golang.org/issue/17165.
	//  Profiling 与 GC 并行运行，因此不能分配。设置陷阱，以防代码确实分配。请注意，在Windows上，一个线程获取所有其他线程的 profiles ，
	// 因此 mp 通常不是 getg().m 。 实际上，mp甚至都无法停止。
	getg().m.mallocing++

	// Define that a "user g" is a user-created goroutine, and a "system g"
	// is one that is m->g0 or m->gsignal.
	// 定义 "用户 g" 是用户创建的 goroutine ， "系统 g" 是 m->g0 或 m->gsignal。
	//
	// We might be interrupted for profiling halfway through a
	// goroutine switch. The switch involves updating three (or four) values:
	// g, PC, SP, and (on arm) LR. The PC must be the last to be updated,
	// because once it gets updated the new g is running.
	// 我们可能会在进行 goroutine 切换时为了 profiling 而被打断 。切换涉及更新三个（或四个）值：g，PC，SP和（on arm）LR。
	//  PC 必须是最后一个要更新的，因为一旦更新，新 g 就会运行。
	//
	// When switching from a user g to a system g, LR is not considered live,
	// so the update only affects g, SP, and PC. Since PC must be last, there
	// the possible partial transitions in ordinary execution are (1) g alone is updated,
	// (2) both g and SP are updated, and (3) SP alone is updated.
	// If SP or g alone is updated, we can detect the partial transition by checking
	// whether the SP is within g's stack bounds. (We could also require that SP
	// be changed only after g, but the stack bounds check is needed by other
	// cases, so there is no need to impose an additional requirement.)
	// 从用户 g 切换到系统 g 时， LR 不被视为存活，因此更新仅影响 g ， SP 和 PC 。 由于 PC 必须位于最后，因此在常规执行中可能会发生部分转换：
	// （1）仅更新 g ，（2） g 和 SP 均被更新，以及（3）仅更新 SP 。 如果仅更新 SP 或 g ，我们可以通过检查 SP 是否在 g 的栈范围内来检测部分。
	// 转换（我们也可能要求仅在 g 之后更改 SP ，但是在其他情况下则需要进行堆栈边界检查，因此无需强加其他要求。）
	//
	// There is one exceptional transition to a system g, not in ordinary execution.
	// When a signal arrives, the operating system starts the signal handler running
	// with an updated PC and SP. The g is updated last, at the beginning of the
	// handler. There are two reasons this is okay. First, until g is updated the
	// g and SP do not match, so the stack bounds check detects the partial transition.
	// Second, signal handlers currently run with signals disabled, so a profiling
	// signal cannot arrive during the handler.
	// 有一种到系统 g 的特殊过渡，不是普通执行。当信号到达时，操作系统将启动信号处理器，并在更新的 PC 和 SP 上运行。 g 在处理程序的开头最后更新。
	// 可以这样做有两个原因。 首先，在更新 g 之前， g 和 SP 不匹配，因此栈边界检查将检测到部分转换。其次，信号处理程序当前在禁用信号的情况下运行，
	// 因此 profiling 信号无法在处理程序期间到达。
	//
	// When switching from a system g to a user g, there are three possibilities.
	// 从系统g切换到用户g时，有三种可能性。
	//
	// First, it may be that the g switch has no PC update, because the SP
	// either corresponds to a user g throughout (as in asmcgocall)
	// or because it has been arranged to look like a user g frame
	// (as in cgocallback_gofunc). In this case, since the entire
	// transition is a g+SP update, a partial transition updating just one of
	// those will be detected by the stack bounds check.
	// 首先，可能是 g 切换没有更新 PC ，因为 SP 要么一直对应于用户 g （如在 asmcgocall 中），要么因为它看起来像用户 g 帧（如在cgocallback_gofunc中）。
	// 在这种情况下，由于整个转换都是 g+SP 更新，因此栈边界检查将检测到仅更新其中之一的部分转换。
	//
	// Second, when returning from a signal handler, the PC and SP updates
	// are performed by the operating system in an atomic update, so the g
	// update must be done before them. The stack bounds check detects
	// the partial transition here, and (again) signal handlers run with signals
	// disabled, so a profiling signal cannot arrive then anyway.
	// 其次，从信号处理程序返回时， PC 和 SP 更新是由操作系统以原子更新的方式执行的，因此 g 更新必须在它们之前完成。栈边界检查在此处检测到部分过渡，并且（再次）
	// 信号处理程序在禁用信号的情况下运行，因此无论如何都无法到达 profiling 信号。
	//
	// Third, the common case: it may be that the switch updates g, SP, and PC
	// separately. If the PC is within any of the functions that does this,
	// we don't ask for a traceback. C.F. the function setsSP for more about this.
	// 第三，常见情况：切换可能分别更新g，SP和PC。如果 PC 在执行此操作的任何函数之内，则我们不要求 traceback 。 请参照函数 setsSP 对此有更多了解。
	//
	// There is another apparently viable approach, recorded here in case
	// the "PC within setsSP function" check turns out not to be usable.
	// It would be possible to delay the update of either g or SP until immediately
	// before the PC update instruction. Then, because of the stack bounds check,
	// the only problematic interrupt point is just before that PC update instruction,
	// and the sigprof handler can detect that instruction and simulate stepping past
	// it in order to reach a consistent state. On ARM, the update of g must be made
	// in two places (in R10 and also in a TLS slot), so the delayed update would
	// need to be the SP update. The sigprof handler must read the instruction at
	// the current PC and if it was the known instruction (for example, JMP BX or
	// MOV R2, PC), use that other register in place of the PC value.
	// The biggest drawback to this solution is that it requires that we can tell
	// whether it's safe to read from the memory pointed at by PC.
	// In a correct program, we can test PC == nil and otherwise read,
	// but if a profiling signal happens at the instant that a program executes
	// a bad jump (before the program manages to handle the resulting fault)
	// the profiling handler could fault trying to read nonexistent memory.
	// 还有另一种显然可行的方法，如果 "PC 在 setsSP 函数内" 检查结果不可用， 请在此处记录。可以将 g 或 SP 的更新延迟到 PC 更新指令之前立即更新。
	// 然后，由于栈边界检查，唯一有问题的中断点就在该 PC 更新指令之前，并且 sigprof 处理程序可以检测到该指令并模拟经过它的步骤以达到一致状态。
	// 在 ARM 上，必须在两个位置（在 R10 和TLS slot中）进行 g 的更新，因此延迟的更新将需要是 SP 更新。 sigprof 处理程序必须在当前PC上读取该指令，
	// 如果它是已知指令（例如，JMP BX 或 MOV R2,PC ），则使用该其他寄存器代替 PC 值。该解决方案的最大缺点是，它要求我们知道从 PC 指向的内存中读取
	// 是否安全。在正确的程序中，我们可以测试 PC == nil 并以其他方式读取，但是如果在程序执行错误跳转的瞬间（在程序设法处理所产生的错误之前）出现
	//  profiling 信号，则 profiling 分析处理程序可能会尝试读取不存在的内存。
	//
	// To recap, there are no constraints on the assembly being used for the
	// transition. We simply require that g and SP match and that the PC is not
	// in gogo.
	// 概括地说，在这个切换中使用的汇编没有约束。 我们只要求 g 和 SP 匹配且 PC 不在 gogo 中即可。
	traceback := true
	if gp == nil || sp < gp.stack.lo || gp.stack.hi < sp || setsSP(pc) || (mp != nil && mp.vdsoSP != 0) {
		traceback = false
	}
	var stk [maxCPUProfStack]uintptr
	n := 0
	if mp.ncgo > 0 && mp.curg != nil && mp.curg.syscallpc != 0 && mp.curg.syscallsp != 0 {
		cgoOff := 0
		// Check cgoCallersUse to make sure that we are not
		// interrupting other code that is fiddling with
		// cgoCallers.  We are running in a signal handler
		// with all signals blocked, so we don't have to worry
		// about any other code interrupting us.
		// 检查 cgoCallersUse 以确保我们不会中断其他与 cgoCallers 纠缠的代码。我们正在信号处理程序中运行，所有信号都被阻塞，因此我们不必担心任何其他代码会中断我们。
		if atomic.Load(&mp.cgoCallersUse) == 0 && mp.cgoCallers != nil && mp.cgoCallers[0] != 0 {
			for cgoOff < len(mp.cgoCallers) && mp.cgoCallers[cgoOff] != 0 {
				cgoOff++
			}
			copy(stk[:], mp.cgoCallers[:cgoOff])
			mp.cgoCallers[0] = 0
		}

		// Collect Go stack that leads to the cgo call.
		// 收集转到 cgo 调用的 Go 堆栈。
		n = gentraceback(mp.curg.syscallpc, mp.curg.syscallsp, 0, mp.curg, 0, &stk[cgoOff], len(stk)-cgoOff, nil, nil, 0)
		if n > 0 {
			n += cgoOff
		}
	} else if traceback {
		n = gentraceback(pc, sp, lr, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
	}

	if n <= 0 {
		// Normal traceback is impossible or has failed.
		// See if it falls into several common cases.
		// 正常回溯是不可能的或已失败。 查看是否属于几种常见情况。
		n = 0
		if (GOOS == "windows" || GOOS == "solaris" || GOOS == "darwin") && mp.libcallg != 0 && mp.libcallpc != 0 && mp.libcallsp != 0 {
			// Libcall, i.e. runtime syscall on windows.
			// Collect Go stack that leads to the call.
			//  Libcall ，即 Windows 上的运行时系统调用。 收集导致调用的 Go 堆栈。
			n = gentraceback(mp.libcallpc, mp.libcallsp, 0, mp.libcallg.ptr(), 0, &stk[0], len(stk), nil, nil, 0)
		}
		if n == 0 && mp != nil && mp.vdsoSP != 0 {
			n = gentraceback(mp.vdsoPC, mp.vdsoSP, 0, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
		}
		if n == 0 {
			// If all of the above has failed, account it against abstract "System" or "GC".
			// 如果以上所有方法均失败，请针对抽象的“系统”或“GC”进行说明。
			n = 2
			if inVDSOPage(pc) {
				pc = funcPC(_VDSO) + sys.PCQuantum
			} else if pc > firstmoduledata.etext {
				// "ExternalCode" is better than "etext".
				pc = funcPC(_ExternalCode) + sys.PCQuantum
			}
			stk[0] = pc
			if mp.preemptoff != "" {
				stk[1] = funcPC(_GC) + sys.PCQuantum
			} else {
				stk[1] = funcPC(_System) + sys.PCQuantum
			}
		}
	}

	if prof.hz != 0 {
		if (GOARCH == "mips" || GOARCH == "mipsle" || GOARCH == "arm") && lostAtomic64Count > 0 {
			cpuprof.addLostAtomic64(lostAtomic64Count)
			lostAtomic64Count = 0
		}
		cpuprof.add(gp, stk[:n])
	}
	getg().m.mallocing--
}

// If the signal handler receives a SIGPROF signal on a non-Go thread,
// it tries to collect a traceback into sigprofCallers.
// sigprofCallersUse is set to non-zero while sigprofCallers holds a traceback.
// 如果信号处理程序在非 Go 线程上收到 SIGPROF 信号，它将尝试将 traceback 收集到 sigprofCallers 中。 sigprofCallersUse 设置为非零，而 sigprofCallers 保留 traceback 。
var sigprofCallers cgoCallers
var sigprofCallersUse uint32

// sigprofNonGo is called if we receive a SIGPROF signal on a non-Go thread,
// and the signal handler collected a stack trace in sigprofCallers.
// When this is called, sigprofCallersUse will be non-zero.
// g is nil, and what we can do is very limited.
// 如果我们在非 Go 线程上收到 SIGPROF 信号，并且信号处理程序在 sigprofCallers 中收集了 stack trace ，则会调用 sigprofNonGo 。 调用此方法时，sigprofCallersUse 将为非零。
//  g 为 nil ，我们所能做的非常有限。
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGo() {
	if prof.hz != 0 {
		n := 0
		for n < len(sigprofCallers) && sigprofCallers[n] != 0 {
			n++
		}
		cpuprof.addNonGo(sigprofCallers[:n])
	}

	atomic.Store(&sigprofCallersUse, 0)
}

// sigprofNonGoPC is called when a profiling signal arrived on a
// non-Go thread and we have a single PC value, not a stack trace.
// g is nil, and what we can do is very limited.
// 当 profiling 信号到达非 Go 线程并且我们具有单个 PC 值而不是 stack trace 时，将调用sigprofNonGoPC。
//  g 为 nil ，我们所能做的非常有限。
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGoPC(pc uintptr) {
	if prof.hz != 0 {
		stk := []uintptr{
			pc,
			funcPC(_ExternalCode) + sys.PCQuantum,
		}
		cpuprof.addNonGo(stk)
	}
}

// Reports whether a function will set the SP
// to an absolute value. Important that
// we don't traceback when these are at the bottom
// of the stack since we can't be sure that we will
// find the caller.
//
// If the function is not on the bottom of the stack
// we assume that it will have set it up so that traceback will be consistent,
// either by being a traceback terminating function
// or putting one on the stack at the right offset.
// 报告是否将 SP 设置为绝对值。 重要的是，当它们位于栈的底部时，我们不要 traceback ，因为我们不确定将会找到调用者。
// 如果该函数不在栈底部，则假定它已经设置好，以便 traceback 是一致的，可以是作为 traceback 终止函数，也可以是一个以正确的偏移量放在栈上。
func setsSP(pc uintptr) bool {
	f := findfunc(pc)
	if !f.valid() {
		// couldn't find the function for this PC,
		// so assume the worst and stop traceback
		// 找不到此 PC 的函数，因此假设情况最糟并停止 traceback
		return true
	}
	switch f.funcID {
	case funcID_gogo, funcID_systemstack, funcID_mcall, funcID_morestack:
		return true
	}
	return false
}

// setcpuprofilerate sets the CPU profiling rate to hz times per second.
// If hz <= 0, setcpuprofilerate turns off CPU profiling.
//  setcpuprofilerate 将 CPU profiling 速率设置为每秒hz次。 如果hz <= 0，则 setcpuprofilerate 关闭 CPU profiling。
func setcpuprofilerate(hz int32) {
	// Force sane arguments. // 强制理智的参数， hz 不能小于 0
	if hz < 0 {
		hz = 0
	}

	// Disable preemption, otherwise we can be rescheduled to another thread
	// that has profiling enabled.
	// 禁用抢占，否则我们可以重新安排到另一个启用了性能分析的线程。
	_g_ := getg()
	_g_.m.locks++

	// Stop profiler on this thread so that it is safe to lock prof.
	// if a profiling signal came in while we had prof locked,
	// it would deadlock.
	// 在此线程上停止 profiler ，以便安全锁定 prof 。 如果在我们锁定 prof 时出现 profiling 信号，则会死锁。
	setThreadCPUProfiler(0)

	// 锁住 prof
	for !atomic.Cas(&prof.signalLock, 0, 1) {
		osyield() // 没锁柱就让出 cpu
	}
	// 如果hz变了，就修改下
	if prof.hz != hz {
		setProcessCPUProfiler(hz)
		prof.hz = hz
	}
	// 解锁 prof
	atomic.Store(&prof.signalLock, 0)

	// 设置 sched.profilehz
	lock(&sched.lock)
	sched.profilehz = hz
	unlock(&sched.lock)

	if hz != 0 {
		setThreadCPUProfiler(hz)
	}

	_g_.m.locks--
}

// Change number of processors. The world is stopped, sched is locked.
// gcworkbufs are not being modified by either the GC or
// the write barrier code.
// Returns list of Ps with local work, they need to be scheduled by the caller.
// 修改 P 的数量，此时所有工作均被停止 STW，sched 被锁定。 gcworkbufs 既不会被 GC 修改，也不会被 write barrier 修改。
// 返回带有 local work 的 P 列表，他们需要被调用方调度。
func procresize(nprocs int32) *p {
	// 获取之前的 P 个数
	old := gomaxprocs
	// 边界检查
	if old < 0 || nprocs <= 0 {
		throw("procresize: invalid arg")
	}
	// trace 相关
	if trace.enabled {
		traceGomaxprocs(nprocs)
	}

	// update statistics
	// 更新统计信息，记录此次修改 gomaxprocs 的时间
	now := nanotime()
	if sched.procresizetime != 0 {
		sched.totaltime += int64(old) * (now - sched.procresizetime)
	}
	sched.procresizetime = now

	// Grow allp if necessary.
	// 必要时增加 allp
	// 这个时候本质上是在检查用户代码是否有调用过 runtime.MAXGOPROCS 调整 p 的数量。
	// 此处多一步检查是为了避免内部的锁，如果 nprocs 明显小于 allp 的可见数量，则不需要进行加锁
	if nprocs > int32(len(allp)) {
		// Synchronize with retake, which could be running
		// concurrently since it doesn't run on a P.
		// 此处与 retake 同步，它可以同时运行，因为它不会在 P 上运行。
		lock(&allpLock)
		if nprocs <= int32(cap(allp)) {
			// 如果 allp 容量足够，去切片就好了
			allp = allp[:nprocs]
		} else {
			// 否则 allp 容量不够，重新申请
			nallp := make([]*p, nprocs)
			// Copy everything up to allp's cap so we
			// never lose old allocated Ps.
			// 将所有内容复制到 allp 的上，这样我们就永远不会丢失旧分配的P 。
			copy(nallp, allp[:cap(allp)])
			allp = nallp
		}
		unlock(&allpLock)
	}

	// initialize new P's
	// 初始化新的 P
	for i := int32(0); i < nprocs; i++ {
		pp := allp[i]
		// 如果 p 是新创建的(新创建的 p 在数组中为 nil)，则申请新的 P 对象
		if pp == nil {
			pp = new(p)
			pp.id = i            //  p 的 id 就是它在 allp 中的索引
			pp.status = _Pgcstop // 新创建的 p 处于 _Pgcstop 状态
			pp.sudogcache = pp.sudogbuf[:0]
			for i := range pp.deferpool {
				pp.deferpool[i] = pp.deferpoolbuf[i][:0]
			}
			pp.wbBuf.reset()
			atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
		}
		// 为 P 分配 cache 对象
		if pp.mcache == nil {
			// 如果 old == 0 且 i == 0 说明这是引导阶段初始化第一个 p 。schedinit 中 mallocinit 有初始化一个 mcache
			if old == 0 && i == 0 {
				// 确认当前 g 的 m 的 mcache 非空
				if getg().m.mcache == nil {
					throw("missing mcache?")
				}
				pp.mcache = getg().m.mcache // bootstrap
			} else {
				pp.mcache = allocmcache()
			}
		}
		// 如果 启动了 race 并且 racectx 为 0，则新建
		if raceenabled && pp.racectx == 0 {
			// 如果 old == 0 且 i == 0 说明这是引导阶段初始化第一个 p 。 schedinit 中有初始化一个 raceproccreate
			if old == 0 && i == 0 {
				pp.racectx = raceprocctx0
				raceprocctx0 = 0 // bootstrap
			} else {
				pp.racectx = raceproccreate()
			}
		}
	}

	// free unused P's
	// 释放不用的 P
	for i := nprocs; i < old; i++ {
		p := allp[i]
		if trace.enabled && p == getg().m.p.ptr() {
			// moving to p[0], pretend that we were descheduled
			// and then scheduled again to keep the trace sane.
			// 移至 p[0] ，假装我们已被调度，然后再次调度以保持跟踪正常。
			traceGoSched()
			traceProcStop(p)
		}
		// move all runnable goroutines to the global queue
		// 将所有的 runnable goroutines 移动到全局队列 sched.runq
		for p.runqhead != p.runqtail {
			// pop from tail of local queue
			// 从本地队列的尾部 pop
			p.runqtail--
			gp := p.runq[p.runqtail%uint32(len(p.runq))].ptr()
			// push onto head of global queue
			//  push 到全局队列的头部
			globrunqputhead(gp)
		}
		// 如果 runnext 不为 0，也加入到全局队列 sched.runq
		if p.runnext != 0 {
			globrunqputhead(p.runnext.ptr())
			p.runnext = 0
		}
		// if there's a background worker, make it runnable and put
		// it on the global queue so it can clean itself up
		// 如果存在 gc 后台 worker，则让其 runnable 并将其放到全局队列中从而可以让其对自身进行清理
		if gp := p.gcBgMarkWorker.ptr(); gp != nil {
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			globrunqput(gp)
			// This assignment doesn't race because the
			// world is stopped.
			// 此赋值不会发生竞争，因为此时已经 STW
			p.gcBgMarkWorker.set(nil)
		}
		// Flush p's write barrier buffer.
		// 刷新 p 的写屏障缓存
		if gcphase != _GCoff {
			wbBufFlush1(p)
			p.gcw.dispose()
		}
		// 设置 sudogbuf
		for i := range p.sudogbuf {
			p.sudogbuf[i] = nil
		}
		p.sudogcache = p.sudogbuf[:0]
		for i := range p.deferpool {
			for j := range p.deferpoolbuf[i] {
				p.deferpoolbuf[i][j] = nil
			}
			p.deferpool[i] = p.deferpoolbuf[i][:0]
		}
		// 释放当前 P 绑定的 mcache
		freemcache(p.mcache)
		p.mcache = nil
		// 将当前 P 的 G 复链转移到全局
		gfpurge(p)
		traceProcFree(p)
		if raceenabled {
			raceprocdestroy(p.racectx)
			p.racectx = 0
		}
		p.gcAssistTime = 0
		p.status = _Pdead
		// can't free P itself because it can be referenced by an M in syscall
		// 不能释放 P 本身，因为它可能被系统调用的 M 引用。
	}

	// Trim allp.
	// 修剪 allp
	if int32(len(allp)) != nprocs {
		lock(&allpLock)
		allp = allp[:nprocs]
		unlock(&allpLock)
	}

	_g_ := getg()
	if _g_.m.p != 0 && _g_.m.p.ptr().id < nprocs { // 当前的 P 不需要被释放
		// continue to use the current P
		// 继续使用当前 P
		_g_.m.p.ptr().status = _Prunning
		_g_.m.p.ptr().mcache.prepareForSweep()
	} else {
		// release the current P and acquire allp[0]
		// 释放当前 P，然后获取 allp[0]
		//  p 和 m 解绑
		if _g_.m.p != 0 {
			_g_.m.p.ptr().m = 0
		}
		_g_.m.p = 0
		_g_.m.mcache = nil
		// 更换到 allp[0]
		p := allp[0]
		p.m = 0
		p.status = _Pidle
		acquirep(p) // 直接将 allp[0] 绑定到当前的 M
		if trace.enabled {
			traceGoStart()
		}
	}
	var runnablePs *p
	for i := nprocs - 1; i >= 0; i-- {
		p := allp[i]
		// 确保不是当前正在使用的 P
		if _g_.m.p.ptr() == p {
			continue
		}

		// 将 p 设为 _Pidle
		p.status = _Pidle

		// 本地任务列表是否为空
		if runqempty(p) {
			// 放入 idle 链表
			pidleput(p)
		} else {
			// 如果有本地任务，则为其绑定一个 M（不一定能获取到）
			p.m.set(mget())
			// 第一个循环为 nil，后续则为上一个 p，此处即为构建可运行的 p 链表
			p.link.set(runnablePs)
			runnablePs = p
		}
	}
	stealOrder.reset(uint32(nprocs))
	var int32p *int32 = &gomaxprocs // make compiler check that gomaxprocs is an int32 // 让编译器检查 gomaxprocs 是 int32 类型
	atomic.Store((*uint32)(unsafe.Pointer(int32p)), uint32(nprocs))
	// 让编译器检查 gomaxprocs 是 int32 类型
	return runnablePs
}

// Associate p and the current m.
//
// This function is allowed to have write barriers even if the caller
// isn't because it immediately acquires _p_.
// 将 p 关联到当前的 m 。因为该函数会立即 acquire P，因此即使调用方不允许 write barrier，此函数仍然允许 write barrier。
//
//go:yeswritebarrierrec
func acquirep(_p_ *p) {
	// Do the part that isn't allowed to have write barriers.
	// 此处不允许 write barrier 。关联了当前的 M 到 P 上。
	wirep(_p_)

	// Have p; write barriers now allowed. // 已经获取了 p，因此之后允许 write barrier

	// Perform deferred mcache flush before this P can allocate
	// from a potentially stale mcache.
	// 在此 P 可以从可能过时的 mcache 分配前执行延迟的 mcache flush
	_p_.mcache.prepareForSweep()

	if trace.enabled {
		traceProcStart()
	}
}

// wirep is the first step of acquirep, which actually associates the
// current M to _p_. This is broken out so we can disallow write
// barriers for this part, since we don't yet have a P.
//  wirep 为 acquirep 的实际获取 p 的第一步，它关联了当前的 M 到 P 上。 我们在这部分使用 write barriers 被打破了，因为我们还没有P。
//
//go:nowritebarrierrec
//go:nosplit
func wirep(_p_ *p) {
	_g_ := getg()

	// 如果当前的 m 已经关联了 p
	if _g_.m.p != 0 || _g_.m.mcache != nil {
		throw("wirep: already in go")
	}
	// 如果 _p_ 已经关联了 m ， 且 _p_ 的状态不是 _Pidle
	if _p_.m != 0 || _p_.status != _Pidle {
		id := int64(0)
		if _p_.m != 0 {
			id = _p_.m.ptr().id
		}
		print("wirep: p->m=", _p_.m, "(", id, ") p->status=", _p_.status, "\n")
		throw("wirep: invalid p state")
	}
	// 关联当前 m 和 _p_ ， 并设置 _p_ 为 _Prunning
	_g_.m.mcache = _p_.mcache // 使用 p 的 mcache
	_g_.m.p.set(_p_)          // 将 p 关联到到 m
	_p_.m.set(_g_.m)          // 将 m 关联到到 p
	_p_.status = _Prunning    // 设置 _Prunning
}

// Disassociate p and the current m.
// 取消关联 p 和当前 m 。
func releasep() *p {
	_g_ := getg()

	// 如果 m 根本就没有关联的 p
	if _g_.m.p == 0 || _g_.m.mcache == nil {
		throw("releasep: invalid arg")
	}
	// 获取当前 m 关联的 p
	_p_ := _g_.m.p.ptr()
	// 检测
	if _p_.m.ptr() != _g_.m || _p_.mcache != _g_.m.mcache || _p_.status != _Prunning {
		print("releasep: m=", _g_.m, " m->p=", _g_.m.p.ptr(), " p->m=", _p_.m, " m->mcache=", _g_.m.mcache, " p->mcache=", _p_.mcache, " p->status=", _p_.status, "\n")
		throw("releasep: invalid p state")
	}
	if trace.enabled {
		traceProcStop(_g_.m.p.ptr())
	}
	// 取消关联清除设置
	_g_.m.p = 0
	_g_.m.mcache = nil
	_p_.m = 0
	_p_.status = _Pidle // 将 p 设置为 _Pidle 状态
	return _p_
}

// 增加当前等待工作的被 lock 的 M 计数
func incidlelocked(v int32) {
	lock(&sched.lock)
	sched.nmidlelocked += v
	if v > 0 {
		checkdead() // 检测死锁
	}
	unlock(&sched.lock)
}

// Check for deadlock situation.
// The check is based on number of running M's, if 0 -> deadlock.
// sched.lock must be held.
// 检查死锁情况。 检查基于当前运行的 M 的数量，如果 0 则表示死锁。sched.lock 必须被持有（加锁）。
func checkdead() {
	// For -buildmode=c-shared or -buildmode=c-archive it's OK if
	// there are no running goroutines. The calling program is
	// assumed to be running.
	// 对于 -buildmode=c-shared 或 -buildmode=c-archive，如果没有正在运行的 goroutine ，则可以。 假定调用程序正在运行。
	if islibrary || isarchive {
		return
	}

	// If we are dying because of a signal caught on an already idle thread,
	// freezetheworld will cause all running threads to block.
	// And runtime will essentially enter into deadlock state,
	// except that there is a thread that will call exit soon.
	// 如果我们由于在一个已经闲置的线程上捕获到信号而垂死，freezetheworld 将导致所有正在运行的线程阻塞。运行时实质上将进入死锁状态，除了有一个线程即将调用exit之外。
	if panicking > 0 {
		return
	}

	// If we are not running under cgo, but we have an extra M then account
	// for it. (It is possible to have an extra M on Windows without cgo to
	// accommodate callbacks created by syscall.NewCallback. See issue #6751
	// for details.)
	// 如果我们不在 cgo 下运行，但是我们有一个额外的M，则为它计数。（在没有 cgo 的 Windows 上可能会有一个额外的 M ，以容纳 syscall.NewCallback 创建的回调。
	var run0 int32
	if !iscgo && cgoHasExtraM {
		run0 = 1
	}

	// 总共的 m 的数量 - 等待 work 的空闲 m 的数量 - 等待 work 的锁住的 m 的数量 - 不计入死锁的系统 m 的数量
	run := mcount() - sched.nmidle - sched.nmidlelocked - sched.nmsys
	if run > run0 {
		return
	}
	// 计数出现问题了 ？
	if run < 0 {
		print("runtime: checkdead: nmidle=", sched.nmidle, " nmidlelocked=", sched.nmidlelocked, " mcount=", mcount(), " nmsys=", sched.nmsys, "\n")
		throw("checkdead: inconsistent counts")
	}
	// 此时 run 要么为 0 要么为 1

	grunning := 0 // 运行的 g 的数目
	lock(&allglock)
	// 遍历所有的 g
	for i := 0; i < len(allgs); i++ {
		gp := allgs[i]
		if isSystemGoroutine(gp, false) { // 系统 g ，不计数
			continue
		}
		s := readgstatus(gp)
		switch s &^ _Gscan {
		case _Gwaiting:
			grunning++
		case _Grunnable,
			_Grunning,
			_Gsyscall:
			unlock(&allglock)
			print("runtime: checkdead: find g ", gp.goid, " in status ", s, "\n")
			throw("checkdead: runnable g")
			// 此时应该没有在运行的 g
		}
	}
	unlock(&allglock)
	if grunning == 0 { // possible if main goroutine calls runtime·Goexit() // 可能 main goroutine 调用 runtime·Goexit()
		throw("no goroutines (main called runtime.Goexit) - deadlock!")
	}

	// Maybe jump time forward for playground.
	// 检查 timer
	gp := timejump()
	if gp != nil {
		casgstatus(gp, _Gwaiting, _Grunnable) // 设置状态为 _Grunnable
		globrunqput(gp)                       // 放入全局任务队列
		_p_ := pidleget()                     // 获取空闲的 p
		if _p_ == nil {
			throw("checkdead: no p for timer")
		}
		mp := mget() // 获取空闲的 m
		if mp == nil {
			// There should always be a free M since
			// nothing is running.
			throw("checkdead: no m for timer")
		}
		mp.nextp.set(_p_)    // 设置 p 用于后续绑定
		notewakeup(&mp.park) // 唤醒 mp
		return
	}

	getg().m.throwing = -1 // do not dump full stacks // 不 dump 完整的 stacks
	throw("all goroutines are asleep - deadlock!")
}

// forcegcperiod is the maximum time in nanoseconds between garbage
// collections. If we go this long without a garbage collection, one
// is forced to run.
//
// This is a variable for testing purposes. It normally doesn't change.
//  forcegcperiod 是两次 GC 之间的最长时间（以纳秒为单位）。 如果我们在没有垃圾回收的情况下走了这么长时间，就会被迫运行。
var forcegcperiod int64 = 2 * 60 * 1e9 // 2 分钟

// Always runs without a P, so write barriers are not allowed.
// 总是在没有 P 的情况下运行，因此 write barrier 是不允许的。系统监控在一个独立的 m 上运行。
//
//go:nowritebarrierrec
func sysmon() {
	lock(&sched.lock)
	sched.nmsys++ // 不计入死锁的系统 m 的数量
	checkdead()   // 检查死锁
	unlock(&sched.lock)

	// If a heap span goes unused for 5 minutes after a garbage collection,
	// we hand it back to the operating system.
	// 如果在垃圾回收后5分钟内未使用 heap span ，我们会将其交还给操作系统。
	scavengelimit := int64(5 * 60 * 1e9)

	// 如果开启了 scavenge debug ，打印 scavenge 信息
	if debug.scavenge > 0 {
		// Scavenge-a-lot for testing.
		forcegcperiod = 10 * 1e6 // 强制 GC 间隔时间设置为 10 毫秒
		scavengelimit = 20 * 1e6 //  scavengelimit 设置为 20 毫秒
	}

	lastscavenge := nanotime() // 上一次时间
	nscavenge := 0

	lasttrace := int64(0) // 上一次 trace
	idle := 0             // how many cycles in succession we had not wokeup somebody // 已经连续多久没有唤醒了
	delay := uint32(0)    // 延迟时间，单位微妙
	for {
		if idle == 0 { // start with 20us sleep... // 每次启动先休眠 20us
			delay = 20
		} else if idle > 50 { // start doubling the sleep after 1ms... //  1ms 后就翻倍休眠时间
			delay *= 2
		}
		if delay > 10*1000 { // up to 10ms // 最多 10ms
			delay = 10 * 1000
		}
		usleep(delay) // sleep 一会儿
		// 如果在 STW，则暂时休眠
		if debug.schedtrace <= 0 && (sched.gcwaiting != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs)) {
			lock(&sched.lock)
			if atomic.Load(&sched.gcwaiting) != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs) {
				atomic.Store(&sched.sysmonwait, 1)
				unlock(&sched.lock)
				// Make wake-up period small enough
				// for the sampling to be correct.
				// 确保 wake-up 周期足够小从而进行正确的采样
				maxsleep := forcegcperiod / 2
				if scavengelimit < forcegcperiod {
					maxsleep = scavengelimit / 2
				}
				shouldRelax := true
				if osRelaxMinNS > 0 {
					next := timeSleepUntil()
					now := nanotime()
					if next-now < osRelaxMinNS {
						shouldRelax = false
					}
				}
				if shouldRelax {
					osRelax(true)
				}
				notetsleep(&sched.sysmonnote, maxsleep) // 休眠
				if shouldRelax {
					osRelax(false)
				}
				lock(&sched.lock)
				atomic.Store(&sched.sysmonwait, 0)
				noteclear(&sched.sysmonnote)
				idle = 0
				delay = 20
			}
			unlock(&sched.lock)
		}
		// trigger libc interceptors if needed
		// 需要时触发 libc interceptor
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		// poll network if not polled for more than 10ms
		// 如果超过 10ms 没有 poll，则 poll 一下网络
		lastpoll := int64(atomic.Load64(&sched.lastpoll))
		now := nanotime()
		if netpollinited() && lastpoll != 0 && lastpoll+10*1000*1000 < now {
			atomic.Cas64(&sched.lastpoll, uint64(lastpoll), uint64(now))
			list := netpoll(false) // non-blocking - returns list of goroutines // 非阻塞，返回 goroutine 列表
			if !list.empty() {
				// Need to decrement number of idle locked M's
				// (pretending that one more is running) before injectglist.
				// Otherwise it can lead to the following situation:
				// injectglist grabs all P's but before it starts M's to run the P's,
				// another M returns from syscall, finishes running its G,
				// observes that there is no work to do and no other running M's
				// and reports deadlock.
				// 需要在插入 g 列表前减少空闲锁住的 m 的数量（假装有一个正在运行）。否则会导致这些情况：injectglist 在它启动 M 来运行 P 之前会绑定所有的 p，
				// 另一个 M 从 syscall 返回，完成运行它的 G ，注意这时候没有 work 要做，且没有其他正在运行 M ，就会产生死锁。
				incidlelocked(-1)
				injectglist(&list)
				incidlelocked(1)
			}
		}
		// retake P's blocked in syscalls
		// and preempt long running G's
		// 抢夺在 syscall 中阻塞的 P、运行时间过长的 G
		if retake(now) != 0 {
			idle = 0 // 抢占到了到重设 idle
		} else {
			idle++ // 否则累加
		}
		// check if we need to force a GC
		// 检查是否需要强制触发 GC
		if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && atomic.Load(&forcegc.idle) != 0 {
			lock(&forcegc.lock)
			forcegc.idle = 0
			var list gList
			list.push(forcegc.g)
			injectglist(&list)
			unlock(&forcegc.lock)
		}
		// scavenge heap once in a while
		// 堆 scavenge
		if lastscavenge+scavengelimit/2 < now {
			mheap_.scavenge(int32(nscavenge), uint64(now), uint64(scavengelimit))
			lastscavenge = now
			nscavenge++
		}
		//  trace 相关
		if debug.schedtrace > 0 && lasttrace+int64(debug.schedtrace)*1000000 <= now {
			lasttrace = now
			schedtrace(debug.scheddetail > 0)
		}
	}
}

type sysmontick struct {
	schedtick   uint32
	schedwhen   int64
	syscalltick uint32
	syscallwhen int64
}

// forcePreemptNS is the time slice given to a G before it is
// preempted.
// forcePreemptNS 是 G 被抢占之前的时间片。
const forcePreemptNS = 10 * 1000 * 1000 // 10ms

// 抢占
func retake(now int64) uint32 {
	n := 0
	// Prevent allp slice changes. This lock will be completely
	// uncontended unless we're already stopping the world.
	// 防止allp slice更改。 除非已经 STW ，否则这种锁定将是没有竞争的。
	lock(&allpLock)
	// We can't use a range loop over allp because we may
	// temporarily drop the allpLock. Hence, we need to re-fetch
	// allp each time around the loop.
	// 不能使用 range 循环，因为 range 可能临时性的放弃 allpLock。所以每轮循环中都需要重新获取 allp
	for i := 0; i < len(allp); i++ {
		_p_ := allp[i]
		if _p_ == nil {
			// This can happen if procresize has grown
			// allp but not yet created new Ps.
			// 这是可能发生的，如果 procresize 已经增长 allp 但还没有创建新的 P
			continue
		}
		pd := &_p_.sysmontick
		s := _p_.status
		if s == _Psyscall {
			// Retake P from syscall if it's there for more than 1 sysmon tick (at least 20us).
			// 如果已经超过了一个系统监控的 tick（20us），则从系统调用中抢占 P
			t := int64(_p_.syscalltick)
			if int64(pd.syscalltick) != t {
				pd.syscalltick = uint32(t)
				pd.syscallwhen = now
				continue
			}
			// On the one hand we don't want to retake Ps if there is no other work to do,
			// but on the other hand we want to retake them eventually
			// because they can prevent the sysmon thread from deep sleep.
			// 一方面，在没有其他 work 的情况下，我们不希望抢夺 P 。另一方面，因为它可能阻止 sysmon 线程从深度睡眠中唤醒，所以最终我们仍希望抢夺 P 。
			if runqempty(_p_) && atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) > 0 && pd.syscallwhen+10*1000*1000 > now {
				continue
			}
			// Drop allpLock so we can take sched.lock.
			// 解除 allpLock，从而可以获取 sched.lock
			unlock(&allpLock)
			// Need to decrement number of idle locked M's
			// (pretending that one more is running) before the CAS.
			// Otherwise the M from which we retake can exit the syscall,
			// increment nmidle and report deadlock.
			// 在 CAS 之前需要减少空闲 M 的数量（假装某个还在运行）。否则发生抢夺的 M 可能退出 syscall 然后再增加 nmidle ，进而发生死锁。
			// 这个过程发生在 stoplockedm 中
			incidlelocked(-1)
			// 将 P 设为 idle，从而交于其他 M 使用
			if atomic.Cas(&_p_.status, s, _Pidle) {
				if trace.enabled {
					traceGoSysBlock(_p_)
					traceProcStop(_p_)
				}
				n++
				_p_.syscalltick++
				handoffp(_p_) // 让出 p
			}
			incidlelocked(1)
			lock(&allpLock)
		} else if s == _Prunning {
			// Preempt G if it's running for too long.
			// 抢占已经运行很久的 g
			t := int64(_p_.schedtick)
			if int64(pd.schedtick) != t {
				pd.schedtick = uint32(t)
				pd.schedwhen = now
				continue
			}
			if pd.schedwhen+forcePreemptNS > now {
				continue
			}
			// 抢占
			preemptone(_p_)
		}
	}
	unlock(&allpLock)
	return uint32(n)
}

// Tell all goroutines that they have been preempted and they should stop.
// This function is purely best-effort. It can fail to inform a goroutine if a
// processor just started running it.
// No locks need to be held.
// Returns true if preemption request was issued to at least one goroutine.
// 告诉所有 goroutine ，它们已被抢占，应该停止。此功能纯粹是尽力而为。如果处理器刚刚开始运行 goroutine ，它可能无法通知goroutine。
// 无需持有任何锁。如果对至少一个 goroutine 发出了抢占请求，则返回true。
func preemptall() bool {
	res := false
	// 遍历所有的 p
	for _, _p_ := range allp {
		if _p_.status != _Prunning {
			continue
		}
		// 只请求处于 _Prunning 状态的 goroutine 暂停
		if preemptone(_p_) {
			res = true
		}
	}
	return res
}

// Tell the goroutine running on processor P to stop.
// This function is purely best-effort. It can incorrectly fail to inform the
// goroutine. It can send inform the wrong goroutine. Even if it informs the
// correct goroutine, that goroutine might ignore the request if it is
// simultaneously executing newstack.
// No lock needs to be held.
// Returns true if preemption request was issued.
// The actual preemption will happen at some point in the future
// and will be indicated by the gp->status no longer being
// Grunning
// 通知运行 goroutine 的 P 停止。该函数仅仅只是尽力而为。他完全有可能通知到错误的 goroutine 上。即使它通知到了正确的 goroutine，
// 如果它此时正在执行 newstack，这个 goroutine 也可能无视这个请求。
// 不需要持有锁，如果抢占请发送成功，则返回真。实际抢占会发生在未来发生，并通过 gp.status 来指明不再为 Grunning 状态
func preemptone(_p_ *p) bool {
	mp := _p_.m.ptr()
	if mp == nil || mp == getg().m {
		return false
	}
	gp := mp.curg // 通过 p 找到正在执行的 goroutine
	if gp == nil || gp == mp.g0 {
		return false
	}

	gp.preempt = true // 设置抢占调度标记

	// Every call in a go routine checks for stack overflow by
	// comparing the current stack pointer to gp->stackguard0.
	// Setting gp->stackguard0 to StackPreempt folds
	// preemption into the normal stack overflow check.
	// goroutine 中的每个调用都通过将当前堆栈指针与 gp->stackguard0 比较来检查堆栈溢出。将 gp->stackguard0 设置为 StackPreempt 来将抢占转换为正常的栈溢出检查。
	//
	// 设置扩栈标记，这里用来触发被请求 goroutine 执行扩栈函数 morestack_noctxt()->morestack()->newstack()
	// 在 newstack 函数中如果发现自己被抢占，则会暂停当前 goroutine 的执行
	gp.stackguard0 = stackPreempt
	return true
}

// 启动时间
var starttime int64

// 调度 trace， detailed 是否打印详情
func schedtrace(detailed bool) {
	now := nanotime()
	if starttime == 0 {
		starttime = now
	}

	lock(&sched.lock)
	// 打印调度 trace
	print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs, " idleprocs=", sched.npidle, " threads=", mcount(), " spinningthreads=", sched.nmspinning, " idlethreads=", sched.nmidle, " runqueue=", sched.runqsize)
	// 详细信息
	if detailed {
		print(" gcwaiting=", sched.gcwaiting, " nmidlelocked=", sched.nmidlelocked, " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait, "\n")
	}
	// We must be careful while reading data from P's, M's and G's.
	// Even if we hold schedlock, most data can be changed concurrently.
	// E.g. (p->m ? p->m->id : -1) can crash if p->m changes from non-nil to nil.
	// 从 P ， M 和 G 读取数据时，我们必须小心。 即使我们持有 schedlock ，大多数数据也可以同时更改。 例如。 如果 p->m 从 non-nil 变为 nil ，
	// 则 (p->m ? p->m->id : -1) 可能会崩溃。
	// 打印 P 的信息
	for i, _p_ := range allp {
		mp := _p_.m.ptr()
		h := atomic.Load(&_p_.runqhead)
		t := atomic.Load(&_p_.runqtail)
		// 详细信息
		if detailed {
			id := int64(-1)
			if mp != nil {
				id = mp.id
			}
			print("  P", i, ": status=", _p_.status, " schedtick=", _p_.schedtick, " syscalltick=", _p_.syscalltick, " m=", id, " runqsize=", t-h, " gfreecnt=", _p_.gFree.n, "\n")
		} else {
			// In non-detailed mode format lengths of per-P run queues as:
			// [len1 len2 len3 len4]
			// 在非详细模式下，每P运行队列的格式长度为：[len1 len2 len3 len4]
			print(" ")
			if i == 0 {
				print("[")
			}
			print(t - h)
			if i == len(allp)-1 {
				print("]\n")
			}
		}
	}

	// 如果不需要打印详情，则返回
	if !detailed {
		unlock(&sched.lock)
		return
	}

	// 打印 M 的信息
	for mp := allm; mp != nil; mp = mp.alllink {
		_p_ := mp.p.ptr()
		gp := mp.curg
		lockedg := mp.lockedg.ptr()
		id1 := int32(-1)
		if _p_ != nil {
			id1 = _p_.id
		}
		id2 := int64(-1)
		if gp != nil {
			id2 = gp.goid
		}
		id3 := int64(-1)
		if lockedg != nil {
			id3 = lockedg.goid
		}
		print("  M", mp.id, ": p=", id1, " curg=", id2, " mallocing=", mp.mallocing, " throwing=", mp.throwing, " preemptoff=", mp.preemptoff, ""+" locks=", mp.locks, " dying=", mp.dying, " spinning=", mp.spinning, " blocked=", mp.blocked, " lockedg=", id3, "\n")
	}

	// 打印 G 的信息
	lock(&allglock)
	for gi := 0; gi < len(allgs); gi++ {
		gp := allgs[gi]
		mp := gp.m
		lockedm := gp.lockedm.ptr()
		id1 := int64(-1)
		if mp != nil {
			id1 = mp.id
		}
		id2 := int64(-1)
		if lockedm != nil {
			id2 = lockedm.id
		}
		print("  G", gp.goid, ": status=", readgstatus(gp), "(", gp.waitreason.String(), ") m=", id1, " lockedm=", id2, "\n")
	}
	unlock(&allglock)
	unlock(&sched.lock)
}

// schedEnableUser enables or disables the scheduling of user
// goroutines.
//
// This does not stop already running user goroutines, so the caller
// should first stop the world when disabling user goroutines.
//  schedEnableUser 启用或禁用用户 goroutine 的调度。这不会停止已经在运行的用户 goroutine ，因此，在禁用用户 goroutine 时，调用者应首先停止运行。
func schedEnableUser(enable bool) {
	lock(&sched.lock)
	if sched.disable.user == !enable {
		unlock(&sched.lock)
		return
	}
	sched.disable.user = !enable
	if enable {
		n := sched.disable.n
		sched.disable.n = 0
		globrunqputbatch(&sched.disable.runnable, n) // 如果重新启动了用户 g ，批量 sched.disable.runnable 放入到全局队列
		unlock(&sched.lock)
		for ; n != 0 && sched.npidle != 0; n-- {
			startm(nil, false) // 启动对应多的 m 来执行，如果没有空闲的 p 了，也不会启动 p
		}
	} else {
		unlock(&sched.lock)
	}
}

// schedEnabled reports whether gp should be scheduled. It returns
// false is scheduling of gp is disabled.
//  schedEnabled 返回是否能调度 gp 。 如果禁用 gp 调度，则返回 false 。
func schedEnabled(gp *g) bool {
	if sched.disable.user {
		return isSystemGoroutine(gp, true)
	}
	return true
}

// Put mp on midle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
// 将 mp 放至空闲列表。调用此调用必须将调度器锁住。可能在 STW 期间运行，因此不允许 write barrier 。
//go:nowritebarrierrec
func mput(mp *m) {
	// 链接起来并递增计数
	mp.schedlink = sched.midle
	sched.midle.set(mp)
	sched.nmidle++
	checkdead() // 检查死锁
}

// Try to get an m from midle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
// 尝试从 midel 列表中获取一个 M 。调用此调用必须将调度器锁住。可能在 STW 期间运行，因此不允许 write barrier 。
//go:nowritebarrierrec
func mget() *m {
	mp := sched.midle.ptr()
	if mp != nil {
		sched.midle = mp.schedlink
		sched.nmidle--
	}
	return mp
}

// Put gp on the global runnable queue.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
// 将 g 放入到全局运行队列尾部。调用此调用必须将调度器锁住。可能在 STW 期间运行，因此不允许 write barrier 。
//go:nowritebarrierrec
func globrunqput(gp *g) {
	sched.runq.pushBack(gp)
	sched.runqsize++
}

// Put gp at the head of the global runnable queue.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
// 将 g 放到全局队列头部。调用此调用必须将调度器锁住。可能在 STW 期间运行，因此不允许 write barrier 。
//go:nowritebarrierrec
func globrunqputhead(gp *g) {
	sched.runq.push(gp)
	sched.runqsize++
}

// Put a batch of runnable goroutines on the global runnable queue.
// This clears *batch.
// Sched must be locked.
//   将一批 runnable goroutine 放入全局 runnable 队列尾部。清除 *batch 。调用此调用必须将调度器锁住。
func globrunqputbatch(batch *gQueue, n int32) {
	sched.runq.pushBackAll(*batch)
	sched.runqsize += n
	*batch = gQueue{}
}

// Try get a batch of G's from the global runnable queue.
// Sched must be locked.
// 尝试从全局运行队列获取一批 g 。调用此调用必须将调度器锁住。
func globrunqget(_p_ *p, max int32) *g {
	// 如果全局队列中没有 g 直接返回
	if sched.runqsize == 0 {
		return nil
	}

	// 这里将全局运行队列的均分下，不让一个 p 获取太多了
	n := sched.runqsize/gomaxprocs + 1
	if n > sched.runqsize {
		n = sched.runqsize
	}
	//  n 也不能超过 max
	if max > 0 && n > max {
		n = max
	}
	// 也不能多余 p 本地队列容量的一半，不然放不下
	if n > int32(len(_p_.runq))/2 {
		n = int32(len(_p_.runq)) / 2
	}

	// 修改全局运行队列剩余数
	sched.runqsize -= n

	// 先获取一个 g ， 用作返回值
	gp := sched.runq.pop()
	n--
	// 其他的 n - 1 个放入本地队列
	for ; n > 0; n-- {
		gp1 := sched.runq.pop()
		runqput(_p_, gp1, false)
	}
	return gp
}

// Put p to on _Pidle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
// 将 p 放入 _Pidle 列表。调用此调用必须将调度器锁住。可能在 STW 期间运行，因此不允许 write barrier 。
//go:nowritebarrierrec
func pidleput(_p_ *p) {
	if !runqempty(_p_) {
		throw("pidleput: P has non-empty run queue")
	}
	// 链接起来并递增计数
	_p_.link = sched.pidle
	sched.pidle.set(_p_)
	atomic.Xadd(&sched.npidle, 1) // TODO: fast atomic
}

// Try get a p from _Pidle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
// 尝试从 _Pidle 列表获取一个 p 。调用此调用必须将调度器锁住。可能在 STW 期间运行，因此不允许 write barrier 。
//go:nowritebarrierrec
func pidleget() *p {
	_p_ := sched.pidle.ptr()
	if _p_ != nil {
		sched.pidle = _p_.link
		atomic.Xadd(&sched.npidle, -1) // TODO: fast atomic
	}
	return _p_
}

// runqempty reports whether _p_ has no Gs on its local run queue.
// It never returns true spuriously.
//  runqempty 返回 true，如果 _p_ 的本地队列没有 G 。它永远不会错误的返回 true 。
func runqempty(_p_ *p) bool {
	// Defend against a race where 1) _p_ has G1 in runqnext but runqhead == runqtail,
	// 2) runqput on _p_ kicks G1 to the runq, 3) runqget on _p_ empties runqnext.
	// Simply observing that runqhead == runqtail and then observing that runqnext == nil
	// does not mean the queue is empty.
	// 在以下地方防止 race :
	// 1）_p_ 在 runqnext 中有G1 ，但 runqhead == runqtail ，2）_p_ 上的 runqput 将 G1 踢到 runq ，3）_p_ 上的 runqget 清空 runqnext 。
	// 只需观察 runqhead == runqtail ，然后观察 runqnext == nil 并不意味着队列为空。
	for {
		head := atomic.Load(&_p_.runqhead)
		tail := atomic.Load(&_p_.runqtail)
		runnext := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&_p_.runnext)))
		if tail == atomic.Load(&_p_.runqtail) {
			return head == tail && runnext == 0
		}
	}
}

// To shake out latent assumptions about scheduling order,
// we introduce some randomness into scheduling decisions
// when running with the race detector.
// The need for this was made obvious by changing the
// (deterministic) scheduling order in Go 1.5 and breaking
// many poorly-written tests.
// With the randomness here, as long as the tests pass
// consistently with -race, they shouldn't have latent scheduling
// assumptions.
// 为了摆脱关于调度顺序的潜在假设，我们运行 race 检测器时将一些随机性引入调度决策中。 通过更改 Go 1.5 中的（确定性）计划顺序并破坏许多编写不当的测试，这一需求变得显而易见。
// 考虑到这里的随机性，只要测试与 -race 一致通过，它们就不应具有潜在的调度假设。
const randomizeScheduler = raceenabled

// runqput tries to put g on the local runnable queue.
// If next is false, runqput adds g to the tail of the runnable queue.
// If next is true, runqput puts g in the _p_.runnext slot.
// If the run queue is full, runnext puts g on the global queue.
// Executed only by the owner P.
//  runqput 尝试将 g 放入本地可运行队列中。如果 next 为 false，则 runqput 会将 g 放到可运行队列的尾部。如果 next 为 true，则 runqput 会将 g 放入 _p_.runnext 槽内。
// 如果运行队列已满，则runnext 会放到全局队列中去。仅在所有 P 下执行此函数。
func runqput(_p_ *p, gp *g, next bool) {
	if randomizeScheduler && next && fastrand()%2 == 0 {
		next = false //  randomizeScheduler 为 flase ， 这里不可能发送
	}

	if next {
	retryNext:
		oldnext := _p_.runnext
		// 如果是放到 runnext 上，则先将原来的替换出来。
		if !_p_.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
			goto retryNext
		}
		if oldnext == 0 {
			return
		}
		// Kick the old runnext out to the regular run queue.
		// 将原先的 runnext 踢出普通运行队列
		gp = oldnext.ptr()
	}

retry:
	h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with consumers // load-acquire, 与 consumer 进行同步
	t := _p_.runqtail
	// 果 P 的本地队列没有满，入队
	if t-h < uint32(len(_p_.runq)) {
		_p_.runq[t%uint32(len(_p_.runq))].set(gp)
		atomic.StoreRel(&_p_.runqtail, t+1) // store-release, makes the item available for consumption // store-release, 使 consumer 可以开始消费这个 item
		return
	}
	// 可运行队列已经满了，放到全局队列中，包含 gp 和本地队列的一半
	if runqputslow(_p_, gp, h, t) {
		return
	}
	// the queue is not full, now the put above must succeed
	// 没有成功放入全局队列，说明本地队列没满，重试一下。
	goto retry
}

// Put g and a batch of work from local runnable queue on global queue.
// Executed only by the owner P.
// 将 g 和一批 work 从本地 runnable 队列放入全局队列。由拥有 P 的 M 执行。
func runqputslow(_p_ *p, gp *g, h, t uint32) bool {
	var batch [len(_p_.runq)/2 + 1]*g

	// First, grab a batch from local queue.
	// 首先，从本地队列中抓取一半 work。
	n := t - h
	n = n / 2
	if n != uint32(len(_p_.runq)/2) {
		throw("runqputslow: queue is not full")
	}
	// 保存到 batch
	for i := uint32(0); i < n; i++ {
		batch[i] = _p_.runq[(h+i)%uint32(len(_p_.runq))].ptr()
	}
	if !atomic.CasRel(&_p_.runqhead, h, h+n) { // cas-release, commits consume //  cas-release, 提交消费
		return false
	}
	//  gp 也加入到其中
	batch[n] = gp

	// 打乱顺序，也不会发生  randomizeScheduler = false
	if randomizeScheduler {
		for i := uint32(1); i <= n; i++ {
			j := fastrandn(i + 1)
			batch[i], batch[j] = batch[j], batch[i]
		}
	}

	// Link the goroutines.
	// 将 goroutine 彼此连接
	for i := uint32(0); i < n; i++ {
		batch[i].schedlink.set(batch[i+1])
	}
	var q gQueue
	q.head.set(batch[0])
	q.tail.set(batch[n])

	// Now put the batch on global queue.
	// 将这批 work 放到全局队列中去。
	lock(&sched.lock)
	globrunqputbatch(&q, int32(n+1))
	unlock(&sched.lock)
	return true
}

// Get g from local runnable queue.
// If inheritTime is true, gp should inherit the remaining time in the
// current time slice. Otherwise, it should start a new time slice.
// Executed only by the owner P.
// 从本地可运行队列中获取 g 。如果 inheritTime 为 true，则 g 继承剩余的时间片。否则开始一个新的时间片。在所有者 P 上执行。
func runqget(_p_ *p) (gp *g, inheritTime bool) {
	// If there's a runnext, it's the next G to run.
	// 如果有 runnext，则为下一个要运行的 g
	for {
		next := _p_.runnext
		if next == 0 {
			break
		}
		// 如果 cas 成功，则 next 继承剩余时间片执行
		if _p_.runnext.cas(next, 0) {
			return next.ptr(), true
		}
	}

	for {
		h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with other consumers // load-acquire, 与其他消费者同步
		t := _p_.runqtail
		if t == h {
			return nil, false // 本地队列是空，返回 nil
		}
		gp := _p_.runq[h%uint32(len(_p_.runq))].ptr()
		if atomic.CasRel(&_p_.runqhead, h, h+1) { // cas-release, commits consume //  cas-release, 提交消费
			return gp, false // 找到了
		}
	}
}

// Grabs a batch of goroutines from _p_'s runnable queue into batch.
// Batch is a ring buffer starting at batchHead.
// Returns number of grabbed goroutines.
// Can be executed by any P.
// 从 _p_ 的可运行队列中获取一批 goroutines 到 batch 中。 Batch 是从 batchHead 开始的环形缓冲区。 返回获取的goroutine的数量。 可以由任何P执行。
func runqgrab(_p_ *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
	for {
		h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with other consumers // load-acquire, 与其他消费者同步
		t := atomic.LoadAcq(&_p_.runqtail) // load-acquire, synchronize with the producer // load-acquire, 与其他生产者同步
		n := t - h
		n = n - n/2 // 一半的 g
		if n == 0 {
			if stealRunNextG {
				// Try to steal from _p_.runnext.
				// 尝试从 _p_.runnext 偷
				if next := _p_.runnext; next != 0 {
					if _p_.status == _Prunning {
						// Sleep to ensure that _p_ isn't about to run the g
						// we are about to steal.
						// The important use case here is when the g running
						// on _p_ ready()s another g and then almost
						// immediately blocks. Instead of stealing runnext
						// in this window, back off to give _p_ a chance to
						// schedule runnext. This will avoid thrashing gs
						// between different Ps.
						// A sync chan send/recv takes ~50ns as of time of
						// writing, so 3us gives ~50x overshoot.
						// 睡眠以确保 _p_ 不会运行我们将要窃取的g。这里重要的用例是当在 _p_ 上运行的 g 来 ready() 另一个 g ，然后并且几乎立即阻塞时。
						// 不要在此窗口中窃取 runnext ，而是退让给 _p_ 安排 runnext 的机会。这将避免在不同的 P 之间打乱 g 。
						// 截至撰写本文时，同步 send/recv 需要约 50ns 的时间，因此 3us 会产生约 50 倍的过冲。
						if GOOS != "windows" {
							usleep(3)
						} else {
							// On windows system timer granularity is
							// 1-15ms, which is way too much for this
							// optimization. So just yield.
							// 在 Windows 系统上，计时器的粒度为 1-15 毫秒，对于此优化而言，这太多了。 所以就调用 yield 。
							osyield()
						}
					}
					//  cas 失败，runnext 已经变了， 重新来
					if !_p_.runnext.cas(next, 0) {
						continue
					}
					// 偷到了 runnext
					batch[batchHead%uint32(len(batch))] = next
					return 1
				}
			}
			return 0
		}
		if n > uint32(len(_p_.runq)/2) { // read inconsistent h and t // 读取不一致的 h 和 t
			continue
		}
		// 窃取 n 个
		for i := uint32(0); i < n; i++ {
			g := _p_.runq[(h+i)%uint32(len(_p_.runq))]
			batch[(batchHead+i)%uint32(len(batch))] = g
		}
		//  cas 成功， 则返回； 否则继续。
		if atomic.CasRel(&_p_.runqhead, h, h+n) { // cas-release, commits consume //  cas-release, 提交消费
			return n
		}
	}
}

// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
// 从 p2 runnable 队列中偷取一半的元素并将其放入 p 的 runnable 队列中。返回其中一个偷取的元素（如果失败则返回 nil）
func runqsteal(_p_, p2 *p, stealRunNextG bool) *g {
	t := _p_.runqtail
	// 去偷吧
	n := runqgrab(p2, &_p_.runq, t, stealRunNextG)
	if n == 0 {
		return nil
	}
	// 获取返回的拿个
	n--
	gp := _p_.runq[(t+n)%uint32(len(_p_.runq))].ptr()
	if n == 0 { // 只有1个， 则返回
		return gp
	}
	// 之前在 runqgrab 已经放到 runq 中了， 但是没有更新 runqtail ，以下会判断一下正确性，再修改 runqtail 。
	h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with consumers // load-acquire, 与 consumer 进行同步
	if t-h+n >= uint32(len(_p_.runq)) {
		throw("runqsteal: runq overflow")
	}
	atomic.StoreRel(&_p_.runqtail, t+n) // store-release, makes the item available for consumption // store-release, 使 consumer 可以开始消费这个 item
	return gp
}

// A gQueue is a dequeue of Gs linked through g.schedlink. A G can only
// be on one gQueue or gList at a time.
//  gQueue 是通过 g.schedlink 链接的 G 的 dequeue （双端队列） 。 一个 G 一次只能位于一个 gQueue 或 gList 上。
type gQueue struct {
	head guintptr
	tail guintptr
}

// empty reports whether q is empty. // 判断是否为空
func (q *gQueue) empty() bool {
	return q.head == 0
}

// push adds gp to the head of q. // 插入队头
func (q *gQueue) push(gp *g) {
	gp.schedlink = q.head
	q.head.set(gp)
	if q.tail == 0 {
		q.tail.set(gp)
	}
}

// pushBack adds gp to the tail of q. // 插入队尾
func (q *gQueue) pushBack(gp *g) {
	gp.schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink.set(gp)
	} else {
		q.head.set(gp)
	}
	q.tail.set(gp)
}

// pushBackAll adds all Gs in l2 to the tail of q. After this q2 must
// not be used. //  pushBackAll 将 q2 中的所有 G 加到 q 的尾部。 此后不得使用 q2 。
func (q *gQueue) pushBackAll(q2 gQueue) {
	if q2.tail == 0 {
		return
	}
	q2.tail.ptr().schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink = q2.head
	} else {
		q.head = q2.head
	}
	q.tail = q2.tail
}

// pop removes and returns the head of queue q. It returns nil if
// q is empty. // 弹出队头节点
func (q *gQueue) pop() *g {
	gp := q.head.ptr()
	if gp != nil {
		q.head = gp.schedlink
		if q.head == 0 {
			q.tail = 0
		}
	}
	return gp
}

// popList takes all Gs in q and returns them as a gList. // 获取所有的 g ，返回为 gList
func (q *gQueue) popList() gList {
	stack := gList{q.head}
	*q = gQueue{}
	return stack
}

// A gList is a list of Gs linked through g.schedlink. A G can only be
// on one gQueue or gList at a time.
type gList struct {
	head guintptr
}

// empty reports whether l is empty. // 判断是否为空
func (l *gList) empty() bool {
	return l.head == 0
}

// push adds gp to the head of l. // 插入链头部
func (l *gList) push(gp *g) {
	gp.schedlink = l.head
	l.head.set(gp)
}

// pushAll prepends all Gs in q to l. // 将 q 中的所有 G 插入到 gList 头部
func (l *gList) pushAll(q gQueue) {
	if !q.empty() {
		q.tail.ptr().schedlink = l.head
		l.head = q.head
	}
}

// pop removes and returns the head of l. If l is empty, it returns nil. // 弹出列表头节点
func (l *gList) pop() *g {
	gp := l.head.ptr()
	if gp != nil {
		l.head = gp.schedlink
	}
	return gp
}

// 链接函数 runtime/debug.setMaxThreads 。设置最大线程数
//go:linkname setMaxThreads runtime/debug.setMaxThreads
func setMaxThreads(in int) (out int) {
	lock(&sched.lock)
	out = int(sched.maxmcount)
	if in > 0x7fffffff { // MaxInt32
		sched.maxmcount = 0x7fffffff
	} else {
		sched.maxmcount = int32(in)
	}
	checkmcount()
	unlock(&sched.lock)
	return
}

func haveexperiment(name string) bool {
	if name == "framepointer" {
		return framepointer_enabled // set by linker // 通过链接器设置
	}
	x := sys.Goexperiment
	for x != "" {
		xname := ""
		i := index(x, ",")
		if i < 0 {
			xname, x = x, ""
		} else {
			xname, x = x[:i], x[i+1:]
		}
		if xname == name {
			return true
		}
		if len(xname) > 2 && xname[:2] == "no" && xname[2:] == name {
			return false
		}
	}
	return false
}

//  procPin 设置禁止抢占（避免GC），返回当前 P.id
//go:nosplit
func procPin() int {
	_g_ := getg()
	mp := _g_.m

	mp.locks++
	return int(mp.p.ptr().id)
}

// 撤销 procPin 设置的禁止抢占
//go:nosplit
func procUnpin() {
	_g_ := getg()
	_g_.m.locks--
}

//go:linkname sync_runtime_procPin sync.runtime_procPin
//go:nosplit
func sync_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_runtime_procUnpin sync.runtime_procUnpin
//go:nosplit
func sync_runtime_procUnpin() {
	procUnpin()
}

//go:linkname sync_atomic_runtime_procPin sync/atomic.runtime_procPin
//go:nosplit
func sync_atomic_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_atomic_runtime_procUnpin sync/atomic.runtime_procUnpin
//go:nosplit
func sync_atomic_runtime_procUnpin() {
	procUnpin()
}

// Active spinning for sync.Mutex.
// 为 sync.Mutex 激活 spinning ，返回是否可以 spin
// 链接函数sync_runtime_canSpin为sync.runtime_canSpin
//go:linkname sync_runtime_canSpin sync.runtime_canSpin
//go:nosplit
func sync_runtime_canSpin(i int) bool {
	// sync.Mutex is cooperative, so we are conservative with spinning.
	// Spin only few times and only if running on a multicore machine and
	// GOMAXPROCS>1 and there is at least one other running P and local runq is empty.
	// As opposed to runtime mutex we don't do passive spinning here,
	// because there can be work on global runq or on other Ps.
	//  sync.Mutex 是协作的，因此我们对 spinning 保持保守。
	// 仅在多核计算机上运行、GOMAXPROCS>1 、正在运行的 P 并且本地运行队列为空时，满足之前几点，才 Spin 几次。
	// 与运行时互斥锁相反，我们在此不进行被动 spinning ，因为可以在全局 runq 或其他 P 上有任务。
	if i >= active_spin || ncpu <= 1 || gomaxprocs <= int32(sched.npidle+sched.nmspinning)+1 {
		return false
	}
	if p := getg().m.p.ptr(); !runqempty(p) {
		return false
	}
	return true
}

// 为 sync.Mutex 执行 spin
// 链接函数sync_runtime_doSpin为sync.runtime_doSpin
//go:linkname sync_runtime_doSpin sync.runtime_doSpin
//go:nosplit
func sync_runtime_doSpin() {
	// 让出 cpu
	procyield(active_spin_cnt)
}

var stealOrder randomOrder

// randomOrder/randomEnum are helper types for randomized work stealing.
// They allow to enumerate all Ps in different pseudo-random orders without repetitions.
// The algorithm is based on the fact that if we have X such that X and GOMAXPROCS
// are coprime, then a sequences of (i + X) % GOMAXPROCS gives the required enumeration.
//  randomOrder/randomEnum 是用于随机窃取工作的。它们允许以不同的伪随机顺序枚举所有 P ，而无需重复。
// 该算法基于以下事实：如果我们有 X ，使得 X 和 GOMAXPROCS 是互质的，则 (i + X) % GOMAXPROCS 的序列将提供所需的枚举。
type randomOrder struct {
	count    uint32
	coprimes []uint32
}

type randomEnum struct {
	i     uint32
	count uint32
	pos   uint32
	inc   uint32
}

func (ord *randomOrder) reset(count uint32) {
	ord.count = count
	ord.coprimes = ord.coprimes[:0]
	for i := uint32(1); i <= count; i++ {
		if gcd(i, count) == 1 {
			ord.coprimes = append(ord.coprimes, i)
		}
	}
}

func (ord *randomOrder) start(i uint32) randomEnum {
	return randomEnum{
		count: ord.count,
		pos:   i % ord.count,
		inc:   ord.coprimes[i%uint32(len(ord.coprimes))],
	}
}

func (enum *randomEnum) done() bool {
	return enum.i == enum.count
}

func (enum *randomEnum) next() {
	enum.i++
	enum.pos = (enum.pos + enum.inc) % enum.count
}

func (enum *randomEnum) position() uint32 {
	return enum.pos
}

// 求最大共约数
func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
