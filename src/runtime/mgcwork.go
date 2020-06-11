// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	_WorkbufSize = 2048 // in bytes; larger values result in less contention

	// workbufAlloc is the number of bytes to allocate at a time
	// for new workbufs. This must be a multiple of pageSize and
	// should be a multiple of _WorkbufSize.
	//
	// Larger values reduce workbuf allocation overhead. Smaller
	// values reduce heap fragmentation.
	workbufAlloc = 32 << 10
)

// throwOnGCWork causes any operations that add pointers to a gcWork
// buffer to throw.
//
// TODO(austin): This is a temporary debugging measure for issue
// #27993. To be removed before release.
var throwOnGCWork bool

func init() {
	if workbufAlloc%pageSize != 0 || workbufAlloc%_WorkbufSize != 0 {
		throw("bad workbufAlloc")
	}
}

// Garbage collector work pool abstraction.
//
// This implements a producer/consumer model for pointers to grey
// objects. A grey object is one that is marked and on a work
// queue. A black object is marked and not on a work queue.
//
// Write barriers, root discovery, stack scanning, and object scanning
// produce pointers to grey objects. Scanning consumes pointers to
// grey objects, thus blackening them, and then scans them,
// potentially producing new pointers to grey objects.
// GC工作池抽象
// 这实现了生产者/消费者模型来置灰对象。灰色对象是已标记并在工作队列中的对象。黑色对象被标记并且不在工作队列上。
// 写障碍，根发现，堆栈扫描和对象扫描产生指向灰色对象的指针。扫描会消耗指向灰色对象的指针，从而使它们变黑，然后对其进行扫描，从而有可能产生指向灰色对象的新指针。

// A gcWork provides the interface to produce and consume work for the
// garbage collector.
//
// A gcWork can be used on the stack as follows:
//
//     (preemption must be disabled)
//     gcw := &getg().m.p.ptr().gcw
//     .. call gcw.put() to produce and gcw.tryGet() to consume ..
//
// It's important that any use of gcWork during the mark phase prevent
// the garbage collector from transitioning to mark termination since
// gcWork may locally hold GC work buffers. This can be done by
// disabling preemption (systemstack or acquirem).
// 重要的是，在标记阶段使用gcWork可以防止垃圾回收器过渡到标记终止，因为gcWork可能在本地保留GC工作缓冲区。这可以通过禁用抢占（systemstack或acquirem）来完成。
type gcWork struct {
	// wbuf1 and wbuf2 are the primary and secondary work buffers.
	//
	// This can be thought of as a stack of both work buffers'
	// pointers concatenated. When we pop the last pointer, we
	// shift the stack up by one work buffer by bringing in a new
	// full buffer and discarding an empty one. When we fill both
	// buffers, we shift the stack down by one work buffer by
	// bringing in a new empty buffer and discarding a full one.
	// This way we have one buffer's worth of hysteresis, which
	// amortizes the cost of getting or putting a work buffer over
	// at least one buffer of work and reduces contention on the
	// global work lists.
	//
	// wbuf1 is always the buffer we're currently pushing to and
	// popping from and wbuf2 is the buffer that will be discarded
	// next.
	//
	// Invariant: Both wbuf1 and wbuf2 are nil or neither are.
	// wbuf1和wbuf2是主要和辅助工作缓冲区。
	// 可以认为这是两个工作缓冲区的指针串联在一起的堆栈。当我们弹出最后一个指针时，我们通过引入一个新的完整缓冲区并丢弃一个空缓冲区，将堆栈向上移动一个工作缓冲区。
	// 当我们填充两个缓冲区时，我们通过引入一个新的空缓冲区并丢弃一个完整的缓冲区，将堆栈向下移动一个工作缓冲区。这样，我们就有了一个缓冲区的滞后值，这可以分摊在至少
	// 一个工作缓冲区上获得或放置工作缓冲区的成本，并减少全局工作列表上的竞争。
	// wbuf1始终是我们当前要推送到并从中弹出的缓冲区，而wbuf2是接下来将被丢弃的缓冲区。
	// 不变式：wbuf1和wbuf2均为nil或都不为nil。
	wbuf1, wbuf2 *workbuf

	// Bytes marked (blackened) on this gcWork. This is aggregated
	// into work.bytesMarked by dispose.
	// 在此gcWork上标记（涂黑）的字节。通过dispose方将其汇总到work.bytes中。
	bytesMarked uint64

	// Scan work performed on this gcWork. This is aggregated into
	// gcController by dispose and may also be flushed by callers.
	// 扫描在此gcWork上执行的工作。 通过dispose方法将其汇总到gcController中，也可以由调用方将其刷新。
	scanWork int64

	// flushedWork indicates that a non-empty work buffer was
	// flushed to the global work list since the last gcMarkDone
	// termination check. Specifically, this indicates that this
	// gcWork may have communicated work to another gcWork.
	// flushedWork指示自上一次gcMarkDone终止检查以来，非空工作缓冲区已刷新到全局工作列表。具体来说，这表明此gcWork可能已经将工作传达给了另一个gcWork。
	flushedWork bool

	// pauseGen causes put operations to spin while pauseGen ==
	// gcWorkPauseGen if debugCachedWork is true.
	// 如果debugCachedWork为true，当pauseGen == gcWorkPauseGen，则pauseGen会导操作自旋。
	pauseGen uint32

	// putGen is the pauseGen of the last putGen.
	// putGen是最后一个putGen的pauseGen。
	putGen uint32

	// pauseStack is the stack at which this P was paused if
	// debugCachedWork is true.
	// 如果debugCachedWork为true，pauseStack是此P暂停的堆栈。
	pauseStack [16]uintptr
}

