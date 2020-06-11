// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix darwin dragonfly freebsd linux netbsd openbsd solaris

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// sigTabT is the type of an entry in the global sigtable array.
// sigtable is inherently system dependent, and appears in OS-specific files,
// but sigTabT is the same for all Unixy systems.
// The sigtable array is indexed by a system signal number to get the flags
// and printable name of each signal.
type sigTabT struct {
	flags int32
	name  string
}

//go:linkname os_sigpipe os.sigpipe
func os_sigpipe() {
	systemstack(sigpipe)
}

func signame(sig uint32) string {
	if sig >= uint32(len(sigtable)) {
		return ""
	}
	return sigtable[sig].name
}

const (
	_SIG_DFL uintptr = 0
	_SIG_IGN uintptr = 1
)

// Stores the signal handlers registered before Go installed its own.
// These signal handlers will be invoked in cases where Go doesn't want to
// handle a particular signal (e.g., signal occurred on a non-Go thread).
// See sigfwdgo for more information on when the signals are forwarded.
//
// This is read by the signal handler; accesses should use
// atomic.Loaduintptr and atomic.Storeuintptr.
var fwdSig [_NSIG]uintptr

// handlingSig is indexed by signal number and is non-zero if we are
// currently handling the signal. Or, to put it another way, whether
// the signal handler is currently set to the Go signal handler or not.
// This is uint32 rather than bool so that we can use atomic instructions.
var handlingSig [_NSIG]uint32

// channels for synchronizing signal mask updates with the signal mask
// thread
var (
	disableSigChan  chan uint32
	enableSigChan   chan uint32
	maskUpdatedChan chan struct{}
)

func init() {
	// _NSIG is the number of signals on this operating system.
	// sigtable should describe what to do for all the possible signals.
	if len(sigtable) != _NSIG {
		print("runtime: len(sigtable)=", len(sigtable), " _NSIG=", _NSIG, "\n")
		throw("bad sigtable len")
	}
}

var signalsOK bool

// Initialize signals.
// Called by libpreinit so runtime may not be initialized.
// 初始化信号。由 libpreinit 调用，因此 runtime 可能没有初始化。
// libpreinit 是c-archive/c-shared会调用的，会执行： initsig(true) 
//go:nosplit
//go:nowritebarrierrec
func initsig(preinit bool) {
	if !preinit {
		// It's now OK for signal handlers to run.
		// 已经提前初始化过了，所以 signal handler 可以直接运行
		signalsOK = true
	}

	// For c-archive/c-shared this is called by libpreinit with
	// preinit == true.
	// 对于 c-archive/c-shared ，这由libpreinit调用，其中 preinit == true。
	// 静态库/动态库直接返回
	if (isarchive || islibrary) && !preinit {
		return
	}

	// 遍历所有的信号掩码
	for i := uint32(0); i < _NSIG; i++ {
		t := &sigtable[i]
		if t.flags == 0 || t.flags&_SigDefault != 0 {
			continue
		}

		// We don't need to use atomic operations here because
		// there shouldn't be any other goroutines running yet.
		// 此时不需要原子操作，因为此时没有其他运行的 goroutine
		fwdSig[i] = getsig(i)

		if !sigInstallGoHandler(i) {
			// Even if we are not installing a signal handler,
			// set SA_ONSTACK if necessary.
			// 即使我们没有设置信号处理程序，也可以根据需要设置 SA_ONSTACK 。SA_ONSTACK 表示使用一个备用信号栈。
			// _SIG_DFL : 默认信号处理；_SIG_IGN ： 忽略信号处理。
			if fwdSig[i] != _SIG_DFL && fwdSig[i] != _SIG_IGN {
				setsigstack(i) // 设置备用信号处理，加上 _SA_ONSTACK 标记。
			} else if fwdSig[i] == _SIG_IGN {
				sigInitIgnored(i) // 忽略信号，sig.ignored 用 bit 表示是否忽略
			}
			continue
		}

		handlingSig[i] = 1
		setsig(i, funcPC(sighandler))
	}
}

