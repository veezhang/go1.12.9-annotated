// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page heap.
//
// See malloc.go for overview.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// minPhysPageSize is a lower-bound on the physical page size. The
// true physical page size may be larger than this. In contrast,
// sys.PhysPageSize is an upper-bound on the physical page size.
// minPhysPageSize是物理页面大小的下限。实际的物理页面大小可能大于此大小。相反，sys.PhysPageSize是物理页面大小的上限。
const minPhysPageSize = 4096

// Main malloc heap.
// The heap itself is the "free" and "scav" treaps,
// but all the other global data is here too.
//
// 主要内存分配堆。堆本身就是free和scav的treaps，但是所有其他全局数据也都在这里。
// treap: 树堆(Tree Heap)，是一种树的结构。free: 空闲的spans；scav:空闲并且清扫的spans。
//
// mheap must not be heap-allocated because it contains mSpanLists,
// which must not be heap-allocated.
//
// mheap不能是堆分配的，因为它包含不能被堆分配的mSpanLists。
//
//go:notinheap
type mheap struct {
	lock      mutex  // 锁
	free      mTreap // free and non-scavenged spans // 空闲并且没有被清扫的spans
	scav      mTreap // free and scavenged spans // 空闲并且被清扫的spans
	sweepgen  uint32 // sweep generation, see comment in mspan // 扫描生成，请参见mspan中的注释
	sweepdone uint32 // all spans are swept // 所有的spans都被扫描了
	sweepers  uint32 // number of active sweepone calls // 活动的扫描调用数

	// allspans is a slice of all mspans ever created. Each mspan
	// appears exactly once.
	//
	// allspans是曾经创建的所有mspan切片。每个mspan只会出现一次。
	//
	// The memory for allspans is manually managed and can be
	// reallocated and move as the heap grows.
	//
	// allspans的内存是手动管理的，可以随着堆的增长而重新分配和移动。
	//
	// In general, allspans is protected by mheap_.lock, which
	// prevents concurrent access as well as freeing the backing
	// store. Accesses during STW might not hold the lock, but
	// must ensure that allocation cannot happen around the
	// access (since that may free the backing store).
	//
	// 通常，allspans受mheap_.lock保护，这可以防止并发访问以及释放后备存储器。
	// STW中的访问可以不持有锁定，但必须确保在访问期间不会发生分配（因为这可能释放后备存储器）。
	allspans []*mspan // all spans out there // 全部的spans

	// sweepSpans contains two mspan stacks: one of swept in-use
	// spans, and one of unswept in-use spans. These two trade
	// roles on each GC cycle. Since the sweepgen increases by 2
	// on each cycle, this means the swept spans are in
	// sweepSpans[sweepgen/2%2] and the unswept spans are in
	// sweepSpans[1-sweepgen/2%2]. Sweeping pops spans from the
	// unswept stack and pushes spans that are still in-use on the
	// swept stack. Likewise, allocating an in-use span pushes it
	// on the swept stack.
	//
	// sweepSpans包含两个mspan堆栈：一个是已经扫描过使用的spans，另一个是未扫描的使用的spans。在每个GC周期中，这两个角色交换。
	// 由于sweepgen在每个周期上增加2，这意味着已经扫描过的spans在sweepSpans [sweepgen / 2%2]为单位，而未经扫描过的spans在sweepSpans [1-sweepgen / 2％2]为单位。
	// 从未扫描的堆栈中扫出持久性有机污染物跨度，并推送仍在使用的扫掠堆栈中的跨度。 同样，分配使用中的跨度会将其压入扫掠堆栈。
	sweepSpans [2]gcSweepBuf

	_ uint32 // align uint64 fields on 32-bit for atomics // 64字节对齐

	// Proportional sweep
	//
	// These parameters represent a linear function from heap_live
	// to page sweep count. The proportional sweep system works to
	// stay in the black by keeping the current page sweep count
	// above this line at the current heap_live.
	//
	// The line has slope sweepPagesPerByte and passes through a
	// basis point at (sweepHeapLiveBasis, pagesSweptBasis). At
	// any given time, the system is at (memstats.heap_live,
	// pagesSwept) in this space.
	//
	// It's important that the line pass through a point we
	// control rather than simply starting at a (0,0) origin
	// because that lets us adjust sweep pacing at any time while
	// accounting for current progress. If we could only adjust
	// the slope, it would create a discontinuity in debt if any
	// progress has already been made.
	pagesInUse         uint64  // pages of spans in stats mSpanInUse; R/W with mheap.lock // mSpanInUse状态的页
	pagesSwept         uint64  // pages swept this cycle; updated atomically // 扫描的页面数量
	pagesSweptBasis    uint64  // pagesSwept to use as the origin of the sweep ratio; updated atomically // 扫描比例的初始基点
	sweepHeapLiveBasis uint64  // value of heap_live to use as the origin of sweep ratio; written with lock, read without // 扫描比例的初始处于存活状态的初始基点
	sweepPagesPerByte  float64 // proportional sweep ratio; written with lock, read without // 扫描比
	// TODO(austin): pagesInUse should be a uintptr, but the 386
	// compiler can't 8-byte align fields.

	// Page reclaimer state

	// reclaimIndex is the page index in allArenas of next page to
	// reclaim. Specifically, it refers to page (i %
	// pagesPerArena) of arena allArenas[i / pagesPerArena].
	//
	// If this is >= 1<<63, the page reclaimer is done scanning
	// the page marks.
	//
	// This is accessed atomically.
	// reclaimIndex是allArenas中下一个需要回收的页索引。(i % pagesPerArena)计算area中哪一个page；allArenas[i / pagesPerArena]计算哪一个area。
	reclaimIndex uint64
	// reclaimCredit is spare credit for extra pages swept. Since
	// the page reclaimer works in large chunks, it may reclaim
	// more than requested. Any spare pages released go to this
	// credit pool.
	//
	// This is accessed atomically.
	// reclaimCredit额外扫除的页面。由于页面回收器的工作量很大，因此它的回收量可能超出请求的数量。
	// 释放的所有备用页面都将进入此信用额度。这是原子访问的。
	// 多归还的pages,是回收对象在heapArena释放的
	reclaimCredit uintptr

	// scavengeCredit is spare credit for extra bytes scavenged.
	// Since the scavenging mechanisms operate on spans, it may
	// scavenge more than requested. Any spare pages released
	// go to this credit pool.
	//
	// This is protected by the mheap lock.
	// scavengeCredit是额外清扫的字节。因为清扫机制在span上操作，所以可能清扫超出请求的数量。释放的所有备用页面都将进入此信用额度。
	// 多回收给os的字节，下次回收可先扣减这个值，不足再回收真正的空间
	scavengeCredit uintptr

	// Malloc stats.
	largealloc  uint64                  // bytes allocated for large objects // 大对象分配的字节数
	nlargealloc uint64                  // number of large object allocations // 大对象分配的数目
	largefree   uint64                  // bytes freed for large objects (>maxsmallsize) // 大对象（>maxsmallsize）释放的字节数
	nlargefree  uint64                  // number of frees for large objects (>maxsmallsize) // 大对象（>maxsmallsize）释放的数目
	nsmallfree  [_NumSizeClasses]uint64 // number of frees for small objects (<=maxsmallsize) // 小对象（<=maxsmallsize）释放的数目

	// arenas is the heap arena map. It points to the metadata for
	// the heap for every arena frame of the entire usable virtual
	// address space.
	//
	// arenas是堆区的映射。它指向整个可用虚拟地址空间中每个area的元数据。
	//
	// Use arenaIndex to compute indexes into this array.
	//
	// 通过arenaIndex来计算arenas数组中的索引。
	//
	// For regions of the address space that are not backed by the
	// Go heap, the arena map contains nil.
	//
	// 对于GO堆不支持的地址空间，arena映射为nil。
	//
	// Modifications are protected by mheap_.lock. Reads can be
	// performed without locking; however, a given entry can
	// transition from nil to non-nil at any time when the lock
	// isn't held. (Entries never transitions back to nil.)
	//
	// arenas修改受mheap_.lock保护。读取可以不加锁，但是，在不加锁的时候都可以从nil变为non-nil。（永远不会转换回nil。）
	//
	// In general, this is a two-level mapping consisting of an L1
	// map and possibly many L2 maps. This saves space when there
	// are a huge number of arena frames. However, on many
	// platforms (even 64-bit), arenaL1Bits is 0, making this
	// effectively a single-level map. In this case, arenas[0]
	// will never be nil.
	//
	// 通常，这是由一个L1映射和许多L2映射组成两级映射。当存在大量的arena，这可以节省空间。
	// 但是，在许多平台（甚至是64位）上，arenaL1Bits也为0，这实际上使它成为单级映射。 在这种情况下，arenas [0]永远不会为零。
	arenas [1 << arenaL1Bits]*[1 << arenaL2Bits]*heapArena

	// heapArenaAlloc is pre-reserved space for allocating heapArena
	// objects. This is only used on 32-bit, where we pre-reserve
	// this space to avoid interleaving it with the heap itself.
	//
	// heapArenaAlloc是用于分配heapArena对象的预留空间。它仅用于32位系统，我们在此处预先保留此空间，以避免与堆本身交错。
	heapArenaAlloc linearAlloc

	// arenaHints is a list of addresses at which to attempt to
	// add more heap arenas. This is initially populated with a
	// set of general hint addresses, and grown with the bounds of
	// actual heap arena ranges.
	// arenaHints是尝试添加更多堆区的地址列表。最初使用一组常规提示地址进行填充，然后使用实际堆区范围的边界进行扩展。
	arenaHints *arenaHint

	// arena is a pre-reserved space for allocating heap arenas
	// (the actual arenas). This is only used on 32-bit.
	// arena是用于保留堆区（实际堆区）的预留空间。 仅在32位上使用。
	arena linearAlloc

	// allArenas is the arenaIndex of every mapped arena. This can
	// be used to iterate through the address space.
	//
	// allArenas是每个arena的arenaIndex。 这可用于遍历地址空间。
	//
	// Access is protected by mheap_.lock. However, since this is
	// append-only and old backing arrays are never freed, it is
	// safe to acquire mheap_.lock, copy the slice header, and
	// then release mheap_.lock.
	//
	// 访问受mheap_.lock保护。但是，由于这仅是追加操作，并且永远不会释放旧的后备数组，因此可以安全地获取mheap_.lock，复制切片头，然后释放mheap_.lock。
	allArenas []arenaIdx

	// sweepArenas is a snapshot of allArenas taken at the
	// beginning of the sweep cycle. This can be read safely by
	// simply blocking GC (by disabling preemption).
	//
	// sweepArenas是在扫描周期开始时所有Arenas的快照。可以通过简单地阻止GC（通过禁用抢占）来安全地读取它。
	sweepArenas []arenaIdx

	// _ uint32 // ensure 64-bit alignment of central

	// central free lists for small size classes.
	// the padding makes sure that the mcentrals are
	// spaced CacheLinePadSize bytes apart, so that each mcentral.lock
	// gets its own cache line.
	// central is indexed by spanClass.
	//
	// central是小类型的空闲列表。填充确保mcentrals以CacheLinePadSize字节间隔开，以便每个mcentral.lock都有自己的缓存行。central由spanClass索引。
	// 当从内存中取单元到cache中时，会一次取一个cacheline大小的内存区域到cache中，然后存进相应的cacheline中, 所以当你读取一个变量的时候，可能会把它
	// 相邻的变量也读取到CPU的缓存中(如果正好在一个cacheline中)，因为有很大的几率你回继续访问相邻的变量，这样CPU利用缓存就可以加速对内存的访问。在多核
	// 的情况下，如果两个CPU同时访问某个变量，可能两个CPU都会把变量以及相邻的变量都读入到自己的缓存中。这会带来一个问题：当第一个CPU更新一个变量a的时候，
	// 它会导致第二个CPU读取变量b cachemiss, 即使变量b的值实际并没有变化。因为CPU的最小读取单元是cacheline,所以你可以看作a和b是一个整体，这就是伪共享。
	// 这里为了消除伪共享带来的性能问题，添加了 pad 字段。
	central [numSpanClasses]struct {
		mcentral mcentral
		pad      [cpu.CacheLinePadSize - unsafe.Sizeof(mcentral{})%cpu.CacheLinePadSize]byte
	}

	spanalloc             fixalloc // allocator for span* // span*分配器
	cachealloc            fixalloc // allocator for mcache* // mcache*分配器
	treapalloc            fixalloc // allocator for treapNodes* // treapNodes*分配器
	specialfinalizeralloc fixalloc // allocator for specialfinalizer* // specialfinalizer*分配器
	specialprofilealloc   fixalloc // allocator for specialprofile* // specialprofile*分配器
	speciallock           mutex    // lock for special record allocators. // 特殊记录分配器的锁
	arenaHintAlloc        fixalloc // allocator for arenaHints // arenaHints*分配器

	unused *specialfinalizer // never set, just here to force the specialfinalizer type into DWARF // 从来没有设置过，只是在这里强制将specialfinalizer类型设置为DWARF
}