// Most of the methods of gcWork are go:nowritebarrierrec because the
// write barrier itself can invoke gcWork methods but the methods are
// not generally re-entrant. Hence, if a gcWork method invoked the
// write barrier while the gcWork was in an inconsistent state, and
// the write barrier in turn invoked a gcWork method, it could
// permanently corrupt the gcWork.
// gcWork的大多数方法都是go:nowritebarrierrec，因为写屏障本身可以调用gcWork方法，但这些方法通常不能重入。因此，
// 如果在gcWork处于不一致状态时gcWork方法调用了写屏障，而写屏障又又调用了gcWork方法，则它可能会永久破坏gcWork。
// go:nowritebarrierrec => 如果函数包含 write barrier，则 go:nowritebarrier 触发一个编译器错误（它不会抑制 write barrier 的产生，只是一个断言）。

func (w *gcWork) init() {
	w.wbuf1 = getempty()
	wbuf2 := trygetfull()
	if wbuf2 == nil {
		wbuf2 = getempty()
	}
	w.wbuf2 = wbuf2
}

func (w *gcWork) checkPut(ptr uintptr, ptrs []uintptr) {
	if debugCachedWork {
		alreadyFailed := w.putGen == w.pauseGen
		w.putGen = w.pauseGen
		if m := getg().m; m.locks > 0 || m.mallocing != 0 || m.preemptoff != "" || m.p.ptr().status != _Prunning {
			// If we were to spin, the runtime may
			// deadlock: the condition above prevents
			// preemption (see newstack), which could
			// prevent gcMarkDone from finishing the
			// ragged barrier and releasing the spin.
			return
		}
		for atomic.Load(&gcWorkPauseGen) == w.pauseGen {
		}
		if throwOnGCWork {
			printlock()
			if alreadyFailed {
				println("runtime: checkPut already failed at this generation")
			}
			println("runtime: late gcWork put")
			if ptr != 0 {
				gcDumpObject("ptr", ptr, ^uintptr(0))
			}
			for _, ptr := range ptrs {
				gcDumpObject("ptrs", ptr, ^uintptr(0))
			}
			println("runtime: paused at")
			for _, pc := range w.pauseStack {
				if pc == 0 {
					break
				}
				f := findfunc(pc)
				if f.valid() {
					// Obviously this doesn't
					// relate to ancestor
					// tracebacks, but this
					// function prints what we
					// want.
					printAncestorTracebackFuncInfo(f, pc)
				} else {
					println("\tunknown PC ", hex(pc), "\n")
				}
			}
			throw("throwOnGCWork")
		}
	}
}