//go:nosplit
//go:nowritebarrierrec
func sigInstallGoHandler(sig uint32) bool {
	// For some signals, we respect an inherited SIG_IGN handler
	// rather than insist on installing our own default handler.
	// Even these signals can be fetched using the os/signal package.
	switch sig {
	case _SIGHUP, _SIGINT:
		if atomic.Loaduintptr(&fwdSig[sig]) == _SIG_IGN {
			return false
		}
	}

	t := &sigtable[sig]
	if t.flags&_SigSetStack != 0 {
		return false
	}

	// When built using c-archive or c-shared, only install signal
	// handlers for synchronous signals and SIGPIPE.
	if (isarchive || islibrary) && t.flags&_SigPanic == 0 && sig != _SIGPIPE {
		return false
	}

	return true
}

// sigenable enables the Go signal handler to catch the signal sig.
// It is only called while holding the os/signal.handlers lock,
// via os/signal.enableSignal and signal_enable.
func sigenable(sig uint32) {
	if sig >= uint32(len(sigtable)) {
		return
	}

	// SIGPROF is handled specially for profiling.
	if sig == _SIGPROF {
		return
	}

	t := &sigtable[sig]
	if t.flags&_SigNotify != 0 {
		ensureSigM()
		enableSigChan <- sig
		<-maskUpdatedChan
		if atomic.Cas(&handlingSig[sig], 0, 1) {
			atomic.Storeuintptr(&fwdSig[sig], getsig(sig))
			setsig(sig, funcPC(sighandler))
		}
	}
}

// sigdisable disables the Go signal handler for the signal sig.
// It is only called while holding the os/signal.handlers lock,
// via os/signal.disableSignal and signal_disable.
func sigdisable(sig uint32) {
	if sig >= uint32(len(sigtable)) {
		return
	}

	// SIGPROF is handled specially for profiling.
	if sig == _SIGPROF {
		return
	}

	t := &sigtable[sig]
	if t.flags&_SigNotify != 0 {
		ensureSigM()
		disableSigChan <- sig
		<-maskUpdatedChan

		// If initsig does not install a signal handler for a
		// signal, then to go back to the state before Notify
		// we should remove the one we installed.
		if !sigInstallGoHandler(sig) {
			atomic.Store(&handlingSig[sig], 0)
			setsig(sig, atomic.Loaduintptr(&fwdSig[sig]))
		}
	}
}

// sigignore ignores the signal sig.
// It is only called while holding the os/signal.handlers lock,
// via os/signal.ignoreSignal and signal_ignore.
func sigignore(sig uint32) {
	if sig >= uint32(len(sigtable)) {
		return
	}

	// SIGPROF is handled specially for profiling.
	if sig == _SIGPROF {
		return
	}

	t := &sigtable[sig]
	if t.flags&_SigNotify != 0 {
		atomic.Store(&handlingSig[sig], 0)
		setsig(sig, _SIG_IGN)
	}
}

// clearSignalHandlers clears all signal handlers that are not ignored
// back to the default. This is called by the child after a fork, so that
// we can enable the signal mask for the exec without worrying about
// running a signal handler in the child.
//go:nosplit
//go:nowritebarrierrec
func clearSignalHandlers() {
	for i := uint32(0); i < _NSIG; i++ {
		if atomic.Load(&handlingSig[i]) != 0 {
			setsig(i, _SIG_DFL)
		}
	}
}

// setProcessCPUProfiler is called when the profiling timer changes.
// It is called with prof.lock held. hz is the new timer, and is 0 if
// profiling is being disabled. Enable or disable the signal as
// required for -buildmode=c-archive.
func setProcessCPUProfiler(hz int32) {
	if hz != 0 {
		// Enable the Go signal handler if not enabled.
		if atomic.Cas(&handlingSig[_SIGPROF], 0, 1) {
			atomic.Storeuintptr(&fwdSig[_SIGPROF], getsig(_SIGPROF))
			setsig(_SIGPROF, funcPC(sighandler))
		}
	} else {
		// If the Go signal handler should be disabled by default,
		// disable it if it is enabled.
		if !sigInstallGoHandler(_SIGPROF) {
			if atomic.Cas(&handlingSig[_SIGPROF], 1, 0) {
				setsig(_SIGPROF, atomic.Loaduintptr(&fwdSig[_SIGPROF]))
			}
		}
	}
}