// 全局mheap
var mheap_ mheap

// A heapArena stores metadata for a heap arena. heapArenas are stored
// outside of the Go heap and accessed via the mheap_.arenas index.
//
// This gets allocated directly from the OS, so ideally it should be a
// multiple of the system page size. For example, avoid adding small
// fields.
//
// heapArena存储heap arena的元数据。heapArenas存储在Go堆之外，并可以通过mheap_.arenas索引进行访问。
// 这是直接从OS分配的，因此理想情况下，它应该是系统页面大小的倍数。 例如，避免添加小字段。
//
//go:notinheap
type heapArena struct {
	// bitmap stores the pointer/scalar bitmap for the words in
	// this arena. See mbitmap.go for a description. Use the
	// heapBits type to access this.
	//
	// bitmap存储arena是否是指针/是否扫描的信息。使用heapBits类型来访问。
	bitmap [heapArenaBitmapBytes]byte

	// spans maps from virtual address page ID within this arena to *mspan.
	// For allocated spans, their pages map to the span itself.
	// For free spans, only the lowest and highest pages map to the span itself.
	// Internal pages map to an arbitrary span.
	// For pages that have never been allocated, spans entries are nil.
	//
	// Modifications are protected by mheap.lock. Reads can be
	// performed without locking, but ONLY from indexes that are
	// known to contain in-use or stack spans. This means there
	// must not be a safe-point between establishing that an
	// address is live and looking it up in the spans array.
	//
	// spans把arena中虚拟地址页面ID映射到*mspan。对于已经分配spans，其页面映射到span自己。
	// 对空闲的span，只有最低和最高的页面映射到span自己。内部页面映射到任意span。对于从未分配的为nil。
	// 修改受mheap.lock保护。可以在不锁定的情况下执行读取，但是只能从已知包含使用中或堆栈跨度的索引中进行。
	// 这意味着在确定地址是否存在以及在spans数组中查找地址之间一定没有安全点。
	spans [pagesPerArena]*mspan

	// pageInUse is a bitmap that indicates which spans are in
	// state mSpanInUse. This bitmap is indexed by page number,
	// but only the bit corresponding to the first page in each
	// span is used.
	//
	// Writes are protected by mheap_.lock.
	// pageInUse是一个指示哪些spans处于mSpanInUse状态的bitmap。该位图通过页面索引，但是仅使用每个span中与第一页相对应的位。
	pageInUse [pagesPerArena / 8]uint8 // amd64上是：1024(pagesPerArena / 8)

	// pageMarks is a bitmap that indicates which spans have any
	// marked objects on them. Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically during marking. Reads are
	// non-atomic and lock-free since they only occur during
	// sweeping (and hence never race with writes).
	//
	// This is used to quickly find whole spans that can be freed.
	//
	// TODO(austin): It would be nice if this was uint64 for
	// faster scanning, but we don't have 64-bit atomic bit
	// operations.
	//
	// pageMarks是一个指示哪些span上有任何标记的对象的bitmap。与pageInUse一样，仅使用每个span中与第一页相对应的位。
	// 标记期间自动完成写入。 读取是非原子且无锁的，因为它们仅在扫描期间发生（因此从不与写入竞争）。 
	// 这用于快速查找可以释放的整个span。 TODO：如果uint64可以进行更快的扫描，那很好，但是我们没有64位原子位操作。
	pageMarks [pagesPerArena / 8]uint8 // amd64上是：1024(pagesPerArena / 8)
}

// arenaHint is a hint for where to grow the heap arenas. See
// mheap_.arenaHints.
//
// arenaHint提示了在哪里可以增加堆区。参见mheap_.arenaHints
//
//go:notinheap
type arenaHint struct {
	addr uintptr
	down bool
	next *arenaHint
}

// An mspan is a run of pages.
// mspan是一连串的pages
//
// When a mspan is in the heap free treap, state == mSpanFree
// and heapmap(s->start) == span, heapmap(s->start+s->npages-1) == span.
// If the mspan is in the heap scav treap, then in addition to the
// above scavenged == true. scavenged == false in all other cases.
// 当mspan在mheap_.free中，state == mSpanFree，并且 heapmap(s->start) == span, heapmap(s->start+s->npages-1) == span。
// 如果在mheap_.scav中，除了上面那些外，scavenged == true。在其他情况下scavenged == false。
//
// When a mspan is allocated, state == mSpanInUse or mSpanManual
// and heapmap(i) == span for all s->start <= i < s->start+s->npages.
// 当mspan被分配了，state == mSpanInUse或者mSpanManual，并且s->start <= i < s->start+s->npages中的所有都满足heapmap(i) == span。

// Every mspan is in one doubly-linked list, either in the mheap's
// busy list or one of the mcentral's span lists.
// 每一个mspan都在一个双向链表中，要么在mheap的busy list，要么在mcentral的span列表。

// An mspan representing actual memory has state mSpanInUse,
// mSpanManual, or mSpanFree. Transitions between these states are
// constrained as follows:
// 表示实际内存的mspan的状态有：mSpanInUse, mSpanManual, mSpanFree。这些状态之间的转换受以下约束：
//
// * A span may transition from free to in-use or manual during any GC
//   phase.
// * 在任何GC阶段，span都可能由mSpanFree转为mSpanInUse或者mSpanManual。
//
// * During sweeping (gcphase == _GCoff), a span may transition from
//   in-use to free (as a result of sweeping) or manual to free (as a
//   result of stacks being freed).
// * 在清扫（gcphase == _GCoff）期间，span可能由mSpanInUse转为mSpanFree（由于清扫）或者由mSpanManual转为mSpanFree（由于栈释放）。
//
// * During GC (gcphase != _GCoff), a span *must not* transition from
//   manual or in-use to free. Because concurrent GC may read a pointer
//   and then look up its span, the span state must be monotonic.
// 在GC(gcphase != _GCoff)期间，span绝不能从mSpanManual或者mSpanInUse转为mSpanFree。因为并发GC可能读一个指针然后查其span，所以span的状态必须是不变的。
type mSpanState uint8

const (
	mSpanDead   mSpanState = iota
	mSpanInUse             // allocated for garbage collected heap	// 堆GC分配的
	mSpanManual            // allocated for manual management (e.g., stack allocator) // 手动管理分配的（比如栈分配）
	mSpanFree
)

// mSpanStateNames are the names of the span states, indexed by
// mSpanState.
// mSpanStateNames是mSpanState的名称
var mSpanStateNames = []string{
	"mSpanDead",
	"mSpanInUse",
	"mSpanManual",
	"mSpanFree",
}

// mSpanList heads a linked list of spans.
// mspan的链表
//
//go:notinheap
type mSpanList struct {
	first *mspan // first span in list, or nil if none
	last  *mspan // last span in list, or nil if none
}

