// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Should be a built-in for unsafe.Pointer?
// 是否应该内建到 unsafe.Pointer?
//go:nosplit
func add(p unsafe.Pointer, x uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(p) + x)
}

// getg returns the pointer to the current g.
// The compiler rewrites calls to this function into instructions
// that fetch the g directly (from TLS or from the dedicated register).
//  getg 返回指向当前 g 的指针。编译器将对此函数的调用重写为指令，从而直接获取 g（来自 TLS 或来自专用寄存器）
func getg() *g

// mcall switches from the g to the g0 stack and invokes fn(g),
// where g is the goroutine that made the call.
// mcall saves g's current PC/SP in g->sched so that it can be restored later.
// It is up to fn to arrange for that later execution, typically by recording
// g in a data structure, causing something to call ready(g) later.
// mcall returns to the original goroutine g later, when g has been rescheduled.
// fn must not return at all; typically it ends by calling schedule, to let the m
// run other goroutines.
//
// mcall can only be called from g stacks (not g0, not gsignal).
//
// This must NOT be go:noescape: if fn is a stack-allocated closure,
// fn puts g on a run queue, and g executes before fn returns, the
// closure will be invalidated while it is still executing.
//
//  mcall 从 g 切换到 g0 栈并调用 fn(g)，其中 g 为调用该方法的 goroutine。 mcall 保存了 g 在 g->sched 中当前的 PC/SP，进而可以在之后被恢复（调用goready）。
//  fn 通常通过在一个数据结构中记录 g 来安排随后的执行，从而为随后调用 ready(g)。当 g 被重新调度时， mcall 随后返回了原始的 goroutine g 。
//  fn 必须不能返回；通常以调用 schedule 结束，进而让 m 来运行其他的 goroutine。 mcall 只能从 g 栈中被调用（而非 g0 和 gsignal）。
// 一定不能是 go:noescape : 如果 fn 是一个栈分配的 closure，fn 将 g 放入运行队列中，且 g 在 fn 返回前执行，则 closure 将在它仍在执行时失效。
// 实现在 asm_amd64.s 
func mcall(fn func(*g))

// systemstack runs fn on a system stack.
// If systemstack is called from the per-OS-thread (g0) stack, or
// if systemstack is called from the signal handling (gsignal) stack,
// systemstack calls fn directly and returns.
// Otherwise, systemstack is being called from the limited stack
// of an ordinary goroutine. In this case, systemstack switches
// to the per-OS-thread stack, calls fn, and switches back.
// It is common to use a func literal as the argument, in order
// to share inputs and outputs with the code around the call
// to system stack:
// systemstack 在系统栈上运行 fn. 如果：systemstack 从 per-OS 线程 (g0) 栈上调用，或 systemstack 从信号处理 (gsignal) 栈上调用，
// 则 systemstack 直接调用 fn 并返回。否则 systemstack 会在一个普通的 goroutine 的有限栈上进行调用。这时，systemstack 会切换到 
//  per-OS-thread (g0) 栈上，然后调用 fn ，然后再切换回来。通常使用 func 字面量作为参数，以便与调用系统堆栈的代码共享输入和输出：
//
//	... set up y ...
//	systemstack(func() {
//		x = bigcall(y)
//	})
//	... use x ...
//
//go:noescape
func systemstack(fn func())

var badsystemstackMsg = "fatal: systemstack called from unexpected goroutine"

//go:nosplit
//go:nowritebarrierrec
func badsystemstack() {
	sp := stringStructOf(&badsystemstackMsg)
	write(2, sp.str, int32(sp.len))
}

// memclrNoHeapPointers clears n bytes starting at ptr.
//
// Usually you should use typedmemclr. memclrNoHeapPointers should be
// used only when the caller knows that *ptr contains no heap pointers
// because either:
//
// *ptr is initialized memory and its type is pointer-free, or
//
// *ptr is uninitialized memory (e.g., memory that's being reused
// for a new allocation) and hence contains only "junk".
//
// The (CPU-specific) implementations of this function are in memclr_*.s.
//
//  memclrNoHeapPointers 清除从 ptr 开始的 n 个字节。通常情况下你应该使用 typedmemclr，而 memclrNoHeapPointers 应该仅在调用方知道 *ptr 
// 不包含堆指针的情况下使用，因为 *ptr 只能是下面两种情况：
// 1. *ptr 是初始化过的内存，且其类型不是指针。
// 2. *ptr 是未初始化的内存（例如刚被新分配时使用的内存），则指包含 "junk" 垃圾内存
//  CPU 特定的实现参见 memclr_*.s
//go:noescape
func memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr)