// setThreadCPUProfiler makes any thread-specific changes required to
// implement profiling at a rate of hz.
//  setThreadCPUProfiler 以 hz 的速率进行实现分析所需的任何特定于线程的更改。
func setThreadCPUProfiler(hz int32) {
	var it itimerval
	if hz == 0 {
		setitimer(_ITIMER_PROF, &it, nil) // 清除计时器
	} else {
		//  it.it_interval 为间隔时间。it.it_value 为初次定时时间。
		it.it_interval.tv_sec = 0
		it.it_interval.set_usec(1000000 / hz)
		it.it_value = it.it_interval
		// 调用系统函数SYS_setittimer，设置定时器， ITIMER_PROF 以该进程在用户态下和内核态下所费的时间来计算，它送出 SIGPROF 信号。 见： sys_linux_amd64.s 
		//  SIGPROF 信号处理函数在 sighandler 中，然后调用 sigprof 函数， 见：signal_sighandler.go 
		setitimer(_ITIMER_PROF, &it, nil)
	}
	_g_ := getg()
	_g_.m.profilehz = hz
}

func sigpipe() {
	if sigsend(_SIGPIPE) {
		return
	}
	dieFromSignal(_SIGPIPE)
}

// sigtrampgo is called from the signal handler function, sigtramp,
// written in assembly code.
// This is called by the signal handler, and the world may be stopped.
//
// It must be nosplit because getg() is still the G that was running
// (if any) when the signal was delivered, but it's (usually) called
// on the gsignal stack. Until this switches the G to gsignal, the
// stack bounds check won't work.
//
//go:nosplit
//go:nowritebarrierrec
func sigtrampgo(sig uint32, info *siginfo, ctx unsafe.Pointer) {
	if sigfwdgo(sig, info, ctx) {
		return
	}
	g := getg()
	if g == nil {
		c := &sigctxt{info, ctx}
		if sig == _SIGPROF {
			sigprofNonGoPC(c.sigpc())
			return
		}
		badsignal(uintptr(sig), c)
		return
	}

	// If some non-Go code called sigaltstack, adjust.
	setStack := false
	var gsignalStack gsignalStack
	sp := uintptr(unsafe.Pointer(&sig))
	if sp < g.m.gsignal.stack.lo || sp >= g.m.gsignal.stack.hi {
		if sp >= g.m.g0.stack.lo && sp < g.m.g0.stack.hi {
			// The signal was delivered on the g0 stack.
			// This can happen when linked with C code
			// using the thread sanitizer, which collects
			// signals then delivers them itself by calling
			// the signal handler directly when C code,
			// including C code called via cgo, calls a
			// TSAN-intercepted function such as malloc.
			st := stackt{ss_size: g.m.g0.stack.hi - g.m.g0.stack.lo}
			setSignalstackSP(&st, g.m.g0.stack.lo)
			setGsignalStack(&st, &gsignalStack)
			g.m.gsignal.stktopsp = getcallersp()
			setStack = true
		} else {
			var st stackt
			sigaltstack(nil, &st)
			if st.ss_flags&_SS_DISABLE != 0 {
				setg(nil)
				needm(0)
				noSignalStack(sig)
				dropm()
			}
			stsp := uintptr(unsafe.Pointer(st.ss_sp))
			if sp < stsp || sp >= stsp+st.ss_size {
				setg(nil)
				needm(0)
				sigNotOnStack(sig)
				dropm()
			}
			setGsignalStack(&st, &gsignalStack)
			g.m.gsignal.stktopsp = getcallersp()
			setStack = true
		}
	}

	setg(g.m.gsignal)

	if g.stackguard0 == stackFork {
		signalDuringFork(sig)
	}

	c := &sigctxt{info, ctx}
	c.fixsigcode(sig)
	sighandler(sig, info, ctx, g)
	setg(g)
	if setStack {
		restoreGsignalStack(&gsignalStack)
	}
}