//mspan结构
//go:notinheap
type mspan struct {
	next *mspan     // next span in list, or nil if none // 双向链表，指向下一个
	prev *mspan     // previous span in list, or nil if none// 双向链表，指上一个
	list *mSpanList // For debugging. TODO: Remove.	// 链表

	startAddr uintptr // address of first byte of span aka s.base() // span的第一个字节的地址，也称为s.base()
	npages    uintptr // number of pages in span // span中的页数

	manualFreeList gclinkptr // list of free objects in mSpanManual spans // mSpanManual span的空闲列表 type gclinkptr uintptr

	// freeindex is the slot index between 0 and nelems at which to begin scanning
	// for the next free object in this span.
	// Each allocation scans allocBits starting at freeindex until it encounters a 0
	// indicating a free object. freeindex is then adjusted so that subsequent scans begin
	// just past the newly discovered free object.
	// freeindex是介于0和nelem之间的插槽索引，开始扫描span中的下一个空闲对象。每个分配都扫描从freeindex开始的allocBits，
	// 直到遇到表示空闲对象的0。然后对freeindex进行调整，以使后续扫描刚开始经过新发现的空闲对象。
	//
	// If freeindex == nelem, this span has no free objects.
	// 如果freeindex == nelem，那么这个span中没有空闲的对象了。
	//
	// allocBits is a bitmap of objects in this span.
	// If n >= freeindex and allocBits[n/8] & (1<<(n%8)) is 0
	// then object n is free;
	// otherwise, object n is allocated. Bits starting at nelem are
	// undefined and should never be referenced.
	// allocBits是此span中对象的bitmap。如果n> = freeindex并且allocBits[n/8] & (1<<(n%8))为0（也就是对应的bit为0），则对象n空闲；
	// 否则，对象n已经分配。 从nelem开始的位是未定义的，永远不要被引用。
	//
	// Object n starts at address n*elemsize + (start << pageShift).
	// 对象n起始地址是 n*elemsize + (start << pageShift)。
	freeindex uintptr
	// TODO: Look up nelems from sizeclass and remove this field if it
	// helps performance.
	nelems uintptr // number of object in the span. // span中的对象数。

	// Cache of the allocBits at freeindex. allocCache is shifted
	// such that the lowest bit corresponds to the bit freeindex.
	// allocCache holds the complement of allocBits, thus allowing
	// ctz (count trailing zero) to use it directly.
	// allocCache may contain bits beyond s.nelems; the caller must ignore
	// these.
	// 在freeindex处的allocBits的缓存。移位allocCache使得最低位对应于freeindex的位。allocCache保留allocBits的补码，
	// 从而允许ctz（计数尾随零）直接使用它。 allocCache可能包含s.nelems以外的位；调用者必须忽略这些。
	allocCache uint64

	// allocBits and gcmarkBits hold pointers to a span's mark and
	// allocation bits. The pointers are 8 byte aligned.
	// There are three arenas where this data is held.
	// free: Dirty arenas that are no longer accessed
	//       and can be reused.
	// next: Holds information to be used in the next GC cycle.
	// current: Information being used during this GC cycle.
	// previous: Information being used during the last GC cycle.
	// A new GC cycle starts with the call to finishsweep_m.
	// finishsweep_m moves the previous arena to the free arena,
	// the current arena to the previous arena, and
	// the next arena to the current arena.
	// The next arena is populated as the spans request
	// memory to hold gcmarkBits for the next GC cycle as well
	// as allocBits for newly allocated spans.
	// allocBits和gcmarkBits保存指向span的mask和分配bit的指针。指针是8字节对齐的。有3个arenas保留这个数据。
	// free: 不再访问且能够重新使用的脏arenas。
	// next: 保存要在下一个GC周期中使用的信息。
	// current: 此GC周期中正在使用的信息。
	// previous: 上一个GC周期中使用的信息。
	// 一个新的GC周期从调用finishsweep_m开始。finishsweep_m移动previous arena到空闲free arena，移动current arena到previous aren，
	// 移动next arena到current arena。next arena由spans请求内存填充，以存储下一个GC周期gcmarkBits以及新分配spans的allocBits
	//
	// The pointer arithmetic is done "by hand" instead of using
	// arrays to avoid bounds checks along critical performance
	// paths.
	// The sweep will free the old allocBits and set allocBits to the
	// gcmarkBits. The gcmarkBits are replaced with a fresh zeroed
	// out memory.
	// 指针算法是“手动”完成的，而不是根据关键性的性能使用数组来避免边界检查。扫描将释放旧的allocBits，并将allocBits设置为gcmarkBits。
	// gcmarkBits被替换为新的清零内存。
	allocBits  *gcBits // mspan中对象的位图
	gcmarkBits *gcBits // mspan中标记的位图,用于垃圾回收

	// sweep generation:
	// if sweepgen == h->sweepgen - 2, the span needs sweeping
	// if sweepgen == h->sweepgen - 1, the span is currently being swept
	// if sweepgen == h->sweepgen, the span is swept and ready to use
	// if sweepgen == h->sweepgen + 1, the span was cached before sweep began and is still cached, and needs sweeping
	// if sweepgen == h->sweepgen + 3, the span was swept and then cached and is still cached
	// h->sweepgen is incremented by 2 after every GC
	// sweep代，扫描计数器
	// 如果sweepgen == h->sweepgen - 2, span需要进行清扫。
	// 如果sweepgen == h->sweepgen - 1, span正在被清扫。
	// 如果sweepgen == h->sweepgen, span已经被清扫的，可以使用了。
	// 如果sweepgen == h->sweepgen + 1, span在清扫开始之前已被缓存，并且仍然被缓存，需要进行清扫。
	// 如果sweepgen == h->sweepgen + 3, span被清扫后缓存，并且仍然被缓存。
	// h->sweepgen在每次GC中增加2。

	sweepgen    uint32     // 扫描计数值，用户与mheap的sweepgen比较，根据差值确定该span的扫描状态
	divMul      uint16     // for divide by elemsize - divMagic.mul // divMagic成员，参考runtime/sizeclasses.go
	baseMask    uint16     // if non-0, elemsize is a power of 2, & this will get object allocation base // divMagic成员，参考runtime/sizeclasses.go
	allocCount  uint16     // number of allocated objects // 已分配的对象数
	spanclass   spanClass  // size class and noscan (uint8) // 尺寸类型和无需扫描
	state       mSpanState // mspaninuse etc // span状态，mSpanInUse, mSpanManual, mSpanFree
	needzero    uint8      // needs to be zeroed before allocation // 分配前需要清零
	divShift    uint8      // for divide by elemsize - divMagic.shift // divMagic成员，参考runtime/sizeclasses.go
	divShift2   uint8      // for divide by elemsize - divMagic.shift2 // divMagic成员，参考runtime/sizeclasses.go
	scavenged   bool       // whether this span has had its pages released to the OS // span是否已将其页面释放到操作系统
	elemsize    uintptr    // computed from sizeclass or from npages // 元素大小，从sizeclass或npages计算得来
	unusedsince int64      // first time spotted by gc in mspanfree state // GC首次发现mSpanFree状态
	limit       uintptr    // end of data in span // span中数据结束位置
	speciallock mutex      // guards specials list // specials的锁
	specials    *special   // linked list of special records sorted by offset. // 按偏移量排序的特殊记录列表
}

// 返回mspan的首地址
func (s *mspan) base() uintptr {
	return s.startAddr
}

// 返回mspan的布局，包含元素大小，元素个数，总大小
func (s *mspan) layout() (size, n, total uintptr) {
	total = s.npages << _PageShift
	size = s.elemsize
	if size > 0 {
		n = total / size
	}
	return
}

// physPageBounds returns the start and end of the span
// rounded in to the physical page size.
// physPageBounds返回物理页大小起始边界和结束地址，返回的边界在[s.base(), start + s.npages<<_PageShift]之间，并且与physPageSize对齐。
func (s *mspan) physPageBounds() (uintptr, uintptr) {
	start := s.base()
	end := start + s.npages<<_PageShift
	// 物理页地址大于_PageSize(8Kb)，调整
	if physPageSize > _PageSize {
		// Round start and end in.
		start = (start + physPageSize - 1) &^ (physPageSize - 1)
		end &^= physPageSize - 1
	}
	return start, end
}

// 合并mspan
func (h *mheap) coalesce(s *mspan) {
	// We scavenge s at the end after coalescing if s or anything
	// it merged with is marked scavenged.
	// 如果合并s或任何与它合并的被标记为清扫，则在合并后最后清扫s。
	needsScavenge := false
	prescavenged := s.released() // number of bytes already scavenged. // 已经被清扫的字节

	// merge is a helper which merges other into s, deletes references to other
	// in heap metadata, and then discards it. other must be adjacent to s.
	// merge是一个将other合并到s的辅助函数，删除堆元数据中对other的引用，然后将其丢弃。other必须与s相邻。
	merge := func(other *mspan) {
		// Adjust s via base and npages and also in heap metadata.
		// 通过base和npage以及堆元数据调整。
		s.npages += other.npages
		s.needzero |= other.needzero
		if other.startAddr < s.startAddr {
			s.startAddr = other.startAddr
			h.setSpan(s.base(), s)
		} else {
			h.setSpan(s.base()+s.npages*pageSize-1, s)
		}

		// If before or s are scavenged, then we need to scavenge the final coalesced span.
		// 如果之前或者s已经被清扫了，则需要清扫最终合并的span。
		needsScavenge = needsScavenge || other.scavenged || s.scavenged
		prescavenged += other.released()

		// The size is potentially changing so the treap needs to delete adjacent nodes and
		// insert back as a combined node.
		// 大小可能会发生变化，因此treap需要删除相邻节点并作为组合节点重新插入。
		if other.scavenged {
			h.scav.removeSpan(other)
		} else {
			h.free.removeSpan(other)
		}
		other.state = mSpanDead
		h.spanalloc.free(unsafe.Pointer(other))
	}

	// realign is a helper which shrinks other and grows s such that their
	// boundary is on a physical page boundary.
	// realign是一个辅助函数，它使other缩小并增长s，使它们的边界位于物理页面边界上。
	realign := func(a, b, other *mspan) {
		// Caller must ensure a.startAddr < b.startAddr and that either a or
		// b is s. a and b must be adjacent. other is whichever of the two is
		// not s.
		// 调用者必须确保a.startAddr < b.startAddr，并且a或b是s。a和b必须相邻。other是两者中不是s的那个。

		// If pageSize <= physPageSize then spans are always aligned
		// to physical page boundaries, so just exit.
		// 如果pageSize <= physPageSize， 那么spans始终与物理页对齐，因此退出即可。
		if pageSize <= physPageSize {
			return
		}
		// Since we're resizing other, we must remove it from the treap.
		// 由于我们正在调整other大小，因此必须将其从treap中移除。
		if other.scavenged {
			h.scav.removeSpan(other)
		} else {
			h.free.removeSpan(other)
		}
		// Round boundary to the nearest physical page size, toward the
		// scavenged span.
		// 将边界四舍五入到最近的物理页面尺寸，朝着清扫的span。
		boundary := b.startAddr
		if a.scavenged {
			boundary &^= (physPageSize - 1)
		} else {
			boundary = (boundary + physPageSize - 1) &^ (physPageSize - 1)
		}
		a.npages = (boundary - a.startAddr) / pageSize
		b.npages = (b.startAddr + b.npages*pageSize - boundary) / pageSize
		b.startAddr = boundary

		h.setSpan(boundary-1, a)
		h.setSpan(boundary, b)

		// Re-insert other now that it has a new size.
		// 现在重新插入other，使其具有新的大小。
		if other.scavenged {
			h.scav.insert(other)
		} else {
			h.free.insert(other)
		}
	}

	// Coalesce with earlier, later spans.
	// 和前面、后面的span合并。
	if before := spanOf(s.base() - 1); before != nil && before.state == mSpanFree {
		if s.scavenged == before.scavenged {
			merge(before)
		} else {
			realign(before, s, before)
		}
	}

	// Now check to see if next (greater addresses) span is free and can be coalesced.
	// 现在，检查下一个（更大的地址）span是否空闲，并且可以合并。
	if after := spanOf(s.base() + s.npages*pageSize); after != nil && after.state == mSpanFree {
		if s.scavenged == after.scavenged {
			merge(after)
		} else {
			realign(s, after, after)
		}
	}

	if needsScavenge {
		// When coalescing spans, some physical pages which
		// were not returned to the OS previously because
		// they were only partially covered by the span suddenly
		// become available for scavenging. We want to make sure
		// those holes are filled in, and the span is properly
		// scavenged. Rather than trying to detect those holes
		// directly, we collect how many bytes were already
		// scavenged above and subtract that from heap_released
		// before re-scavenging the entire newly-coalesced span,
		// which will implicitly bump up heap_released.
		// 合并span时，一些以前由于只是部分被span覆盖而没有归还给操作系统的物理页突然变得可以清扫。我们要确保填充了这些坑，并且span被正真地清扫。
		// 与其尝试直接检测这些漏洞，不如收集上面已经清扫的字节数，然后在重新清扫新合并的span之前从heap_released中减去这些字节数，这将隐式地提高heap_released。
		memstats.heap_released -= uint64(prescavenged)
		s.scavenge()
	}
}

// 清扫
func (s *mspan) scavenge() uintptr {
	// start and end must be rounded in, otherwise madvise
	// will round them *out* and release more memory
	// than we want.
	// 开始和结束必须按照物理页字节对齐，否则madvise会将它们放大，并释放比我们想要的更多的内存。
	start, end := s.physPageBounds()
	if end <= start {
		// start and end don't span a whole physical page.
		// 开始和结束不跨越整个物理页面。
		return 0
	}
	// 归还给操作系统，统计，标记scavenged
	released := end - start
	memstats.heap_released += uint64(released)
	s.scavenged = true
	sysUnused(unsafe.Pointer(start), released)
	return released
}

// released returns the number of bytes in this span
// which were returned back to the OS.
// released返回span已经归还给操作系统的字节数。
func (s *mspan) released() uintptr {
	if !s.scavenged {
		return 0
	}
	start, end := s.physPageBounds()
	return end - start
}