// put enqueues a pointer for the garbage collector to trace.
// obj must point to the beginning of a heap object or an oblet.
//go:nowritebarrierrec
func (w *gcWork) put(obj uintptr) {
	w.checkPut(obj, nil)

	flushed := false
	wbuf := w.wbuf1
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
		// wbuf is empty at this point.
	} else if wbuf.nobj == len(wbuf.obj) {
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1
		wbuf = w.wbuf1
		if wbuf.nobj == len(wbuf.obj) {
			putfull(wbuf)
			w.flushedWork = true
			wbuf = getempty()
			w.wbuf1 = wbuf
			flushed = true
		}
	}

	wbuf.obj[wbuf.nobj] = obj
	wbuf.nobj++

	// If we put a buffer on full, let the GC controller know so
	// it can encourage more workers to run. We delay this until
	// the end of put so that w is in a consistent state, since
	// enlistWorker may itself manipulate w.
	if flushed && gcphase == _GCmark {
		gcController.enlistWorker()
	}
}

// putFast does a put and reports whether it can be done quickly
// otherwise it returns false and the caller needs to call put.
//go:nowritebarrierrec
func (w *gcWork) putFast(obj uintptr) bool {
	w.checkPut(obj, nil)

	wbuf := w.wbuf1
	if wbuf == nil {
		return false
	} else if wbuf.nobj == len(wbuf.obj) {
		return false
	}

	wbuf.obj[wbuf.nobj] = obj
	wbuf.nobj++
	return true
}

// putBatch performs a put on every pointer in obj. See put for
// constraints on these pointers.
//
//go:nowritebarrierrec
func (w *gcWork) putBatch(obj []uintptr) {
	if len(obj) == 0 {
		return
	}

	w.checkPut(0, obj)

	flushed := false
	wbuf := w.wbuf1
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
	}

	for len(obj) > 0 {
		for wbuf.nobj == len(wbuf.obj) {
			putfull(wbuf)
			w.flushedWork = true
			w.wbuf1, w.wbuf2 = w.wbuf2, getempty()
			wbuf = w.wbuf1
			flushed = true
		}
		n := copy(wbuf.obj[wbuf.nobj:], obj)
		wbuf.nobj += n
		obj = obj[n:]
	}

	if flushed && gcphase == _GCmark {
		gcController.enlistWorker()
	}
}

// tryGet dequeues a pointer for the garbage collector to trace.
//
// If there are no pointers remaining in this gcWork or in the global
// queue, tryGet returns 0.  Note that there may still be pointers in
// other gcWork instances or other caches.
//go:nowritebarrierrec
func (w *gcWork) tryGet() uintptr {
	wbuf := w.wbuf1
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
		// wbuf is empty at this point.
	}
	if wbuf.nobj == 0 {
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1
		wbuf = w.wbuf1
		if wbuf.nobj == 0 {
			owbuf := wbuf
			wbuf = trygetfull()
			if wbuf == nil {
				return 0
			}
			putempty(owbuf)
			w.wbuf1 = wbuf
		}
	}

	wbuf.nobj--
	return wbuf.obj[wbuf.nobj]
}