// sigpanic turns a synchronous signal into a run-time panic.
// If the signal handler sees a synchronous panic, it arranges the
// stack to look like the function where the signal occurred called
// sigpanic, sets the signal's PC value to sigpanic, and returns from
// the signal handler. The effect is that the program will act as
// though the function that got the signal simply called sigpanic
// instead.
//
// This must NOT be nosplit because the linker doesn't know where
// sigpanic calls can be injected.
//
// The signal handler must not inject a call to sigpanic if
// getg().throwsplit, since sigpanic may need to grow the stack.
func sigpanic() {
	g := getg()
	if !canpanic(g) {
		throw("unexpected signal during runtime execution")
	}

	switch g.sig {
	case _SIGBUS:
		if g.sigcode0 == _BUS_ADRERR && g.sigcode1 < 0x1000 {
			panicmem()
		}
		// Support runtime/debug.SetPanicOnFault.
		if g.paniconfault {
			panicmem()
		}
		print("unexpected fault address ", hex(g.sigcode1), "\n")
		throw("fault")
	case _SIGSEGV:
		if (g.sigcode0 == 0 || g.sigcode0 == _SEGV_MAPERR || g.sigcode0 == _SEGV_ACCERR) && g.sigcode1 < 0x1000 {
			panicmem()
		}
		// Support runtime/debug.SetPanicOnFault.
		if g.paniconfault {
			panicmem()
		}
		print("unexpected fault address ", hex(g.sigcode1), "\n")
		throw("fault")
	case _SIGFPE:
		switch g.sigcode0 {
		case _FPE_INTDIV:
			panicdivide()
		case _FPE_INTOVF:
			panicoverflow()
		}
		panicfloat()
	}

	if g.sig >= uint32(len(sigtable)) {
		// can't happen: we looked up g.sig in sigtable to decide to call sigpanic
		throw("unexpected signal value")
	}
	panic(errorString(sigtable[g.sig].name))
}

// dieFromSignal kills the program with a signal.
// This provides the expected exit status for the shell.
// This is only called with fatal signals expected to kill the process.
//go:nosplit
//go:nowritebarrierrec
func dieFromSignal(sig uint32) {
	unblocksig(sig)
	// Mark the signal as unhandled to ensure it is forwarded.
	atomic.Store(&handlingSig[sig], 0)
	raise(sig)

	// That should have killed us. On some systems, though, raise
	// sends the signal to the whole process rather than to just
	// the current thread, which means that the signal may not yet
	// have been delivered. Give other threads a chance to run and
	// pick up the signal.
	osyield()
	osyield()
	osyield()

	// If that didn't work, try _SIG_DFL.
	setsig(sig, _SIG_DFL)
	raise(sig)

	osyield()
	osyield()
	osyield()

	// If we are still somehow running, just exit with the wrong status.
	exit(2)
}

// raisebadsignal is called when a signal is received on a non-Go
// thread, and the Go program does not want to handle it (that is, the
// program has not called os/signal.Notify for the signal).
func raisebadsignal(sig uint32, c *sigctxt) {
	if sig == _SIGPROF {
		// Ignore profiling signals that arrive on non-Go threads.
		return
	}

	var handler uintptr
	if sig >= _NSIG {
		handler = _SIG_DFL
	} else {
		handler = atomic.Loaduintptr(&fwdSig[sig])
	}

	// Reset the signal handler and raise the signal.
	// We are currently running inside a signal handler, so the
	// signal is blocked. We need to unblock it before raising the
	// signal, or the signal we raise will be ignored until we return
	// from the signal handler. We know that the signal was unblocked
	// before entering the handler, or else we would not have received
	// it. That means that we don't have to worry about blocking it
	// again.
	unblocksig(sig)
	setsig(sig, handler)

	// If we're linked into a non-Go program we want to try to
	// avoid modifying the original context in which the signal
	// was raised. If the handler is the default, we know it
	// is non-recoverable, so we don't have to worry about
	// re-installing sighandler. At this point we can just
	// return and the signal will be re-raised and caught by
	// the default handler with the correct context.
	//
	// On FreeBSD, the libthr sigaction code prevents
	// this from working so we fall through to raise.
	if GOOS != "freebsd" && (isarchive || islibrary) && handler == _SIG_DFL && c.sigcode() != _SI_USER {
		return
	}

	raise(sig)

	// Give the signal a chance to be delivered.
	// In almost all real cases the program is about to crash,
	// so sleeping here is not a waste of time.
	usleep(1000)

	// If the signal didn't cause the program to exit, restore the
	// Go signal handler and carry on.
	//
	// We may receive another instance of the signal before we
	// restore the Go handler, but that is not so bad: we know
	// that the Go program has been ignoring the signal.
	setsig(sig, funcPC(sighandler))
}