// recordspan adds a newly allocated span to h.allspans.
//
// This only happens the first time a span is allocated from
// mheap.spanalloc (it is not called when a span is reused).
//
// Write barriers are disallowed here because it can be called from
// gcWork when allocating new workbufs. However, because it's an
// indirect call from the fixalloc initializer, the compiler can't see
// this.
// recordspan将新分配的span添加到h.allspans。
// 仅在第一次从mheap.spanalloc中分配span时才会发生这种情况（当span重新使用时不会调用它）。
// 这里不允许写障碍，因为在分配新的工作缓冲区时可以从gcWork调用它。但是，由于它是从fixalloc初始化的间接调用，因此编译器看不到这一点。
// fixalloc的回调函数
//
//go:nowritebarrierrec
func recordspan(vh unsafe.Pointer, p unsafe.Pointer) {
	h := (*mheap)(vh)
	s := (*mspan)(p)
	// 扩容
	if len(h.allspans) >= cap(h.allspans) {
		// 第一次为64 * 1024 / sys.PtrSize，之后1.5倍增长
		n := 64 * 1024 / sys.PtrSize
		if n < cap(h.allspans)*3/2 {
			n = cap(h.allspans) * 3 / 2
		}
		// 直接修改切片的底层数组
		var new []*mspan
		sp := (*slice)(unsafe.Pointer(&new))
		sp.array = sysAlloc(uintptr(n)*sys.PtrSize, &memstats.other_sys)
		if sp.array == nil {
			throw("runtime: cannot allocate memory")
		}
		sp.len = len(h.allspans)
		sp.cap = n
		if len(h.allspans) > 0 {
			copy(new, h.allspans)
		}
		oldAllspans := h.allspans
		*(*notInHeapSlice)(unsafe.Pointer(&h.allspans)) = *(*notInHeapSlice)(unsafe.Pointer(&new))
		if len(oldAllspans) != 0 {
			sysFree(unsafe.Pointer(&oldAllspans[0]), uintptr(cap(oldAllspans))*unsafe.Sizeof(oldAllspans[0]), &memstats.other_sys)
		}
	}
	h.allspans = h.allspans[:len(h.allspans)+1]
	h.allspans[len(h.allspans)-1] = s
}

// A spanClass represents the size class and noscan-ness of a span.
//
// Each size class has a noscan spanClass and a scan spanClass. The
// noscan spanClass contains only noscan objects, which do not contain
// pointers and thus do not need to be scanned by the garbage
// collector.
// spanClass表示尺寸类和无需扫描的span
// 每个尺寸类都有一个noscan spanClass和一个scan spanClass。noscan spanClass仅包含noscan对象，该对象不包含指针，因此不需要由GC进行扫描。
type spanClass uint8

const (
	numSpanClasses = _NumSizeClasses << 1            // 67 << 1 = 134
	tinySpanClass  = spanClass(tinySizeClass<<1 | 1) // 2<<1 | 1 = 5
)

// 创建spanClass，最低一位作为是否需要扫描
func makeSpanClass(sizeclass uint8, noscan bool) spanClass {
	return spanClass(sizeclass<<1) | spanClass(bool2int(noscan))
}

// 获取spanClass的尺寸类
func (sc spanClass) sizeclass() int8 {
	return int8(sc >> 1)
}

// 判断是否是无需扫描的spanClass
func (sc spanClass) noscan() bool {
	return sc&1 != 0
}

// arenaIndex returns the index into mheap_.arenas of the arena
// containing metadata for p. This index combines of an index into the
// L1 map and an index into the L2 map and should be used as
// mheap_.arenas[ai.l1()][ai.l2()].
// arenaIndex返回包含p的元数据在的mheap_.arenas中的索引。此索引将L1映射中的索引和L2映射中的索引组合在一起，
// 应该这么使用mheap_.arenas[ai.l1()][ai.l2()]。
//
// If p is outside the range of valid heap addresses, either l1() or
// l2() will be out of bounds.
// 如果p在有效堆地址范围之外，则l1()或l2()都将超出范围。
//
// It is nosplit because it's called by spanOf and several other
// nosplit functions.
// 之所以使用nosplit，是因为它是由spanOf和其他几个nosplit函数调用的。
//
//go:nosplit
func arenaIndex(p uintptr) arenaIdx {
	return arenaIdx((p + arenaBaseOffset) / heapArenaBytes)
}

// arenaBase returns the low address of the region covered by heap
// arena i.
// arenaBase返回arenaIdx为i的低地址，和arenaIndex相反
func arenaBase(i arenaIdx) uintptr {
	return uintptr(i)*heapArenaBytes - arenaBaseOffset
}

// arenaIdx l1<<arenaL1Shift | l2
type arenaIdx uint

// 获取第一级缓存L1
func (i arenaIdx) l1() uint {
	if arenaL1Bits == 0 {
		// Let the compiler optimize this away if there's no
		// L1 map.
		// 如果没有L1映射，让编译器优化
		return 0
	} else {
		return uint(i) >> arenaL1Shift
	}
}

// 获取第二级缓存L2
func (i arenaIdx) l2() uint {
	if arenaL1Bits == 0 {
		return uint(i)
	} else {
		return uint(i) & (1<<arenaL2Bits - 1)
	}
}

// inheap reports whether b is a pointer into a (potentially dead) heap object.
// It returns false for pointers into mSpanManual spans.
// Non-preemptible because it is used by write barriers.
// inheap报告b是否是指向（可能已死）堆对象的指针。对于指向mSpanManual状态span的指针，它返回false。不可抢先，因为它被写屏障使用。
//
//go:nowritebarrier
//go:nosplit
func inheap(b uintptr) bool {
	return spanOfHeap(b) != nil
}

// inHeapOrStack is a variant of inheap that returns true for pointers
// into any allocated heap span.
// inHeapOrStack是inheap的一种变体，对于指向任何已分配堆span的指针，它返回true。
//
//go:nowritebarrier
//go:nosplit
func inHeapOrStack(b uintptr) bool {
	s := spanOf(b)
	if s == nil || b < s.base() {
		return false
	}
	switch s.state {
	case mSpanInUse, mSpanManual:
		return b < s.limit
	default:
		return false
	}
}

// spanOf returns the span of p. If p does not point into the heap
// arena or no span has ever contained p, spanOf returns nil.
//
// If p does not point to allocated memory, this may return a non-nil
// span that does *not* contain p. If this is a possibility, the
// caller should either call spanOfHeap or check the span bounds
// explicitly.
//
// Must be nosplit because it has callers that are nosplit.
//
// spanOf返回p的span。如果p没有指向heap arena或没有任何span包含p，则spanOf返回nil。
// 如果p没有指向分配的内存，则这可能会返回不包含p非空的span。如果可能，调用者应调用spanOfHeap或显式检查span边界。
//
//go:nosplit
func spanOf(p uintptr) *mspan {
	// This function looks big, but we use a lot of constant
	// folding around arenaL1Bits to get it under the inlining
	// budget. Also, many of the checks here are safety checks
	// that Go needs to do anyway, so the generated code is quite
	// short.
	// 这个函数看起来很大，但我们在arenaL1Bits周围使用了许多固定折叠，以使其在内联预算下得以实现。
	// 另外，这里的许多检查都是Go仍然需要执行的安全检查，因此生成的代码很短。
	ri := arenaIndex(p)
	if arenaL1Bits == 0 {
		// If there's no L1, then ri.l1() can't be out of bounds but ri.l2() can.
		// 如果没有L1，那么ri.l1()不能越界，但是ri.l2()可以。
		if ri.l2() >= uint(len(mheap_.arenas[0])) {
			return nil
		}
	} else {
		// If there's an L1, then ri.l1() can be out of bounds but ri.l2() can't.
		// 如果有L1，那么ri.l1()可能越界，但是ri.l2()不可能。
		if ri.l1() >= uint(len(mheap_.arenas)) {
			return nil
		}
	}
	l2 := mheap_.arenas[ri.l1()]
	if arenaL1Bits != 0 && l2 == nil { // Should never happen if there's no L1. // 如果没有L1，L2不可能为nil
		return nil
	}
	ha := l2[ri.l2()]
	if ha == nil {
		return nil
	}
	return ha.spans[(p/pageSize)%pagesPerArena]
}

// spanOfUnchecked is equivalent to spanOf, but the caller must ensure
// that p points into an allocated heap arena.
// spanOfUnchecked与spanOf等效，但是调用者必须确保p指向分配的heap arena。
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfUnchecked(p uintptr) *mspan {
	ai := arenaIndex(p)
	return mheap_.arenas[ai.l1()][ai.l2()].spans[(p/pageSize)%pagesPerArena]
}

// spanOfHeap is like spanOf, but returns nil if p does not point to a
// heap object.
// spanOfHeap类似于spanOf，但是如果p没有指向heap arena对象，则返回nil。
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfHeap(p uintptr) *mspan {
	s := spanOf(p)
	// If p is not allocated, it may point to a stale span, so we
	// have to check the span's bounds and state.
	if s == nil || p < s.base() || p >= s.limit || s.state != mSpanInUse {
		return nil
	}
	return s
}

// pageIndexOf returns the arena, page index, and page mask for pointer p.
// The caller must ensure p is in the heap.
// pageIndexOf返回指针p的arena，页面索引和页面掩码。调用者必须确保p在堆中。
// arena为对于的arena，pageIdx为arena中对于page的索引，pageMask为page掩码对于的位。
func pageIndexOf(p uintptr) (arena *heapArena, pageIdx uintptr, pageMask uint8) {
	ai := arenaIndex(p)
	arena = mheap_.arenas[ai.l1()][ai.l2()]
	pageIdx = ((p / pageSize) / 8) % uintptr(len(arena.pageInUse))
	pageMask = byte(1 << ((p / pageSize) % 8))
	return
}

// Initialize the heap.
// 初始化heap
func (h *mheap) init() {
	// 初始化各种分配器
	h.treapalloc.init(unsafe.Sizeof(treapNode{}), nil, nil, &memstats.other_sys)
	h.spanalloc.init(unsafe.Sizeof(mspan{}), recordspan, unsafe.Pointer(h), &memstats.mspan_sys) // span分配的时候会通过recordspan自动加入到heap
	h.cachealloc.init(unsafe.Sizeof(mcache{}), nil, nil, &memstats.mcache_sys)
	h.specialfinalizeralloc.init(unsafe.Sizeof(specialfinalizer{}), nil, nil, &memstats.other_sys)
	h.specialprofilealloc.init(unsafe.Sizeof(specialprofile{}), nil, nil, &memstats.other_sys)
	h.arenaHintAlloc.init(unsafe.Sizeof(arenaHint{}), nil, nil, &memstats.other_sys)

	// Don't zero mspan allocations. Background sweeping can
	// inspect a span concurrently with allocating it, so it's
	// important that the span's sweepgen survive across freeing
	// and re-allocating a span to prevent background sweeping
	// from improperly cas'ing it from 0.
	//
	// This is safe because mspan contains no heap pointers.
	// mspan分配不要置零。后台扫描可以在分配span的时候同时检查span，因此span的sweepgen在释放和重新分配span时必须存在，
	// 以防止后台扫描将其不正确地从0计算(CAS:Compare and Swap)，这一点很重要。
	h.spanalloc.zero = false

	// h->mapcache needs no init

	// 初始化central
	for i := range h.central {
		h.central[i].mcentral.init(spanClass(i))
	}
}