// 函数链接，将reflect_memclrNoHeapPointers链接到reflect.memclrNoHeapPointers
//go:linkname reflect_memclrNoHeapPointers reflect.memclrNoHeapPointers
func reflect_memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr) {
	memclrNoHeapPointers(ptr, n)
}

// memmove copies n bytes from "from" to "to".
// in memmove_*.s
//  memmove 从 "from" 复制 n 字节到 "to" 
//go:noescape
func memmove(to, from unsafe.Pointer, n uintptr)

//go:linkname reflect_memmove reflect.memmove
func reflect_memmove(to, from unsafe.Pointer, n uintptr) {
	memmove(to, from, n)
}

// exported value for testing
var hashLoad = float32(loadFactorNum) / float32(loadFactorDen)

// 快速随机算法Xorshift
//go:nosplit
func fastrand() uint32 {
	mp := getg().m
	// Implement xorshift64+: 2 32-bit xorshift sequences added together.
	// Shift triplet [17,7,16] was calculated as indicated in Marsaglia's
	// Xorshift paper: https://www.jstatsoft.org/article/view/v008i14/xorshift.pdf
	// This generator passes the SmallCrush suite, part of TestU01 framework:
	// http://simul.iro.umontreal.ca/testu01/tu01.html
	s1, s0 := mp.fastrand[0], mp.fastrand[1]
	s1 ^= s1 << 17
	s1 = s1 ^ s0 ^ s1>>7 ^ s0>>16
	mp.fastrand[0], mp.fastrand[1] = s0, s1
	return s0 + s1
}

//go:nosplit
func fastrandn(n uint32) uint32 {
	// This is similar to fastrand() % n, but faster.
	// See https://lemire.me/blog/2016/06/27/a-fast-alternative-to-the-modulo-reduction/
	return uint32(uint64(fastrand()) * uint64(n) >> 32)
}

//go:linkname sync_fastrand sync.fastrand
func sync_fastrand() uint32 { return fastrand() }

// 内存数据是否相等
// in asm_*.s
//go:noescape
func memequal(a, b unsafe.Pointer, size uintptr) bool

// noescape hides a pointer from escape analysis.  noescape is
// the identity function but escape analysis doesn't think the
// output depends on the input.  noescape is inlined and currently
// compiles down to zero instructions.
// USE CAREFULLY!
// noescape在逃逸分析中隐藏了指针。noescape是标识函数，但是逃逸分析不认为输出取决于输入。noescape是内联的，当前可编译为零指令。小心使用！
//go:nosplit
func noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

// cgo回调函数
func cgocallback(fn, frame unsafe.Pointer, framesize, ctxt uintptr)

// 从gobuf恢复状态
func gogo(buf *gobuf)

// 保存调用者的状态
func gosave(buf *gobuf)

//go:noescape
func jmpdefer(fv *funcval, argp uintptr) // 跳到defer
func asminit()                           // 初始化汇编
func setg(gg *g)                         // 设置g
func breakpoint()                        // 断点 BYTE	$0xcc

// reflectcall calls fn with a copy of the n argument bytes pointed at by arg.
// After fn returns, reflectcall copies n-retoffset result bytes
// back into arg+retoffset before returning. If copying result bytes back,
// the caller should pass the argument frame type as argtype, so that
// call can execute appropriate write barriers during the copy.
// Package reflect passes a frame type. In package runtime, there is only
// one call that copies results back, in cgocallbackg1, and it does NOT pass a
// frame type, meaning there are no write barriers invoked. See that call
// site for justification.
//
// Package reflect accesses this symbol through a linkname.
// Reflectioncall调用fn，并复制arg指向的n个字节的参数。在fn返回之后，reflectcall将n-retoffset结果字节复制回arg + retoffset，然后再返回。
// 如果将结果字节复制回去，则调用方应将参数frame类型作为argtype传递，以便调用可以在复制期间执行适当的写障碍。reflect包传递frame类型。
// 在程序包运行时中，在cgocallbackg1中只有一个调用可以将结果复制回去，并且不会传递frame类型，这意味着不会调用任何写障碍。
func reflectcall(argtype *_type, fn, arg unsafe.Pointer, argsize uint32, retoffset uint32)