//go:nosplit
func crash() {
	if GOOS == "darwin" {
		// OS X core dumps are linear dumps of the mapped memory,
		// from the first virtual byte to the last, with zeros in the gaps.
		// Because of the way we arrange the address space on 64-bit systems,
		// this means the OS X core file will be >128 GB and even on a zippy
		// workstation can take OS X well over an hour to write (uninterruptible).
		// Save users from making that mistake.
		if GOARCH == "amd64" {
			return
		}
	}

	dieFromSignal(_SIGABRT)
}

// ensureSigM starts one global, sleeping thread to make sure at least one thread
// is available to catch signals enabled for os/signal.
func ensureSigM() {
	if maskUpdatedChan != nil {
		return
	}
	maskUpdatedChan = make(chan struct{})
	disableSigChan = make(chan uint32)
	enableSigChan = make(chan uint32)
	go func() {
		// Signal masks are per-thread, so make sure this goroutine stays on one
		// thread.
		LockOSThread()
		defer UnlockOSThread()
		// The sigBlocked mask contains the signals not active for os/signal,
		// initially all signals except the essential. When signal.Notify()/Stop is called,
		// sigenable/sigdisable in turn notify this thread to update its signal
		// mask accordingly.
		sigBlocked := sigset_all
		for i := range sigtable {
			if !blockableSig(uint32(i)) {
				sigdelset(&sigBlocked, i)
			}
		}
		sigprocmask(_SIG_SETMASK, &sigBlocked, nil)
		for {
			select {
			case sig := <-enableSigChan:
				if sig > 0 {
					sigdelset(&sigBlocked, int(sig))
				}
			case sig := <-disableSigChan:
				if sig > 0 && blockableSig(sig) {
					sigaddset(&sigBlocked, int(sig))
				}
			}
			sigprocmask(_SIG_SETMASK, &sigBlocked, nil)
			maskUpdatedChan <- struct{}{}
		}
	}()
}

// This is called when we receive a signal when there is no signal stack.
// This can only happen if non-Go code calls sigaltstack to disable the
// signal stack.
func noSignalStack(sig uint32) {
	println("signal", sig, "received on thread with no signal stack")
	throw("non-Go code disabled sigaltstack")
}

// This is called if we receive a signal when there is a signal stack
// but we are not on it. This can only happen if non-Go code called
// sigaction without setting the SS_ONSTACK flag.
func sigNotOnStack(sig uint32) {
	println("signal", sig, "received but handler not on signal stack")
	throw("non-Go code set up signal handler without SA_ONSTACK flag")
}

// signalDuringFork is called if we receive a signal while doing a fork.
// We do not want signals at that time, as a signal sent to the process
// group may be delivered to the child process, causing confusion.
// This should never be called, because we block signals across the fork;
// this function is just a safety check. See issue 18600 for background.
func signalDuringFork(sig uint32) {
	println("signal", sig, "received during fork")
	throw("signal received during fork")
}

// This runs on a foreign stack, without an m or a g. No stack split.
//go:nosplit
//go:norace
//go:nowritebarrierrec
func badsignal(sig uintptr, c *sigctxt) {
	needm(0)
	if !sigsend(uint32(sig)) {
		// A foreign thread received the signal sig, and the
		// Go code does not want to handle it.
		raisebadsignal(uint32(sig), c)
	}
	dropm()
}

//go:noescape
func sigfwd(fn uintptr, sig uint32, info *siginfo, ctx unsafe.Pointer)