// reclaim sweeps and reclaims at least npage pages into the heap.
// It is called before allocating npage pages to keep growth in check.
//
// reclaim implements the page-reclaimer half of the sweeper.
//
// h must NOT be locked.
/// reclaim扫描并回收至少npage页到堆中。reclaim实现扫描器一半的页面回收器。h不能被锁住。
func (h *mheap) reclaim(npage uintptr) {
	// This scans pagesPerChunk at a time. Higher values reduce
	// contention on h.reclaimPos, but increase the minimum
	// latency of performing a reclaim.
	//
	// Must be a multiple of the pageInUse bitmap element size.
	//
	// The time required by this can vary a lot depending on how
	// many spans are actually freed. Experimentally, it can scan
	// for pages at ~300 GB/ms on a 2.6GHz Core i7, but can only
	// free spans at ~32 MB/ms. Using 512 pages bounds this at
	// roughly 100µs.
	//
	// TODO(austin): Half of the time spent freeing spans is in
	// locking/unlocking the heap (even with low contention). We
	// could make the slow path here several times faster by
	// batching heap frees.
	//
	// 一次扫描pagesPerChunk。较高的值会减少h.reclaimPos上的争用，但会增加执行回收的最小延迟。
	// 必须是pageInUse bitmap元素大小的倍数。
	// 所需的时间可能会有所不同，具体取决于实际释放了多少span。实验上，它可以在2.6GHz Core i7上以~300GB/ms的速度扫描页面，
	// 但只能以~32MB/ms的速度释放span。使用512页限制到大约100µs的时间。
	// TODO:释放span所花费的时间的一半是锁定/解锁heap（即使争用程度较低）。通过批量释放堆，我们可以快几倍。
	const pagesPerChunk = 512

	// Bail early if there's no more reclaim work.
	// 如果没有其他回收工作，请尽早保释（退出）。
	if atomic.Load64(&h.reclaimIndex) >= 1<<63 {
		return
	}

	// Disable preemption so the GC can't start while we're
	// sweeping, so we can read h.sweepArenas, and so
	// traceGCSweepStart/Done pair on the P.
	// 禁用抢占，以便在扫描时无法启动GC，因此我们可以读取h.sweepArenas和P上的traceGCSweepStart、traceGCSweepDone对。
	// 获取M
	mp := acquirem()

	// 开启跟踪扫描
	if trace.enabled {
		traceGCSweepStart()
	}

	// h.sweepArenas是所有的Arenas的快照
	arenas := h.sweepArenas
	locked := false
	// 知道回收npage，或者扫描完了
	for npage > 0 {
		// Pull from accumulated credit first.
		// 首先从积累的提取，直到没有积累了或者满足npage了
		if credit := atomic.Loaduintptr(&h.reclaimCredit); credit > 0 {
			take := credit
			if take > npage {
				// Take only what we need.
				take = npage
			}
			if atomic.Casuintptr(&h.reclaimCredit, credit, credit-take) {
				npage -= take
			}
			continue
		}
		// 积累的不够npage

		// Claim a chunk of work.
		// idx为下一个需要回收的页索引。h.reclaimIndex += pagesPerChunk，idx = h.reclaimIndex
		idx := uintptr(atomic.Xadd64(&h.reclaimIndex, pagesPerChunk) - pagesPerChunk)
		// 越界了，表示已经查找了所有的arenas
		if idx/pagesPerArena >= uintptr(len(arenas)) {
			// Page reclaiming is done.
			atomic.Store64(&h.reclaimIndex, 1<<63)
			break
		}

		if !locked {
			// Lock the heap for reclaimChunk.
			// 锁住heap为reclaimChunk函数
			lock(&h.lock)
			locked = true
		}

		// Scan this chunk.
		// 扫描page块，返回回收的页数
		nfound := h.reclaimChunk(arenas, idx, pagesPerChunk)
		if nfound <= npage { // 刚好或不够
			npage -= nfound
		} else { // 超过了，放入reclaimCredit中
			// Put spare pages toward global credit.
			atomic.Xadduintptr(&h.reclaimCredit, nfound-npage)
			npage = 0
		}
	}
	if locked {
		unlock(&h.lock)
	}

	// 结束跟踪扫描
	if trace.enabled {
		traceGCSweepDone()
	}
	// 释放M
	releasem(mp)
}

// reclaimChunk sweeps unmarked spans that start at page indexes [pageIdx, pageIdx+n).
// It returns the number of pages returned to the heap.
//
// h.lock must be held and the caller must be non-preemptible.
// reclaimChunk扫描从页面索引[pageIdx，pageIdx + n）开始的未标记span。它返回归还到堆的页面数。
// h.lock必须锁住，并且调用者必须不可抢占。
func (h *mheap) reclaimChunk(arenas []arenaIdx, pageIdx, n uintptr) uintptr {
	// The heap lock must be held because this accesses the
	// heapArena.spans arrays using potentially non-live pointers.
	// In particular, if a span were freed and merged concurrently
	// with this probing heapArena.spans, it would be possible to
	// observe arbitrary, stale span pointers.
	// 必须锁住堆锁，因为这会使用潜在的非活动指针访问heapArena.spans数组。
	// 尤其是，如果释放和合并span并发访问heapArena.spans，则可能会观察到任意的陈旧的span指针。
	// 只是扫描n，并不是释放n
	n0 := n
	var nFreed uintptr
	sg := h.sweepgen
	for n > 0 {
		ai := arenas[pageIdx/pagesPerArena]
		ha := h.arenas[ai.l1()][ai.l2()]

		// Get a chunk of the bitmap to work on.
		// 获取一块bitmap，arenaPage为对于的page，inUse为被使用的页面bitmap，marked为被标记的页面bitmap
		// (pageIdx % pagesPerArena)计算area中哪一个page；allArenas[pageIdx / pagesPerArena]计算哪一个area
		// arenaPage/8 计算出对应的位
		arenaPage := uint(pageIdx % pagesPerArena)
		inUse := ha.pageInUse[arenaPage/8:]
		marked := ha.pageMarks[arenaPage/8:]
		if uintptr(len(inUse)) > n/8 { // 只取n/8长度
			inUse = inUse[:n/8]
			marked = marked[:n/8]
		}

		// Scan this bitmap chunk for spans that are in-use
		// but have no marked objects on them.
		// 扫描此位图块以查找正在使用但没有标记对象的span。
		for i := range inUse {
			// 快速的检测，这里是检测1个byte，也就是检测8个
			inUseUnmarked := inUse[i] &^ marked[i]
			if inUseUnmarked == 0 {
				continue
			}

			// 针对8个bit，一个一个的检测
			for j := uint(0); j < 8; j++ {
				if inUseUnmarked&(1<<j) != 0 {
					// 获取对于的span
					s := ha.spans[arenaPage+uint(i)*8+j]
					// 可以参见：sweep generation:
					// if sweepgen == h->sweepgen - 2, the span needs sweeping
					// if sweepgen == h->sweepgen - 1, the span is currently being swept
					// 需要被扫描，然后转为正在扫描
					if atomic.Load(&s.sweepgen) == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) {
						npages := s.npages
						unlock(&h.lock)
						// 清扫span，不保留
						if s.sweep(false) {
							nFreed += npages
						}
						lock(&h.lock)
						// Reload inUse. It's possible nearby
						// spans were freed when we dropped the
						// lock and we don't want to get stale
						// pointers from the spans array.
						// 重新加载inUse。当我们解锁并且我们不想从spans数组中获取过时的指针时，可能释放了附近的span。
						inUseUnmarked = inUse[i] &^ marked[i]
					}
				}
			}
		}

		// Advance.
		pageIdx += uintptr(len(inUse) * 8)
		n -= uintptr(len(inUse) * 8)
	}
	if trace.enabled {
		// Account for pages scanned but not reclaimed.
		// 追踪GC清扫span，已扫描但未回收的页面。
		traceGCSweepSpan((n0 - nFreed) * pageSize)
	}
	return nFreed
}

// alloc_m is the internal implementation of mheap.alloc.
//
// alloc_m must run on the system stack because it locks the heap, so
// any stack growth during alloc_m would self-deadlock.
//
// alloc_m是mheap.alloc的内部实现。
// alloc_m必须在系统堆栈上运行，因为它会锁定堆，因此alloc_m期间任何堆栈增长都会自锁死。
//go:systemstack
func (h *mheap) alloc_m(npage uintptr, spanclass spanClass, large bool) *mspan {
	_g_ := getg()

	// To prevent excessive heap growth, before allocating n pages
	// we need to sweep and reclaim at least n pages.
	// 为了防止堆过度增长，在分配n页之前，我们需要清除并回收至少n页。
	if h.sweepdone == 0 {
		h.reclaim(npage)
	}

	lock(&h.lock)
	// transfer stats from cache to global
	// 将统计信息从mcache转移到全局
	memstats.heap_scan += uint64(_g_.m.mcache.local_scan)
	_g_.m.mcache.local_scan = 0
	memstats.tinyallocs += uint64(_g_.m.mcache.local_tinyallocs)
	_g_.m.mcache.local_tinyallocs = 0

	// 分配npage页的span
	s := h.allocSpanLocked(npage, &memstats.heap_inuse)
	if s != nil {
		// Record span info, because gc needs to be
		// able to map interior pointer to containing span.
		// 初始化span信息，因为gc需要能够将内部指针映射来包含span。
		atomic.Store(&s.sweepgen, h.sweepgen)
		h.sweepSpans[h.sweepgen/2%2].push(s) // Add to swept in-use list. // 将span加入到sweepSpans
		s.state = mSpanInUse
		s.allocCount = 0
		s.spanclass = spanclass
		if sizeclass := spanclass.sizeclass(); sizeclass == 0 {
			// sizeclass == 0为大的span
			s.elemsize = s.npages << _PageShift
			s.divShift = 0
			s.divMul = 0
			s.divShift2 = 0
			s.baseMask = 0
		} else {
			// 67中尺寸类
			s.elemsize = uintptr(class_to_size[sizeclass])
			m := &class_to_divmagic[sizeclass]
			s.divShift = m.shift
			s.divMul = m.mul
			s.divShift2 = m.shift2
			s.baseMask = m.baseMask
		}

		// Mark in-use span in arena page bitmap.
		// 在arena页bitmap中记录使用的span
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		arena.pageInUse[pageIdx] |= pageMask

		// update stats, sweep lists
		// 更新统计信息，清扫列表
		h.pagesInUse += uint64(npage)
		if large {
			memstats.heap_objects++
			mheap_.largealloc += uint64(s.elemsize)
			mheap_.nlargealloc++
			atomic.Xadd64(&memstats.heap_live, int64(npage<<_PageShift))
		}
	}
	// heap_scan and heap_live were updated.
	// heap_scan和heap_live更新了。
	if gcBlackenEnabled != 0 {
		gcController.revise()
	}

	if trace.enabled {
		traceHeapAlloc()
	}

	// h.spans is accessed concurrently without synchronization
	// from other threads. Hence, there must be a store/store
	// barrier here to ensure the writes to h.spans above happen
	// before the caller can publish a pointer p to an object
	// allocated from s. As soon as this happens, the garbage
	// collector running on another processor could read p and
	// look up s in h.spans. The unlock acts as the barrier to
	// order these writes. On the read side, the data dependency
	// between p and the index in h.spans orders the reads.
	//
	// h.spans可以并发访问，而不会与其他线程同步。因此，这里必须有一个存储/存储屏障，以确保在调用者可以
	// 在从s分配的对象指针p发布之前将以上的写入到h.spans。一旦发生这种情况，运行在另一个处理器上的垃圾收集器
	// 便可以读取p并在h.spans中查找s。解锁是这些有序写入的障碍。在读取方面，p和h.spans中的索引之间的数据有序读取。
	unlock(&h.lock)
	return s
}

// alloc allocates a new span of npage pages from the GC'd heap.
//
// Either large must be true or spanclass must indicates the span's
// size class and scannability.
//
// If needzero is true, the memory for the returned span will be zeroed.
//
// alloc从GC的堆中分配新的npage页的span。large必须为true或spanclass必须指示span的尺寸类和可扫描性。
// 如果Needzero为true，则返回范围的内存将清零。
func (h *mheap) alloc(npage uintptr, spanclass spanClass, large bool, needzero bool) *mspan {
	// Don't do any operations that lock the heap on the G stack.
	// It might trigger stack growth, and the stack growth code needs
	// to be able to allocate heap.
	// 不要执行任何将heap锁定在G堆栈上的操作。它可能会触发stack增长，并且stack增长代码需要能够分配heap。
	var s *mspan
	// systemstack在系统栈中调用给定的函数fn
	systemstack(func() {
		s = h.alloc_m(npage, spanclass, large)
	})

	// 置零
	if s != nil {
		if needzero && s.needzero != 0 {
			memclrNoHeapPointers(unsafe.Pointer(s.base()), s.npages<<_PageShift)
		}
		s.needzero = 0
	}
	return s
}