func procyield(cycles uint32) // 让出CPU

type neverCallThisFunction struct{}

// goexit is the return stub at the top of every goroutine call stack.
// Each goroutine stack is constructed as if goexit called the
// goroutine's entry point function, so that when the entry point
// function returns, it will return to goexit, which will call goexit1
// to perform the actual exit.
//
// This function must never be called directly. Call goexit1 instead.
// gentraceback assumes that goexit terminates the stack. A direct
// call on the stack will cause gentraceback to stop walking the stack
// prematurely and if there is leftover state it may panic.
// goexit是每个goroutine调用堆栈顶部的return存根。每个goroutine堆栈的构造就像goexit调用了goroutine的入口点函数一样，
// 因此，当入口点函数返回时，它将返回到goexit，后者将调用goexit1来执行真正的退出。绝对不能直接调用此函数。请调用goexit1。
// gentraceback假定goexit终止了堆栈。直接调用堆栈将导致gentraceback停止过早地遍历堆栈，如果存在剩余状态，则可能会恐慌。
func goexit(neverCallThisFunction)

// Not all cgocallback_gofunc frames are actually cgocallback_gofunc,
// so not all have these arguments. Mark them uintptr so that the GC
// does not misinterpret memory when the arguments are not present.
// cgocallback_gofunc is not called from go, only from cgocallback,
// so the arguments will be found via cgocallback's pointer-declared arguments.
// See the assembly implementations for more details.
// 并非所有cgocallback_gofunc框架实际上都是cgocallback_gofunc，因此并非所有人都具有这些参数。将它们标记为uintptr，
// 以便在不存在参数时GC不会误解内存。cgocallback_gofunc不是从go调用的，只能从cgocallback调用的，因此可以通过
// cgocallback的指针声明的参数找到。 有关更多详细信息，请参见程序集实现。
func cgocallback_gofunc(fv, frame, framesize, ctxt uintptr)

// publicationBarrier performs a store/store barrier (a "publication"
// or "export" barrier). Some form of synchronization is required
// between initializing an object and making that object accessible to
// another processor. Without synchronization, the initialization
// writes and the "publication" write may be reordered, allowing the
// other processor to follow the pointer and observe an uninitialized
// object. In general, higher-level synchronization should be used,
// such as locking or an atomic pointer write. publicationBarrier is
// for when those aren't an option, such as in the implementation of
// the memory manager.
//
// There's no corresponding barrier for the read side because the read
// side naturally has a data dependency order. All architectures that
// Go supports or seems likely to ever support automatically enforce
// data dependency ordering.
// publicationBarrier执行存储屏障（“发布”或“导出”屏障）。在初始化对象和使该对象可被另一个处理器访问之间，需要某种形式的同步。
// 如果没有同步，则初始化写入和“发布”写入可能会重新排序，从而使另一个处理器可以跟随指针并观察未初始化的对象。通常，应使用更高级
// 别的同步，例如锁定或原子指针写入。 publicationBarrier用于无法使用的可选项，例如在内存管理器的实现中。
// 读取侧没有相应的障碍，因为读取侧自然具有数据依赖顺序。Go支持或似乎曾经支持的所有体系结构都会自动执行数据依赖关系排序。
func publicationBarrier()

// getcallerpc returns the program counter (PC) of its caller's caller.
// getcallersp returns the stack pointer (SP) of its caller's caller.
// The implementation may be a compiler intrinsic; there is not
// necessarily code implementing this on every platform.
// getcallerpc返回其调用方的程序计数器（PC）。getcallersp返回其调用者的调用者的堆栈指针（SP）。该实现可以是编译器固有的，不一定在每个平台上都实现此代码。
//
// For example:
//
//	func f(arg1, arg2, arg3 int) {
//		pc := getcallerpc()
//		sp := getcallersp()
//	}
//
// These two lines find the PC and SP immediately following
// the call to f (where f will return).
// 这两行在调用f之后立即找到PC和SP（f将返回）。
//
// The call to getcallerpc and getcallersp must be done in the
// frame being asked about.
// 对getcallerpc和getcallersp的调用必须在支持的框架中完成。
//
// The result of getcallersp is correct at the time of the return,
// but it may be invalidated by any subsequent call to a function
// that might relocate the stack in order to grow or shrink it.
// A general rule is that the result of getcallersp should be used
// immediately and can only be passed to nosplit functions.
// getcallersp的结果在返回时是正确的，但是随后对函数的任何调用都可能使该结果无效，该函数可能会重新定位堆栈以增大或缩小堆栈。
// 一般规则是，应立即使用getcallersp的结果，并且只能将其传递给nosplit函数。

