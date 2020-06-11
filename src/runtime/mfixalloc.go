// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object allocator. Returned memory is not zeroed.
//
// See malloc.go for overview.
// 固定大小的对象分配器，返回的内存没有置0。

package runtime

import "unsafe"

// FixAlloc is a simple free-list allocator for fixed size objects.
// Malloc uses a FixAlloc wrapped around sysAlloc to manage its
// mcache and mspan objects.
// FixAlloc是用于固定大小对象的简单的自由列表分配器。Malloc使用包裹在sysAlloc周围的FixAlloc来管理其mcache和mspan对象。
//
// Memory returned by fixalloc.alloc is zeroed by default, but the
// caller may take responsibility for zeroing allocations by setting
// the zero flag to false. This is only safe if the memory never
// contains heap pointers.
//  由fixalloc.alloc返回的内存默认情况下置零了，但是调用者可以通过将zero标志设置为false来负责零分配。只有在内存中永远不包含堆指针的情况下，这才是安全的。
//
// The caller is responsible for locking around FixAlloc calls.
// Callers can keep state in the object but the first word is
// smashed by freeing and reallocating.
//  调用方负责锁定FixAlloc调用。调用方可以将状态保持在对象中，但是释放和重新分配会破坏第一个单词。
//
// Consider marking fixalloc'd types go:notinheap.
type fixalloc struct {
	size   uintptr                     // 固定大小对象的大小， 例如spanalloc为Sizeof(mspan{})， cachealloc为Sizeof(mcache{})
	first  func(arg, p unsafe.Pointer) // called first time p is returned
	arg    unsafe.Pointer              // 参数
	list   *mlink                      // 固定大小对象的列表，类似于空闲列表，释放后放到这里，下次申请的时候优先使用这个
	chunk  uintptr                     // use uintptr instead of unsafe.Pointer to avoid write barriers // 可分配块指针，使用uintptr而不是unsafe.Pointer来避免写障碍
	nchunk uint32                      // 块剩余大小
	inuse  uintptr                     // in-use bytes now // 已经用的字节数
	stat   *uint64                     //  统计的， 参见 ./mstatus.go
	zero   bool                        // zero allocations， // 初始化零标志
}

// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being performed
// this cannot be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
// 块的通用链接列表。 （通常，该块大于sizeof（MLink）。）由于对mlink.next的赋值将导致执行写屏障，因此某些内部GC结构无法使用它。
// 例如，当清除程序将未标记的对象放置在空闲列表上时，它不希望调用写屏障，因为这可能导致该对象可访问。
//
//go:notinheap
type mlink struct {
	next *mlink
}

// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
//  初始化f以分配给定大小的对象，使用分配器获取内存块。
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *uint64) {
	f.size = size
	f.first = first
	f.arg = arg
	f.list = nil
	f.chunk = 0
	f.nchunk = 0
	f.inuse = 0
	f.stat = stat
	f.zero = true
}

// 分配内存
func (f *fixalloc) alloc() unsafe.Pointer {
	if f.size == 0 {
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	// 如果列表中有，则直接使用
	if f.list != nil {
		v := unsafe.Pointer(f.list)
		f.list = f.list.next
		f.inuse += f.size
		if f.zero {
			memclrNoHeapPointers(v, f.size)
		}
		return v
	}
	// 不够申请一个固定大小的对象了， 重新省钱一个chunk
	if uintptr(f.nchunk) < f.size {
		// 分配_FixAllocChunk（16KB）内存， persistentalloc最终调用mmap来分配内存
		f.chunk = uintptr(persistentalloc(_FixAllocChunk, 0, f.stat))
		f.nchunk = _FixAllocChunk
	}
	v := unsafe.Pointer(f.chunk)
	// 分配的时候执行的函数，类似于构造函数
	if f.first != nil {
		f.first(f.arg, v)
	}
	f.chunk = f.chunk + f.size
	f.nchunk -= uint32(f.size)
	f.inuse += f.size
	return v
}

// 释放内存
func (f *fixalloc) free(p unsafe.Pointer) {
	f.inuse -= f.size
	// 释放后放到list中，供下次使用
	v := (*mlink)(p)
	v.next = f.list
	f.list = v
}