// allocManual allocates a manually-managed span of npage pages.
// allocManual returns nil if allocation fails.
//
// allocManual adds the bytes used to *stat, which should be a
// memstats in-use field. Unlike allocations in the GC'd heap, the
// allocation does *not* count toward heap_inuse or heap_sys.
//
// The memory backing the returned span may not be zeroed if
// span.needzero is set.
//
// allocManual must be called on the system stack to prevent stack
// growth. Since this is used by the stack allocator, stack growth
// during allocManual would self-deadlock.
//
// allocManual分配npage页的手动管理span，失败返回nil。
// allocManual将使用的字节加入到*stat中，*stat应该是memstats使用中的字段。与GC堆中的分配不同，该分配不计入heap_inuse或heap_sys。
// 如果设置了span.needzero，返回的span的内存可能不会置零。
// 必须在系统堆栈上调用allocManual，以防止堆栈增长。由于这是由堆栈分配器使用的，因此在allocManual期间堆栈的增长将自锁死。
//go:systemstack
func (h *mheap) allocManual(npage uintptr, stat *uint64) *mspan {
	lock(&h.lock)
	// 分配npage页的span
	s := h.allocSpanLocked(npage, stat)
	if s != nil {
		s.state = mSpanManual
		s.manualFreeList = 0
		s.allocCount = 0
		s.spanclass = 0
		s.nelems = 0
		s.elemsize = 0
		s.limit = s.base() + s.npages<<_PageShift // 手动管理要设置limit
		// Manually managed memory doesn't count toward heap_sys.
		// 手动管理的内存不计入heap_sys。
		memstats.heap_sys -= uint64(s.npages << _PageShift)
	}

	// This unlock acts as a release barrier. See mheap.alloc_m.
	// 此解锁充当释放屏障。参考mheap.alloc_m。
	unlock(&h.lock)

	return s
}

// setSpan modifies the span map so spanOf(base) is s.
// setSpan修改span映射，确保spanOf(base)就是s。
func (h *mheap) setSpan(base uintptr, s *mspan) {
	ai := arenaIndex(base)
	// base/pageSize计算属于哪个page，然后再%pagesPerArena，就是当前arena中对于的span
	h.arenas[ai.l1()][ai.l2()].spans[(base/pageSize)%pagesPerArena] = s
}

// setSpans modifies the span map so [spanOf(base), spanOf(base+npage*pageSize))
// is s.
// setSpans修改span映射，确保[spanOf(base), spanOf(base+npage*pageSize))都是s。
func (h *mheap) setSpans(base, npage uintptr, s *mspan) {
	p := base / pageSize
	ai := arenaIndex(base)
	ha := h.arenas[ai.l1()][ai.l2()]
	for n := uintptr(0); n < npage; n++ {
		i := (p + n) % pagesPerArena
		// 却换到下一个arena了，如果p % pagesPerArena == 0 的时候会重复计算ai,ha。
		if i == 0 { // if i == 0 && n != 0 {
			ai = arenaIndex(base + n*pageSize)
			ha = h.arenas[ai.l1()][ai.l2()]
		}
		ha.spans[i] = s
	}
}

// pickFreeSpan acquires a free span from internal free list
// structures if one is available. Otherwise returns nil.
// h must be locked.
// pickFreeSpan（如果有）从内部空闲列表结构获取一个空闲span。否则返回nil。h必须被锁定。
func (h *mheap) pickFreeSpan(npage uintptr) *mspan {
	tf := h.free.find(npage) // 从mheap.free找npage个span
	ts := h.scav.find(npage) // 从mheap.scav找npage个span

	// Check for whichever treap gave us the smaller, non-nil result.
	// Note that we want the _smaller_ free span, i.e. the free span
	// closer in size to the amount we requested (npage).
	// 检测是否free，scav哪一个返回非空但是小一些。请注意，我们需要小的空闲span，也就是说，更接近我们请求的数量（npage）
	// free和scav如果都满足，返回npages小一些的；否则返回满足的那一个，如果都不满足则是nil
	var s *mspan
	if tf != nil && (ts == nil || tf.spanKey.npages <= ts.spanKey.npages) {
		s = tf.spanKey
		h.free.removeNode(tf)
	} else if ts != nil && (tf == nil || tf.spanKey.npages > ts.spanKey.npages) {
		s = ts.spanKey
		h.scav.removeNode(ts)
	}
	return s
}

// Allocates a span of the given size.  h must be locked.
// The returned span has been removed from the
// free structures, but its state is still mSpanFree.
// 分配给定大小的span。h必须被锁定。返回的span已从mheap.free中删除，但其状态仍为mSpanFree。
func (h *mheap) allocSpanLocked(npage uintptr, stat *uint64) *mspan {
	var s *mspan

	// 先从mheap.free，mheap.scav中查找
	s = h.pickFreeSpan(npage)
	if s != nil {
		goto HaveSpan
	}
	// On failure, grow the heap and try again.
	// mheap.free，mheap.scav中没有，则增长heap
	if !h.grow(npage) {
		return nil
	}
	// 然后再从mheap.free，mheap.scav中查找
	s = h.pickFreeSpan(npage)
	if s != nil {
		goto HaveSpan
	}
	// 增长了heap，但是没有找到充足的span
	throw("grew heap, but no adequate free span found")

HaveSpan:
	// Mark span in use.
	if s.state != mSpanFree {
		// 候选的mspan不是mSpanFree状态
		throw("candidate mspan for allocation is not free")
	}
	if s.npages < npage {
		// 候选的mspan页大小不足
		throw("candidate mspan for allocation is too small")
	}

	// First, subtract any memory that was released back to
	// the OS from s. We will re-scavenge the trimmed section
	// if necessary.
	// 首先，减去释放回OS的s中的内存。如有必要，我们将重新扫描修剪过的部分。
	memstats.heap_released -= uint64(s.released())

	if s.npages > npage {
		// Trim extra and put it back in the heap.
		// 剔除多余的，然后放回堆中。将多的页组成一个新的span，然会到heap中。
		t := (*mspan)(h.spanalloc.alloc())
		t.init(s.base()+npage<<_PageShift, s.npages-npage)
		s.npages = npage
		h.setSpan(t.base()-1, s)
		h.setSpan(t.base(), t)
		h.setSpan(t.base()+t.npages*pageSize-1, t)
		t.needzero = s.needzero
		// If s was scavenged, then t may be scavenged.
		// 如果s已经被清扫了，那么t可能也被清扫了。需要归还之前已经删除了的heap_released。
		start, end := t.physPageBounds()
		if s.scavenged && start < end {
			memstats.heap_released += uint64(end - start)
			t.scavenged = true
		}
		s.state = mSpanManual // prevent coalescing with s // 防止与s合并，freeSpanLocked可能会合并
		t.state = mSpanManual
		h.freeSpanLocked(t, false, false, s.unusedsince) // 将多余的页组成的span释放
		s.state = mSpanFree                              // 重新设置为mSpanFree
	}
	// "Unscavenge" s only AFTER splitting so that
	// we only sysUsed whatever we actually need.
	// 没有被清扫的span分割后就能使用； 已经清扫过的需要调用sysUsed
	if s.scavenged {
		// sysUsed all the pages that are actually available
		// in the span. Note that we don't need to decrement
		// heap_released since we already did so earlier.
		// sysUsed span中实际可用的所有页面。请注意，由于我们之前已经这样做了，所以我们不需要再减少heap_released。
		sysUsed(unsafe.Pointer(s.base()), s.npages<<_PageShift)
		s.scavenged = false
	}
	// 重置GC首次发现mSpanFree状态
	s.unusedsince = 0

	// 设置span
	h.setSpans(s.base(), npage, s)

	// 更新统计信息
	*stat += uint64(npage << _PageShift)
	memstats.heap_idle -= uint64(npage << _PageShift)

	//println("spanalloc", hex(s.start<<_PageShift))
	if s.inList() {
		throw("still in list")
	}
	return s
}

// Try to add at least npage pages of memory to the heap,
// returning whether it worked.
//
// h must be locked.
// 尝试将至少分配npage页的内存添加到heap中，并返回它是否成功。h必须被锁定。
func (h *mheap) grow(npage uintptr) bool {
	ask := npage << _PageShift // 请求的字节
	v, size := h.sysAlloc(ask)
	if v == nil {
		print("runtime: out of memory: cannot allocate ", ask, "-byte block (", memstats.heap_sys, " in use)\n")
		return false
	}

	// Scavenge some pages out of the free treap to make up for
	// the virtual memory space we just allocated. We prefer to
	// scavenge the largest spans first since the cost of scavenging
	// is proportional to the number of sysUnused() calls rather than
	// the number of pages released, so we make fewer of those calls
	// with larger spans.
	// 从mheap.free堆树中清除一些页，以弥补我们刚刚分配的虚拟内存空间。我们更喜欢先清理最大的span，因为清理的成本
	// 与sysUnused()调用的数量成正比，而不是与释放的页面数成正比，因此，在span较大的那些调用中，我们调用得较少。
	h.scavengeLargest(size)

	// Create a fake "in use" span and free it, so that the
	// right coalescing happens.
	// 创建一个伪造的mSpanInUse状态的span并将其释放，以便进行正确的合并。
	s := (*mspan)(h.spanalloc.alloc())
	s.init(uintptr(v), size/pageSize)
	h.setSpans(s.base(), s.npages, s)
	atomic.Store(&s.sweepgen, h.sweepgen)
	s.state = mSpanInUse
	h.pagesInUse += uint64(s.npages)
	h.freeSpanLocked(s, false, true, 0)
	return true
}

// Free the span back into the heap.
//
// large must match the value of large passed to mheap.alloc. This is
// used for accounting.
// 释放span到heap中。large参数必须匹配传递给函数mheap.alloc的large参数。这用于计数。
func (h *mheap) freeSpan(s *mspan, large bool) {
	// 在systemstack中调用
	systemstack(func() {
		mp := getg().m
		lock(&h.lock)
		// 将统计信息从mcache转移到全局
		memstats.heap_scan += uint64(mp.mcache.local_scan)
		mp.mcache.local_scan = 0
		memstats.tinyallocs += uint64(mp.mcache.local_tinyallocs)
		mp.mcache.local_tinyallocs = 0
		// 如果开启了 The memory sanitizer (msan)
		if msanenabled {
			// Tell msan that this entire span is no longer in use.
			// 告诉msan这个span已不再使用
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			msanfree(base, bytes)
		}
		if large {
			// Match accounting done in mheap.alloc.
			// 和mheap.alloc中奇数相匹配
			memstats.heap_objects--
		}
		if gcBlackenEnabled != 0 {
			// heap_scan changed.
			// heap_scan改变了
			gcController.revise()
		}
		// 释放span
		h.freeSpanLocked(s, true, true, 0)
		unlock(&h.lock)
	})
}

// freeManual frees a manually-managed span returned by allocManual.
// stat must be the same as the stat passed to the allocManual that
// allocated s.
//
// This must only be called when gcphase == _GCoff. See mSpanState for
// an explanation.
//
// freeManual must be called on the system stack to prevent stack
// growth, just like allocManual.
//
// freeManual释放allocManual申请的手动管理的span。stat参数必须与传递给分配s的allocManual的stat参数相同。
//
//go:systemstack
func (h *mheap) freeManual(s *mspan, stat *uint64) {
	s.needzero = 1
	lock(&h.lock)
	*stat -= uint64(s.npages << _PageShift)
	memstats.heap_sys += uint64(s.npages << _PageShift)
	h.freeSpanLocked(s, false, true, 0)
	unlock(&h.lock)
}