// Determines if the signal should be handled by Go and if not, forwards the
// signal to the handler that was installed before Go's. Returns whether the
// signal was forwarded.
// This is called by the signal handler, and the world may be stopped.
//go:nosplit
//go:nowritebarrierrec
func sigfwdgo(sig uint32, info *siginfo, ctx unsafe.Pointer) bool {
	if sig >= uint32(len(sigtable)) {
		return false
	}
	fwdFn := atomic.Loaduintptr(&fwdSig[sig])
	flags := sigtable[sig].flags

	// If we aren't handling the signal, forward it.
	if atomic.Load(&handlingSig[sig]) == 0 || !signalsOK {
		// If the signal is ignored, doing nothing is the same as forwarding.
		if fwdFn == _SIG_IGN || (fwdFn == _SIG_DFL && flags&_SigIgn != 0) {
			return true
		}
		// We are not handling the signal and there is no other handler to forward to.
		// Crash with the default behavior.
		if fwdFn == _SIG_DFL {
			setsig(sig, _SIG_DFL)
			dieFromSignal(sig)
			return false
		}

		sigfwd(fwdFn, sig, info, ctx)
		return true
	}

	// If there is no handler to forward to, no need to forward.
	if fwdFn == _SIG_DFL {
		return false
	}

	c := &sigctxt{info, ctx}
	// Only forward synchronous signals and SIGPIPE.
	// Unfortunately, user generated SIGPIPEs will also be forwarded, because si_code
	// is set to _SI_USER even for a SIGPIPE raised from a write to a closed socket
	// or pipe.
	if (c.sigcode() == _SI_USER || flags&_SigPanic == 0) && sig != _SIGPIPE {
		return false
	}
	// Determine if the signal occurred inside Go code. We test that:
	//   (1) we were in a goroutine (i.e., m.curg != nil), and
	//   (2) we weren't in CGO.
	g := getg()
	if g != nil && g.m != nil && g.m.curg != nil && !g.m.incgo {
		return false
	}

	// Signal not handled by Go, forward it.
	if fwdFn != _SIG_IGN {
		sigfwd(fwdFn, sig, info, ctx)
	}

	return true
}

// msigsave saves the current thread's signal mask into mp.sigmask.
// This is used to preserve the non-Go signal mask when a non-Go
// thread calls a Go function.
// This is nosplit and nowritebarrierrec because it is called by needm
// which may be called on a non-Go thread with no g available.
//go:nosplit
//go:nowritebarrierrec
func msigsave(mp *m) {
	sigprocmask(_SIG_SETMASK, nil, &mp.sigmask)
}

// msigrestore sets the current thread's signal mask to sigmask.
// This is used to restore the non-Go signal mask when a non-Go thread
// calls a Go function.
// This is nosplit and nowritebarrierrec because it is called by dropm
// after g has been cleared.
//go:nosplit
//go:nowritebarrierrec
func msigrestore(sigmask sigset) {
	sigprocmask(_SIG_SETMASK, &sigmask, nil)
}

// sigblock blocks all signals in the current thread's signal mask.
// This is used to block signals while setting up and tearing down g
// when a non-Go thread calls a Go function.
// The OS-specific code is expected to define sigset_all.
// This is nosplit and nowritebarrierrec because it is called by needm
// which may be called on a non-Go thread with no g available.
//go:nosplit
//go:nowritebarrierrec
func sigblock() {
	sigprocmask(_SIG_SETMASK, &sigset_all, nil)
}

// unblocksig removes sig from the current thread's signal mask.
// This is nosplit and nowritebarrierrec because it is called from
// dieFromSignal, which can be called by sigfwdgo while running in the
// signal handler, on the signal stack, with no g available.
//go:nosplit
//go:nowritebarrierrec
func unblocksig(sig uint32) {
	var set sigset
	sigaddset(&set, int(sig))
	sigprocmask(_SIG_UNBLOCK, &set, nil)
}