//go:noescape
func getcallerpc() uintptr

//go:noescape
func getcallersp() uintptr // implemented as an intrinsic on all platforms

// getclosureptr returns the pointer to the current closure.
// getclosureptr can only be used in an assignment statement
// at the entry of a function. Moreover, go:nosplit directive
// must be specified at the declaration of caller function,
// so that the function prolog does not clobber the closure register.
// getclosureptr返回指向当前闭包的指针。getclosureptr只能在函数入口处的赋值语句中使用。此外，必须在调用方函数的声明中
// 指定go：nosplit指令，以使函数prolog不会破坏闭包寄存器。
//
// for example:
//
//	//go:nosplit
//	func f(arg1, arg2, arg3 int) {
//		dx := getclosureptr()
//	}
//
// The compiler rewrites calls to this function into instructions that fetch the
// pointer from a well-known register (DX on x86 architecture, etc.) directly.
// 编译器将对该函数的调用重写为指令，这些指令可直接从知名寄存器（x86体系结构上的DX等）获取指针。
func getclosureptr() uintptr

//go:noescape
func asmcgocall(fn, arg unsafe.Pointer) int32 // 汇编cgo调用

// argp used in Defer structs when there is no argp.
// 没有参数时，在Defer结构中使用_NoArgs。
const _NoArgs = ^uintptr(0)

func morestack()        // 需要跟多的堆栈
func morestack_noctxt() // 需要跟多的堆栈，不保留ctxt
func rt0_go()           // 入口函数

// return0 is a stub used to return 0 from deferproc.
// It is called at the very end of deferproc to signal
// the calling Go function that it should not jump
// to deferreturn.
// in asm_*.s
// return0是一个存根，用于从deferproc返回0。它在deferproc的最后被调用，以向调用Go函数发出信号，告知它不应跳转到deferreturn
func return0()

// in asm_*.s
// not called directly; definitions here supply type information for traceback.
// 不直接调用；定义在这里用于支持traceback的类型信息
func call32(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call64(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call128(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call256(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call512(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1024(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2048(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4096(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8192(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16384(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call32768(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call65536(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call131072(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call262144(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call524288(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1048576(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2097152(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4194304(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8388608(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16777216(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call33554432(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call67108864(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call134217728(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call268435456(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call536870912(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1073741824(typ, fn, arg unsafe.Pointer, n, retoffset uint32)

func systemstack_switch()

// round n up to a multiple of a.  a must be a power of 2.
// 将n舍入为a的倍数。 a必须是2的幂。
func round(n, a uintptr) uintptr {
	return (n + a - 1) &^ (a - 1)
}

// checkASM reports whether assembly runtime checks have passed.
// checkASM报告汇编运行时是否检测通过
func checkASM() bool

// 内存数据是否相等
func memequal_varlen(a, b unsafe.Pointer) bool

// bool2int returns 0 if x is false or 1 if x is true.
// bool2int bool转int
func bool2int(x bool) int {
	// Avoid branches. In the SSA compiler, this compiles to
	// exactly what you would want it to.
	return int(uint8(*(*uint8)(unsafe.Pointer(&x))))
}

// abort crashes the runtime in situations where even throw might not
// work. In general it should do something a debugger will recognize
// (e.g., an INT3 on x86). A crash in abort is recognized by the
// signal handler, which will attempt to tear down the runtime
// immediately.
// 在抛出异常甚至都不起作用的情况下，abort会使运行时崩溃。通常，它应该执行调试程序可以识别的操作（例如，x86上的INT3）。
// 信号处理程序会识别中止中的崩溃，这将尝试立即中断运行时。 INT 3
func abort()
