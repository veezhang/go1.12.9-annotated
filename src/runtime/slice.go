// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

// slice 是切片的底层数据结构
type slice struct {
	array unsafe.Pointer // 数据指针
	len   int            // 长度
	cap   int            // 容量
}

// An notInHeapSlice is a slice backed by go:notinheap memory.
// notInHeapSlice: go:notinheap 版本的 slice
type notInHeapSlice struct {
	array *notInHeap
	len   int
	cap   int
}

// 长度 out of range 错误
func panicmakeslicelen() {
	panic(errorString("makeslice: len out of range"))
}

// 容量 out of range 错误
func panicmakeslicecap() {
	panic(errorString("makeslice: cap out of range"))
}

// makeslice 生成 slice ，make []XXX 会替换成此函数
func makeslice(et *_type, len, cap int) unsafe.Pointer {
	// 根据切片的数据类型，获取切片的最大容量
	mem, overflow := math.MulUintptr(et.size, uintptr(cap))
	// 溢出判断
	if overflow || mem > maxAlloc || len < 0 || len > cap {
		// NOTE: Produce a 'len out of range' error instead of a
		// 'cap out of range' error when someone does make([]T, bignumber).
		// 'cap out of range' is true too, but since the cap is only being
		// supplied implicitly, saying len is clearer.
		// See golang.org/issue/4085.
		mem, overflow := math.MulUintptr(et.size, uintptr(len))
		if overflow || mem > maxAlloc || len < 0 {
			panicmakeslicelen()
		}
		panicmakeslicecap()
	}
	// 在堆上申请连续内存
	return mallocgc(mem, et, true)
}

func makeslice64(et *_type, len64, cap64 int64) unsafe.Pointer {
	len := int(len64)
	if int64(len) != len64 {
		panicmakeslicelen()
	}

	cap := int(cap64)
	if int64(cap) != cap64 {
		panicmakeslicecap()
	}

	return makeslice(et, len, cap)
}

// growslice handles slice growth during append.
// It is passed the slice element type, the old slice, and the desired new minimum capacity,
// and it returns a new slice with at least that capacity, with the old data
// copied into it.
// The new slice's length is set to the old slice's length,
// NOT to the new requested capacity.
// This is for codegen convenience. The old slice's length is used immediately
// to calculate where to write new values during an append.
// TODO: When the old backend is gone, reconsider this decision.
// The SSA backend might prefer the new length or to return only ptr/cap and save stack space.
// growslice 切片扩容， 对应 append 函数
func growslice(et *_type, old slice, cap int) slice {
	if raceenabled {
		callerpc := getcallerpc()
		racereadrangepc(old.array, uintptr(old.len*int(et.size)), callerpc, funcPC(growslice))
	}
	if msanenabled {
		msanread(old.array, uintptr(old.len*int(et.size)))
	}

	// 如果新要扩容的容量比原来的容量还要小，这代表要缩容了，那么可以直接 panic 。
	if cap < old.cap {
		panic(errorString("growslice: cap out of range"))
	}

	// 如果类型的尺寸为 0 ，不需要保留 old.array ，之间生成新的
	if et.size == 0 {
		// append should not create a slice with nil pointer but non-zero len.
		// We assume that append doesn't need to preserve old.array in this case.
		// append 不应使用 nil 指针创建切片，而是使用非零的 len 。 我们假设在这种情况下 append 不需要保留 old.array 。
		return slice{unsafe.Pointer(&zerobase), old.len, cap}
	}
	// 先按旧的 slice 容量翻倍，如果还不满足预期值，则按预期值扩容
	newcap := old.cap
	doublecap := newcap + newcap
	if cap > doublecap {
		newcap = cap
	} else {
		// 如果旧数据的长度小于 1024 ，则扩充 1 倍；否则按 1/4 倍扩容，直到满足预期值
		if old.len < 1024 {
			newcap = doublecap
		} else {
			// Check 0 < newcap to detect overflow
			// and prevent an infinite loop.
			// 0 < newcap 是为了检测溢出，以免无线循环
			for 0 < newcap && newcap < cap {
				newcap += newcap / 4
			}
			// Set newcap to the requested cap when
			// the newcap calculation overflowed.
			// 如果溢出了，则按预期值扩容
			if newcap <= 0 {
				newcap = cap
			}
		}
	}

	// 计算新的切片的容量，长度
	var overflow bool
	var lenmem, newlenmem, capmem uintptr
	// Specialize for common values of et.size.
	// For 1 we don't need any division/multiplication.
	// For sys.PtrSize, compiler will optimize division/multiplication into a shift by a constant.
	// For powers of 2, use a variable shift.
	// 根据元素类型的大小，选择对应的计算逻辑，节省计算量
	switch {
	case et.size == 1:
		lenmem = uintptr(old.len)
		newlenmem = uintptr(cap)
		capmem = roundupsize(uintptr(newcap))
		overflow = uintptr(newcap) > maxAlloc
		newcap = int(capmem)
	case et.size == sys.PtrSize:
		lenmem = uintptr(old.len) * sys.PtrSize
		newlenmem = uintptr(cap) * sys.PtrSize
		capmem = roundupsize(uintptr(newcap) * sys.PtrSize)
		overflow = uintptr(newcap) > maxAlloc/sys.PtrSize
		newcap = int(capmem / sys.PtrSize)
	case isPowerOfTwo(et.size):
		// 2的倍数，可以通过位移计算
		var shift uintptr
		if sys.PtrSize == 8 {
			// Mask shift for better code generation.
			shift = uintptr(sys.Ctz64(uint64(et.size))) & 63
		} else {
			shift = uintptr(sys.Ctz32(uint32(et.size))) & 31
		}
		lenmem = uintptr(old.len) << shift
		newlenmem = uintptr(cap) << shift
		capmem = roundupsize(uintptr(newcap) << shift)
		overflow = uintptr(newcap) > (maxAlloc >> shift)
		newcap = int(capmem >> shift)
	default:
		lenmem = uintptr(old.len) * et.size
		newlenmem = uintptr(cap) * et.size
		capmem, overflow = math.MulUintptr(et.size, uintptr(newcap))
		capmem = roundupsize(capmem)
		newcap = int(capmem / et.size)
	}

	// The check of overflow in addition to capmem > maxAlloc is needed
	// to prevent an overflow which can be used to trigger a segfault
	// on 32bit architectures with this example program:
	//
	// type T [1<<27 + 1]int64
	//
	// var d T
	// var s []T
	//
	// func main() {
	//   s = append(s, d, d, d, d)
	//   print(len(s), "\n")
	// }
	// 溢出判断
	if overflow || capmem > maxAlloc {
		panic(errorString("growslice: cap out of range"))
	}

	var p unsafe.Pointer
	if et.kind&kindNoPointers != 0 {
		// 没有堆上指针的情况下
		// 申请内存，并且不需要清零，此处类型是没有指针的，无关紧要
		p = mallocgc(capmem, nil, false)
		// The append() that calls growslice is going to overwrite from old.len to cap (which will be the new length).
		// Only clear the part that will not be overwritten.
		// 这里清零，只需要清零后的，应该前面的部分会调用 memmove 来初始化
		memclrNoHeapPointers(add(p, newlenmem), capmem-newlenmem)
	} else {
		// 有堆上指针的情况下
		// 申请内存，并且清零
		// Note: can't use rawmem (which avoids zeroing of memory), because then GC can scan uninitialized memory.
		// 注意：不能使用 rawmem （这样可以避免内存清零），因为 GC 可以扫描未初始化的内存。
		p = mallocgc(capmem, et, true)
		if writeBarrier.enabled { // 写屏障开启了
			// Only shade the pointers in old.array since we know the destination slice p
			// only contains nil pointers because it has been cleared during alloc.
			// 在分配过程中已将其清零，p 仅包含 nil 指针，所以仅对 old.array 中的指针进行 shade 处理。
			bulkBarrierPreWriteSrcOnly(uintptr(p), uintptr(old.array), lenmem)
		}
	}
	// 拷贝 old.array 到 p 中
	memmove(p, old.array, lenmem)
	// 返回 slice
	return slice{p, old.len, newcap}
}