// s must be on the busy list or unlinked.
// s必须是在使用，或者没有链接到heap
// acctinuse：是否是mSpanInUse；acctidle：是否是空闲的；unusedsince：首次GC扫描处于mSpanInUse状态的时间戳
func (h *mheap) freeSpanLocked(s *mspan, acctinuse, acctidle bool, unusedsince int64) {
	switch s.state {
	case mSpanManual:
		if s.allocCount != 0 {
			throw("mheap.freeSpanLocked - invalid stack free")
		}
	case mSpanInUse:
		if s.allocCount != 0 || s.sweepgen != h.sweepgen {
			print("mheap.freeSpanLocked - span ", s, " ptr ", hex(s.base()), " allocCount ", s.allocCount, " sweepgen ", s.sweepgen, "/", h.sweepgen, "\n")
			throw("mheap.freeSpanLocked - invalid free")
		}
		h.pagesInUse -= uint64(s.npages)

		// Clear in-use bit in arena page bitmap.
		// 清理arena page bitmap，只需要计算第一个page对于的bit
		// pageInUse是一个指示哪些spans处于mSpanInUse状态的bitmap。该位图通过页面索引，但是仅使用每个span中与第一页相对应的位。
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		// &^: 双目运算符，按位置零，将运算符左边数据相异的位保留，相同位清零。功能同a&(^b)相同
		// 0&^0 = 0
		// 0&^1 = 0
		// 1&^0 = 1
		// 1&^1 = 0
		// pageMask对于的bit清0，其他bit保持不变
		arena.pageInUse[pageIdx] &^= pageMask
	default:
		throw("mheap.freeSpanLocked - invalid span state")
	}

	// 处理heap_inuse，heap_idle统计
	if acctinuse {
		memstats.heap_inuse -= uint64(s.npages << _PageShift)
	}
	if acctidle {
		memstats.heap_idle += uint64(s.npages << _PageShift)
	}
	s.state = mSpanFree

	// Stamp newly unused spans. The scavenger will use that
	// info to potentially give back some pages to the OS.
	// 标记新未使用的span。清扫将使用该信息来将某些页面还给操作系统。
	s.unusedsince = unusedsince
	if unusedsince == 0 {
		s.unusedsince = nanotime()
	}

	// Coalesce span with neighbors.
	// 与相连的span合并。
	h.coalesce(s)

	// Insert s into the appropriate treap.
	// 把s插入到合适的树堆中
	if s.scavenged {
		h.scav.insert(s)
	} else {
		h.free.insert(s)
	}
}

// scavengeLargest scavenges nbytes worth of spans in unscav
// starting from the largest span and working down. It then takes those spans
// and places them in scav. h must be locked.
// scavengeLargest从最大的没有被清扫的span开始清扫nbytes字节。然后，将这些span放入到mheap.scav中。h必须被锁定。
func (h *mheap) scavengeLargest(nbytes uintptr) {
	// Use up scavenge credit if there's any available.
	// 先用完可用的，scavengeCredit多之前清扫的字节。
	if nbytes > h.scavengeCredit {
		nbytes -= h.scavengeCredit
		h.scavengeCredit = 0
	} else {
		h.scavengeCredit -= nbytes
		return
	}
	// scavengeCredit的字节数不够

	// Iterate over the treap backwards (from largest to smallest) scavenging spans
	// until we've reached our quota of nbytes.
	// 向后（从最大到最小）迭代清理span，直到达到nbytes。
	released := uintptr(0)
	for t := h.free.end(); released < nbytes && t.valid(); {
		s := t.span()
		r := s.scavenge()
		if r == 0 {
			// Since we're going in order of largest-to-smallest span, this
			// means all other spans are no bigger than s. There's a high
			// chance that the other spans don't even cover a full page,
			// (though they could) but iterating further just for a handful
			// of pages probably isn't worth it, so just stop here.
			//
			// This check also preserves the invariant that spans that have
			// `scavenged` set are only ever in the `scav` treap, and
			// those which have it unset are only in the `free` treap.
			//
			// 由于我们按照最大到最小的span顺序进行，所以这意味着所有其他span都不大于s。其他span甚至都有很可能无法
			// 覆盖整页（尽管它们可以），但是仅对少数几页进行进一步迭代可能不值得，所以就在这里停止。
			// 该检查还保留了只在mheap.scav中设置为“scavenged”的不变的那些span，而那些未设其的跨度仅在mheap.free中。
			return
		}
		// 往前迭代，n将会赋值给t
		n := t.prev()
		h.free.erase(t)
		// Now that s is scavenged, we must eagerly coalesce it
		// with its neighbors to prevent having two spans with
		// the same scavenged state adjacent to each other.
		// 既然已经清扫了s，我们必须热切地将其与它的邻居合并，以防止两个被清除的span彼此相邻。
		h.coalesce(s)
		t = n
		h.scav.insert(s)
		released += r
	}
	// If we over-scavenged, turn that extra amount into credit.
	// 如果我们清扫过了，则将这笔额外的字节记入scavengeCredit。
	if released > nbytes {
		h.scavengeCredit += released - nbytes
	}
}

// scavengeAll visits each node in the unscav treap and scavenges the
// treapNode's span. It then removes the scavenged span from
// unscav and adds it into scav before continuing. h must be locked.
// scavengeAll访问mheap.free(unscav)的每个节点，并清理treapNode的span。然后，它从mheap.free(unscav)中删除，
// 然后添加到mheap.scav中，然后再继续循环。 h必须被锁定。
func (h *mheap) scavengeAll(now, limit uint64) uintptr {
	// Iterate over the treap scavenging spans if unused for at least limit time.
	// 如果至少在limit时间内未使用，请遍历mheap.free清扫span。
	released := uintptr(0)
	for t := h.free.start(); t.valid(); {
		s := t.span()
		n := t.next()
		// limit长时间没有使用此span了，将其清扫
		if (now - uint64(s.unusedsince)) > limit {
			r := s.scavenge()
			if r != 0 {
				h.free.erase(t)
				// Now that s is scavenged, we must eagerly coalesce it
				// with its neighbors to prevent having two spans with
				// the same scavenged state adjacent to each other.
				// 既然已经清扫了s，我们必须热切地将其与它的邻居合并，以防止两个被清除的span彼此相邻。
				h.coalesce(s)
				h.scav.insert(s)
				released += r
			}
		}
		t = n
	}
	return released
}

// heap清扫，在sysmon会调用
func (h *mheap) scavenge(k int32, now, limit uint64) {
	// Disallow malloc or panic while holding the heap lock. We do
	// this here because this is an non-mallocgc entry-point to
	// the mheap API.
	// heap锁住时，禁止malloc或panic。我们这样做是因为这不是mallocgc的mheap API入口。
	gp := getg()
	gp.m.mallocing++
	lock(&h.lock)
	released := h.scavengeAll(now, limit)
	unlock(&h.lock)
	gp.m.mallocing--

	// 打印gc跟踪日志
	if debug.gctrace > 0 {
		if released > 0 {
			print("scvg", k, ": ", released>>20, " MB released\n")
		}
		print("scvg", k, ": inuse: ", memstats.heap_inuse>>20, ", idle: ", memstats.heap_idle>>20, ", sys: ", memstats.heap_sys>>20, ", released: ", memstats.heap_released>>20, ", consumed: ", (memstats.heap_sys-memstats.heap_released)>>20, " (MB)\n")
	}
}

//go:linkname runtime_debug_freeOSMemory runtime/debug.freeOSMemory
func runtime_debug_freeOSMemory() {
	GC()
	systemstack(func() { mheap_.scavenge(-1, ^uint64(0), 0) })
}

// Initialize a new span with the given start and npages.
// 根据起始地址和页数初始化一个span，span构造函数
func (span *mspan) init(base uintptr, npages uintptr) {
	// span is *not* zeroed.
	span.next = nil
	span.prev = nil
	span.list = nil
	span.startAddr = base
	span.npages = npages
	span.allocCount = 0
	span.spanclass = 0
	span.elemsize = 0
	span.state = mSpanDead
	span.unusedsince = 0
	span.scavenged = false
	span.speciallock.key = 0
	span.specials = nil
	span.needzero = 0
	span.freeindex = 0
	span.allocBits = nil
	span.gcmarkBits = nil
}

// span是否在链表中
func (span *mspan) inList() bool {
	return span.list != nil
}

// Initialize an empty doubly-linked list.
// 初始化空的双向链表
func (list *mSpanList) init() {
	list.first = nil
	list.last = nil
}

// 从mSpanList删除span
func (list *mSpanList) remove(span *mspan) {
	if span.list != list {
		print("runtime: failed mSpanList.remove span.npages=", span.npages,
			" span=", span, " prev=", span.prev, " span.list=", span.list, " list=", list, "\n")
		throw("mSpanList.remove")
	}
	if list.first == span {
		list.first = span.next
	} else {
		span.prev.next = span.next
	}
	if list.last == span {
		list.last = span.prev
	} else {
		span.next.prev = span.prev
	}
	span.next = nil
	span.prev = nil
	span.list = nil
}

// mSpanList是否为空
func (list *mSpanList) isEmpty() bool {
	return list.first == nil
}

// mSpanList中插入span，头插法
func (list *mSpanList) insert(span *mspan) {
	// span应该是单独的，不在任务链表中
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insert", span, span.next, span.prev, span.list)
		throw("mSpanList.insert")
	}
	span.next = list.first
	if list.first != nil {
		// The list contains at least one span; link it in.
		// The last span in the list doesn't change.
		list.first.prev = span
	} else {
		// The list contains no spans, so this is also the last span.
		list.last = span
	}
	list.first = span
	span.list = list
}

// mSpanList中插入span，尾插法
func (list *mSpanList) insertBack(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insertBack", span, span.next, span.prev, span.list)
		throw("mSpanList.insertBack")
	}
	span.prev = list.last
	if list.last != nil {
		// The list contains at least one span.
		list.last.next = span
	} else {
		// The list contains no spans, so this is also the first span.
		list.first = span
	}
	list.last = span
	span.list = list
}

// takeAll removes all spans from other and inserts them at the front
// of list.
// takeAll从所有其他mSpanList中删除所有span并将它们插入到本列表的开头。
func (list *mSpanList) takeAll(other *mSpanList) {
	if other.isEmpty() {
		return
	}

	// Reparent everything in other to list.
	// 将other中所有的span的成员list设置为本list
	for s := other.first; s != nil; s = s.next {
		s.list = list
	}

	// Concatenate the lists.
	// 合并列表
	if list.isEmpty() {
		*list = *other
	} else {
		// Neither list is empty. Put other before list.
		other.last.next = list.first
		list.first.prev = other.last
		list.first = other.first
	}

	other.first, other.last = nil, nil
}

const (
	_KindSpecialFinalizer = 1
	_KindSpecialProfile   = 2
	// Note: The finalizer special must be first because if we're freeing
	// an object, a finalizer special will cause the freeing operation
	// to abort, and we want to keep the other special records around
	// if that happens.
	// 注意：特殊终结器必须是第一个，因为如果我们要释放对象，则特殊终结器将导致释放操作中止，
	// 如果发生这种情况，我们希望保留其他特殊记录。
)

//go:notinheap
type special struct {
	next   *special // linked list in span
	offset uint16   // span offset of object
	kind   byte     // kind of special
}