// minitSignals is called when initializing a new m to set the
// thread's alternate signal stack and signal mask.
// 初始化新的 m 调用 minitSignals 来设置线程所需要的 alternate signal stack （备用信号栈）和 mask 时。
func minitSignals() {
	minitSignalStack()
	minitSignalMask()
}

// minitSignalStack is called when initializing a new m to set the
// alternate signal stack. If the alternate signal stack is not set
// for the thread (the normal case) then set the alternate signal
// stack to the gsignal stack. If the alternate signal stack is set
// for the thread (the case when a non-Go thread sets the alternate
// signal stack and then calls a Go function) then set the gsignal
// stack to the alternate signal stack. Record which choice was made
// in newSigstack, so that it can be undone in unminit.
// 初始化新 m 以设置备用信号栈时调用 minitSignalStack。如果没有为线程设置备用信号栈（正常情况），则将备用信号栈设置为 gsignal 栈。
// 如果为线程设置了备用信号栈（非 Go 线程设置备用信号栈然后调用 Go 函数的情况）， 则将 gsignal 栈设置为备用信号栈。如果没有使用 cgo 
// 我们还设置了额外的 gsignal 信号栈（无论其是否已经被设置），则记录在 newSigstack 中做出的选择，以便可以在 unminit 中撤消。
func minitSignalStack() {
	_g_ := getg()
	var st stackt
	// sigaltstack 为系统调用( int sigaltstack(const stack_t *ss, stack_t *old_ss); )，在 sys_linux_amd64.s 中
	// sigaltstack 是用于 设置/获取 专门的信号处理函数使用的栈空间的。
	sigaltstack(nil, &st) // 获取备用信号栈
	// 检测备用信号栈是否被禁用了
	if st.ss_flags&_SS_DISABLE != 0 {
		// 将 gsignal 的执行栈设置为备用信号栈
		signalstack(&_g_.m.gsignal.stack)
		_g_.m.newSigstack = true
	} else {
		// 否则将 m 的 gsignal 栈设置为从 sigaltstack 返回的备用信号栈
		setGsignalStack(&st, &_g_.m.goSigStack)
		_g_.m.newSigstack = false
	}
}

// minitSignalMask is called when initializing a new m to set the
// thread's signal mask. When this is called all signals have been
// blocked for the thread.  This starts with m.sigmask, which was set
// either from initSigmask for a newly created thread or by calling
// msigsave if this is a non-Go thread calling a Go function. It
// removes all essential signals from the mask, thus causing those
// signals to not be blocked. Then it sets the thread's signal mask.
// After this is called the thread can receive signals.
// 调用 minitSignalMask 为新的 m 设置信号掩码。当此函数调用时，这个线程的所有信号已经被阻塞。m.sigmask 是 通过 initSigmask 为新线程设置 或
// 在非GO调用GO情况下调用 msigsave 设置。从 m.sigmask 中删除信号掩码中所有的必要的信号，从而导致这些信号不会阻塞。然后设置这个线程的信号掩码，
// 调用此函数后，线程可以接受信号了。
func minitSignalMask() {
	nmask := getg().m.sigmask
	// 遍历 sigtable ， 删除不阻塞的信号
	for i := range sigtable {
		if !blockableSig(uint32(i)) {
			sigdelset(&nmask, i)
		}
	}
	sigprocmask(_SIG_SETMASK, &nmask, nil)
}

// unminitSignals is called from dropm, via unminit, to undo the
// effect of calling minit on a non-Go thread.
//go:nosplit
func unminitSignals() {
	if getg().m.newSigstack {
		st := stackt{ss_flags: _SS_DISABLE}
		sigaltstack(&st, nil)
	} else {
		// We got the signal stack from someone else. Restore
		// the Go-allocated stack in case this M gets reused
		// for another thread (e.g., it's an extram). Also, on
		// Android, libc allocates a signal stack for all
		// threads, so it's important to restore the Go stack
		// even on Go-created threads so we can free it.
		restoreGsignalStack(&getg().m.goSigStack)
	}
}