// tryGetFast dequeues a pointer for the garbage collector to trace
// if one is readily available. Otherwise it returns 0 and
// the caller is expected to call tryGet().
//go:nowritebarrierrec
func (w *gcWork) tryGetFast() uintptr {
	wbuf := w.wbuf1
	if wbuf == nil {
		return 0
	}
	if wbuf.nobj == 0 {
		return 0
	}

	wbuf.nobj--
	return wbuf.obj[wbuf.nobj]
}

// dispose returns any cached pointers to the global queue.
// The buffers are being put on the full queue so that the
// write barriers will not simply reacquire them before the
// GC can inspect them. This helps reduce the mutator's
// ability to hide pointers during the concurrent mark phase.
//
//go:nowritebarrierrec
func (w *gcWork) dispose() {
	if wbuf := w.wbuf1; wbuf != nil {
		if wbuf.nobj == 0 {
			putempty(wbuf)
		} else {
			putfull(wbuf)
			w.flushedWork = true
		}
		w.wbuf1 = nil

		wbuf = w.wbuf2
		if wbuf.nobj == 0 {
			putempty(wbuf)
		} else {
			putfull(wbuf)
			w.flushedWork = true
		}
		w.wbuf2 = nil
	}
	if w.bytesMarked != 0 {
		// dispose happens relatively infrequently. If this
		// atomic becomes a problem, we should first try to
		// dispose less and if necessary aggregate in a per-P
		// counter.
		atomic.Xadd64(&work.bytesMarked, int64(w.bytesMarked))
		w.bytesMarked = 0
	}
	if w.scanWork != 0 {
		atomic.Xaddint64(&gcController.scanWork, w.scanWork)
		w.scanWork = 0
	}
}

// balance moves some work that's cached in this gcWork back on the
// global queue.
//go:nowritebarrierrec
func (w *gcWork) balance() {
	if w.wbuf1 == nil {
		return
	}
	if wbuf := w.wbuf2; wbuf.nobj != 0 {
		w.checkPut(0, wbuf.obj[:wbuf.nobj])
		putfull(wbuf)
		w.flushedWork = true
		w.wbuf2 = getempty()
	} else if wbuf := w.wbuf1; wbuf.nobj > 4 {
		w.checkPut(0, wbuf.obj[:wbuf.nobj])
		w.wbuf1 = handoff(wbuf)
		w.flushedWork = true // handoff did putfull
	} else {
		return
	}
	// We flushed a buffer to the full list, so wake a worker.
	if gcphase == _GCmark {
		gcController.enlistWorker()
	}
}

// empty reports whether w has no mark work available.
//go:nowritebarrierrec
func (w *gcWork) empty() bool {
	return w.wbuf1 == nil || (w.wbuf1.nobj == 0 && w.wbuf2.nobj == 0)
}

// Internally, the GC work pool is kept in arrays in work buffers.
// The gcWork interface caches a work buffer until full (or empty) to
// avoid contending on the global work buffer lists.

type workbufhdr struct {
	node lfnode // must be first
	nobj int
}

//go:notinheap
type workbuf struct {
	workbufhdr
	// account for the above fields
	obj [(_WorkbufSize - unsafe.Sizeof(workbufhdr{})) / sys.PtrSize]uintptr
}

// workbuf factory routines. These funcs are used to manage the
// workbufs.
// If the GC asks for some work these are the only routines that
// make wbufs available to the GC.

func (b *workbuf) checknonempty() {
	if b.nobj == 0 {
		throw("workbuf is empty")
	}
}

func (b *workbuf) checkempty() {
	if b.nobj != 0 {
		throw("workbuf is not empty")
	}
}