// isPowerOfTwo 判断 2 的倍数
func isPowerOfTwo(x uintptr) bool {
	return x&(x-1) == 0
}

// slicecopy 拷贝 slice ，对应的 copy 函数
func slicecopy(to, fm slice, width uintptr) int {
	// 如果源切片或者目标切片有一个长度为 0 ，不需要拷贝
	if fm.len == 0 || to.len == 0 {
		return 0
	}

	// n 记录源切片或者目标切片较短的那一个的长度
	n := fm.len
	if to.len < n {
		n = to.len
	}

	// 如果 width = 0 ，不需要拷贝
	if width == 0 {
		return n
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(slicecopy)
		racewriterangepc(to.array, uintptr(n*int(width)), callerpc, pc)
		racereadrangepc(fm.array, uintptr(n*int(width)), callerpc, pc)
	}
	if msanenabled {
		msanwrite(to.array, uintptr(n*int(width)))
		msanread(fm.array, uintptr(n*int(width)))
	}

	// 记录大小，如果只有 1 个字节，直接指针转换；否则调用 memmove
	size := uintptr(n) * width
	if size == 1 { // common case worth about 2x to do here
		// TODO: is this still worth it with new memmove impl?
		*(*byte)(to.array) = *(*byte)(fm.array) // known to be a byte pointer
	} else {
		memmove(to.array, fm.array, size)
	}
	return n
}

// slicecopy 的特例，当 copy 拷贝 string 到 []byte 的时候调用这个
func slicestringcopy(to []byte, fm string) int {
	// 如果源切片或者目标切片有一个长度为 0 ，不需要拷贝
	if len(fm) == 0 || len(to) == 0 {
		return 0
	}

	// n 记录源切片或者目标切片较短的那一个的长度
	n := len(fm)
	if len(to) < n {
		n = len(to)
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(slicestringcopy)
		racewriterangepc(unsafe.Pointer(&to[0]), uintptr(n), callerpc, pc)
	}
	if msanenabled {
		msanwrite(unsafe.Pointer(&to[0]), uintptr(n))
	}

	// 调用 memmove
	memmove(unsafe.Pointer(&to[0]), stringStructOf(&fm).str, uintptr(n))
	return n
}