// blockableSig reports whether sig may be blocked by the signal mask.
// We never want to block the signals marked _SigUnblock;
// these are the synchronous signals that turn into a Go panic.
// In a Go program--not a c-archive/c-shared--we never want to block
// the signals marked _SigKill or _SigThrow, as otherwise it's possible
// for all running threads to block them and delay their delivery until
// we start a new thread. When linked into a C program we let the C code
// decide on the disposition of those signals.
// blockableSig 报告信号掩码是否可能阻塞。我们永远不会阻塞标记为 _SigUnblock 的信号；这些都是同步信号（跟当前的执行者一起执行缺出了状况所发出的 signal，
// 比如 SIGILL (Illegal Instruction : 非法指令) 和 SIGTRAP (Trace/breakpoint trap) ），而让 GO 语言的处理方法会使之成为 panic 。在Go程序中， 而不是
//  c-archive/c-shared ，我们从不希望阻塞标记为 _SigKill 或 _SigThrow 的信号，否则所有正在运行的线程都可以阻塞它们并延迟其交付，直到我们开始一个新线程为止。
// 当链接到C程序时，我们让 C 代码决定这些信号的配置。
func blockableSig(sig uint32) bool {
	flags := sigtable[sig].flags
	if flags&_SigUnblock != 0 { // _SigUnblock 不阻塞
		return false
	}
	if isarchive || islibrary { // c-archive/c-shared 阻塞
		return true
	}
	return flags&(_SigKill|_SigThrow) == 0 // _SigKill 或者 _SigThrow 不阻塞，其他阻塞
}

// gsignalStack saves the fields of the gsignal stack changed by
// setGsignalStack.
//  gsignalStack 保存由 setGsignalStack 更改的 gsignal 堆栈的字段。
type gsignalStack struct {
	stack       stack
	stackguard0 uintptr
	stackguard1 uintptr
	stktopsp    uintptr
}

// setGsignalStack sets the gsignal stack of the current m to an
// alternate signal stack returned from the sigaltstack system call.
// It saves the old values in *old for use by restoreGsignalStack.
// This is used when handling a signal if non-Go code has set the
// alternate signal stack.
// setGsignalStack 将当前 m 的 gsignal 栈设置为从 sigaltstack 系统调用返回的备用信号堆栈。它将旧值保存在 *old 中以供 restoreGsignalStack 使用。
// 这个函数用在当不是 Go 代码设置了备用信号站的情况。
//go:nosplit
//go:nowritebarrierrec
func setGsignalStack(st *stackt, old *gsignalStack) {
	g := getg()
	if old != nil {
		old.stack = g.m.gsignal.stack
		old.stackguard0 = g.m.gsignal.stackguard0
		old.stackguard1 = g.m.gsignal.stackguard1
		old.stktopsp = g.m.gsignal.stktopsp
	}
	stsp := uintptr(unsafe.Pointer(st.ss_sp))
	g.m.gsignal.stack.lo = stsp
	g.m.gsignal.stack.hi = stsp + st.ss_size
	g.m.gsignal.stackguard0 = stsp + _StackGuard
	g.m.gsignal.stackguard1 = stsp + _StackGuard
}

// restoreGsignalStack restores the gsignal stack to the value it had
// before entering the signal handler.
//go:nosplit
//go:nowritebarrierrec
func restoreGsignalStack(st *gsignalStack) {
	gp := getg().m.gsignal
	gp.stack = st.stack
	gp.stackguard0 = st.stackguard0
	gp.stackguard1 = st.stackguard1
	gp.stktopsp = st.stktopsp
}

// signalstack sets the current thread's alternate signal stack to s.
// signalstack 设置当前线程的额外信号栈 G 
//go:nosplit
func signalstack(s *stack) {
	st := stackt{ss_size: s.hi - s.lo} // 设置栈大小
	setSignalstackSP(&st, s.lo)        // 设置栈SP
	sigaltstack(&st, nil)              // 设置备用信号栈
}

// setsigsegv is used on darwin/arm{,64} to fake a segmentation fault.
//go:nosplit
func setsigsegv(pc uintptr) {
	g := getg()
	g.sig = _SIGSEGV
	g.sigpc = pc
	g.sigcode0 = _SEGV_MAPERR
	g.sigcode1 = 0 // TODO: emulate si_addr
}