// getempty pops an empty work buffer off the work.empty list,
// allocating new buffers if none are available.
//go:nowritebarrier
func getempty() *workbuf {
	var b *workbuf
	if work.empty != 0 {
		b = (*workbuf)(work.empty.pop())
		if b != nil {
			b.checkempty()
		}
	}
	if b == nil {
		// Allocate more workbufs.
		var s *mspan
		if work.wbufSpans.free.first != nil {
			lock(&work.wbufSpans.lock)
			s = work.wbufSpans.free.first
			if s != nil {
				work.wbufSpans.free.remove(s)
				work.wbufSpans.busy.insert(s)
			}
			unlock(&work.wbufSpans.lock)
		}
		if s == nil {
			systemstack(func() {
				s = mheap_.allocManual(workbufAlloc/pageSize, &memstats.gc_sys)
			})
			if s == nil {
				throw("out of memory")
			}
			// Record the new span in the busy list.
			lock(&work.wbufSpans.lock)
			work.wbufSpans.busy.insert(s)
			unlock(&work.wbufSpans.lock)
		}
		// Slice up the span into new workbufs. Return one and
		// put the rest on the empty list.
		for i := uintptr(0); i+_WorkbufSize <= workbufAlloc; i += _WorkbufSize {
			newb := (*workbuf)(unsafe.Pointer(s.base() + i))
			newb.nobj = 0
			lfnodeValidate(&newb.node)
			if i == 0 {
				b = newb
			} else {
				putempty(newb)
			}
		}
	}
	return b
}

// putempty puts a workbuf onto the work.empty list.
// Upon entry this go routine owns b. The lfstack.push relinquishes ownership.
//go:nowritebarrier
func putempty(b *workbuf) {
	b.checkempty()
	work.empty.push(&b.node)
}

// putfull puts the workbuf on the work.full list for the GC.
// putfull accepts partially full buffers so the GC can avoid competing
// with the mutators for ownership of partially full buffers.
//go:nowritebarrier
func putfull(b *workbuf) {
	b.checknonempty()
	work.full.push(&b.node)
}

// trygetfull tries to get a full or partially empty workbuffer.
// If one is not immediately available return nil
//go:nowritebarrier
func trygetfull() *workbuf {
	b := (*workbuf)(work.full.pop())
	if b != nil {
		b.checknonempty()
		return b
	}
	return b
}

//go:nowritebarrier
func handoff(b *workbuf) *workbuf {
	// Make new buffer with half of b's pointers.
	b1 := getempty()
	n := b.nobj / 2
	b.nobj -= n
	b1.nobj = n
	memmove(unsafe.Pointer(&b1.obj[0]), unsafe.Pointer(&b.obj[b.nobj]), uintptr(n)*unsafe.Sizeof(b1.obj[0]))

	// Put b on full list - let first half of b get stolen.
	putfull(b)
	return b1
}

// prepareFreeWorkbufs moves busy workbuf spans to free list so they
// can be freed to the heap. This must only be called when all
// workbufs are on the empty list.
func prepareFreeWorkbufs() {
	lock(&work.wbufSpans.lock)
	if work.full != 0 {
		throw("cannot free workbufs when work.full != 0")
	}
	// Since all workbufs are on the empty list, we don't care
	// which ones are in which spans. We can wipe the entire empty
	// list and move all workbuf spans to the free list.
	work.empty = 0
	work.wbufSpans.free.takeAll(&work.wbufSpans.busy)
	unlock(&work.wbufSpans.lock)
}

// freeSomeWbufs frees some workbufs back to the heap and returns
// true if it should be called again to free more.
func freeSomeWbufs(preemptible bool) bool {
	const batchSize = 64 // ~1–2 µs per span.
	lock(&work.wbufSpans.lock)
	if gcphase != _GCoff || work.wbufSpans.free.isEmpty() {
		unlock(&work.wbufSpans.lock)
		return false
	}
	systemstack(func() {
		gp := getg().m.curg
		for i := 0; i < batchSize && !(preemptible && gp.preempt); i++ {
			span := work.wbufSpans.free.first
			if span == nil {
				break
			}
			work.wbufSpans.free.remove(span)
			mheap_.freeManual(span, &memstats.gc_sys)
		}
	})
	more := !work.wbufSpans.free.isEmpty()
	unlock(&work.wbufSpans.lock)
	return more
}