// Adds the special record s to the list of special records for
// the object p. All fields of s should be filled in except for
// offset & next, which this routine will fill in.
// Returns true if the special was successfully added, false otherwise.
// (The add will fail only if a record with the same p and s->kind
//  already exists.)
// 将特殊记录的s(special)添加到对象p的特殊记录列表中。s的所有字段都应填写，但偏移量和next除外，runtime将填写该字段。
// 如果成功添加special，则返回true，否则返回false。（仅当p和s->kind的具有相同记录时，添加操作才会失败。）
func addspecial(p unsafe.Pointer, s *special) bool {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("addspecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	// 确保span已经清扫了。扫描访问不带锁的特价商品列表，因此我们必须与其同步。这样更加安全。
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()
	kind := s.kind

	lock(&span.speciallock)

	// Find splice point, check for existing record.
	t := &span.specials
	for {
		x := *t
		if x == nil {
			break
		}
		if offset == uintptr(x.offset) && kind == x.kind {
			unlock(&span.speciallock)
			releasem(mp)
			return false // already exists
		}
		if offset < uintptr(x.offset) || (offset == uintptr(x.offset) && kind < x.kind) {
			break
		}
		t = &x.next
	}

	// Splice in record, fill in offset.
	s.offset = uint16(offset)
	s.next = *t
	*t = s
	unlock(&span.speciallock)
	releasem(mp)

	return true
}

// Removes the Special record of the given kind for the object p.
// Returns the record if the record existed, nil otherwise.
// The caller must FixAlloc_Free the result.
func removespecial(p unsafe.Pointer, kind uint8) *special {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("removespecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()

	lock(&span.speciallock)
	t := &span.specials
	for {
		s := *t
		if s == nil {
			break
		}
		// This function is used for finalizers only, so we don't check for
		// "interior" specials (p must be exactly equal to s->offset).
		if offset == uintptr(s.offset) && kind == s.kind {
			*t = s.next
			unlock(&span.speciallock)
			releasem(mp)
			return s
		}
		t = &s.next
	}
	unlock(&span.speciallock)
	releasem(mp)
	return nil
}

// The described object has a finalizer set for it.
//
// specialfinalizer is allocated from non-GC'd memory, so any heap
// pointers must be specially handled.
//
//go:notinheap
type specialfinalizer struct {
	special special
	fn      *funcval // May be a heap pointer.
	nret    uintptr
	fint    *_type   // May be a heap pointer, but always live.
	ot      *ptrtype // May be a heap pointer, but always live.
}

// Adds a finalizer to the object p. Returns true if it succeeded.
func addfinalizer(p unsafe.Pointer, f *funcval, nret uintptr, fint *_type, ot *ptrtype) bool {
	lock(&mheap_.speciallock)
	s := (*specialfinalizer)(mheap_.specialfinalizeralloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialFinalizer
	s.fn = f
	s.nret = nret
	s.fint = fint
	s.ot = ot
	if addspecial(p, &s.special) {
		// This is responsible for maintaining the same
		// GC-related invariants as markrootSpans in any
		// situation where it's possible that markrootSpans
		// has already run but mark termination hasn't yet.
		if gcphase != _GCoff {
			base, _, _ := findObject(uintptr(p), 0, 0)
			mp := acquirem()
			gcw := &mp.p.ptr().gcw
			// Mark everything reachable from the object
			// so it's retained for the finalizer.
			scanobject(base, gcw)
			// Mark the finalizer itself, since the
			// special isn't part of the GC'd heap.
			scanblock(uintptr(unsafe.Pointer(&s.fn)), sys.PtrSize, &oneptrmask[0], gcw, nil)
			releasem(mp)
		}
		return true
	}

	// There was an old finalizer
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
	return false
}

// Removes the finalizer (if any) from the object p.
func removefinalizer(p unsafe.Pointer) {
	s := (*specialfinalizer)(unsafe.Pointer(removespecial(p, _KindSpecialFinalizer)))
	if s == nil {
		return // there wasn't a finalizer to remove
	}
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
}

// The described object is being heap profiled.
//
//go:notinheap
type specialprofile struct {
	special special
	b       *bucket
}

// Set the heap profile bucket associated with addr to b.
func setprofilebucket(p unsafe.Pointer, b *bucket) {
	lock(&mheap_.speciallock)
	s := (*specialprofile)(mheap_.specialprofilealloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialProfile
	s.b = b
	if !addspecial(p, &s.special) {
		throw("setprofilebucket: profile already set")
	}
}

// Do whatever cleanup needs to be done to deallocate s. It has
// already been unlinked from the mspan specials list.
func freespecial(s *special, p unsafe.Pointer, size uintptr) {
	switch s.kind {
	case _KindSpecialFinalizer:
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		queuefinalizer(p, sf.fn, sf.nret, sf.fint, sf.ot)
		lock(&mheap_.speciallock)
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
	case _KindSpecialProfile:
		sp := (*specialprofile)(unsafe.Pointer(s))
		mProf_Free(sp.b, size)
		lock(&mheap_.speciallock)
		mheap_.specialprofilealloc.free(unsafe.Pointer(sp))
		unlock(&mheap_.speciallock)
	default:
		throw("bad special kind")
		panic("not reached")
	}
}

// gcBits is an alloc/mark bitmap. This is always used as *gcBits.
//
//go:notinheap
type gcBits uint8

// bytep returns a pointer to the n'th byte of b.
func (b *gcBits) bytep(n uintptr) *uint8 {
	return addb((*uint8)(b), n)
}

// bitp returns a pointer to the byte containing bit n and a mask for
// selecting that bit from *bytep.
func (b *gcBits) bitp(n uintptr) (bytep *uint8, mask uint8) {
	return b.bytep(n / 8), 1 << (n % 8)
}

const gcBitsChunkBytes = uintptr(64 << 10)
const gcBitsHeaderBytes = unsafe.Sizeof(gcBitsHeader{})

type gcBitsHeader struct {
	free uintptr // free is the index into bits of the next free byte.
	next uintptr // *gcBits triggers recursive type bug. (issue 14620)
}

//go:notinheap
type gcBitsArena struct {
	// gcBitsHeader // side step recursive type bug (issue 14620) by including fields by hand.
	free uintptr // free is the index into bits of the next free byte; read/write atomically
	next *gcBitsArena
	bits [gcBitsChunkBytes - gcBitsHeaderBytes]gcBits
}

var gcBitsArenas struct {
	lock     mutex
	free     *gcBitsArena
	next     *gcBitsArena // Read atomically. Write atomically under lock.
	current  *gcBitsArena
	previous *gcBitsArena
}

// tryAlloc allocates from b or returns nil if b does not have enough room.
// This is safe to call concurrently.
func (b *gcBitsArena) tryAlloc(bytes uintptr) *gcBits {
	if b == nil || atomic.Loaduintptr(&b.free)+bytes > uintptr(len(b.bits)) {
		return nil
	}
	// Try to allocate from this block.
	end := atomic.Xadduintptr(&b.free, bytes)
	if end > uintptr(len(b.bits)) {
		return nil
	}
	// There was enough room.
	start := end - bytes
	return &b.bits[start]
}

// newMarkBits returns a pointer to 8 byte aligned bytes
// to be used for a span's mark bits.
func newMarkBits(nelems uintptr) *gcBits {
	blocksNeeded := uintptr((nelems + 63) / 64)
	bytesNeeded := blocksNeeded * 8

	// Try directly allocating from the current head arena.
	head := (*gcBitsArena)(atomic.Loadp(unsafe.Pointer(&gcBitsArenas.next)))
	if p := head.tryAlloc(bytesNeeded); p != nil {
		return p
	}

	// There's not enough room in the head arena. We may need to
	// allocate a new arena.
	lock(&gcBitsArenas.lock)
	// Try the head arena again, since it may have changed. Now
	// that we hold the lock, the list head can't change, but its
	// free position still can.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate a new arena. This may temporarily drop the lock.
	fresh := newArenaMayUnlock()
	// If newArenaMayUnlock dropped the lock, another thread may
	// have put a fresh arena on the "next" list. Try allocating
	// from next again.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		// Put fresh back on the free list.
		// TODO: Mark it "already zeroed"
		fresh.next = gcBitsArenas.free
		gcBitsArenas.free = fresh
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate from the fresh arena. We haven't linked it in yet, so
	// this cannot race and is guaranteed to succeed.
	p := fresh.tryAlloc(bytesNeeded)
	if p == nil {
		throw("markBits overflow")
	}

	// Add the fresh arena to the "next" list.
	fresh.next = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), unsafe.Pointer(fresh))

	unlock(&gcBitsArenas.lock)
	return p
}

// newAllocBits returns a pointer to 8 byte aligned bytes
// to be used for this span's alloc bits.
// newAllocBits is used to provide newly initialized spans
// allocation bits. For spans not being initialized the
// mark bits are repurposed as allocation bits when
// the span is swept.
func newAllocBits(nelems uintptr) *gcBits {
	return newMarkBits(nelems)
}

// nextMarkBitArenaEpoch establishes a new epoch for the arenas
// holding the mark bits. The arenas are named relative to the
// current GC cycle which is demarcated by the call to finishweep_m.
//
// All current spans have been swept.
// During that sweep each span allocated room for its gcmarkBits in
// gcBitsArenas.next block. gcBitsArenas.next becomes the gcBitsArenas.current
// where the GC will mark objects and after each span is swept these bits
// will be used to allocate objects.
// gcBitsArenas.current becomes gcBitsArenas.previous where the span's
// gcAllocBits live until all the spans have been swept during this GC cycle.
// The span's sweep extinguishes all the references to gcBitsArenas.previous
// by pointing gcAllocBits into the gcBitsArenas.current.
// The gcBitsArenas.previous is released to the gcBitsArenas.free list.
func nextMarkBitArenaEpoch() {
	lock(&gcBitsArenas.lock)
	if gcBitsArenas.previous != nil {
		if gcBitsArenas.free == nil {
			gcBitsArenas.free = gcBitsArenas.previous
		} else {
			// Find end of previous arenas.
			last := gcBitsArenas.previous
			for last = gcBitsArenas.previous; last.next != nil; last = last.next {
			}
			last.next = gcBitsArenas.free
			gcBitsArenas.free = gcBitsArenas.previous
		}
	}
	gcBitsArenas.previous = gcBitsArenas.current
	gcBitsArenas.current = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), nil) // newMarkBits calls newArena when needed
	unlock(&gcBitsArenas.lock)
}

// newArenaMayUnlock allocates and zeroes a gcBits arena.
// The caller must hold gcBitsArena.lock. This may temporarily release it.
func newArenaMayUnlock() *gcBitsArena {
	var result *gcBitsArena
	if gcBitsArenas.free == nil {
		unlock(&gcBitsArenas.lock)
		result = (*gcBitsArena)(sysAlloc(gcBitsChunkBytes, &memstats.gc_sys))
		if result == nil {
			throw("runtime: cannot allocate memory")
		}
		lock(&gcBitsArenas.lock)
	} else {
		result = gcBitsArenas.free
		gcBitsArenas.free = gcBitsArenas.free.next
		memclrNoHeapPointers(unsafe.Pointer(result), gcBitsChunkBytes)
	}
	result.next = nil
	// If result.bits is not 8 byte aligned adjust index so
	// that &result.bits[result.free] is 8 byte aligned.
	if uintptr(unsafe.Offsetof(gcBitsArena{}.bits))&7 == 0 {
		result.free = 0
	} else {
		result.free = 8 - (uintptr(unsafe.Pointer(&result.bits[0])) & 7)
	}
	return result
}
