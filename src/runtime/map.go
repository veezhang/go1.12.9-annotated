// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go's map type.
//
// A map is just a hash table. The data is arranged
// into an array of buckets. Each bucket contains up to
// 8 key/value pairs. The low-order bits of the hash are
// used to select a bucket. Each bucket contains a few
// high-order bits of each hash to distinguish the entries
// within a single bucket.
//
// If more than 8 keys hash to a bucket, we chain on
// extra buckets.
//
// When the hashtable grows, we allocate a new array
// of buckets twice as big. Buckets are incrementally
// copied from the old bucket array to the new bucket array.
//
// Map iterators walk through the array of buckets and
// return the keys in walk order (bucket #, then overflow
// chain order, then bucket index).  To maintain iteration
// semantics, we never move keys within their bucket (if
// we did, keys might be returned 0 or 2 times).  When
// growing the table, iterators remain iterating through the
// old table and must check the new table if the bucket
// they are iterating through has been moved ("evacuated")
// to the new table.

// Picking loadFactor: too large and we have lots of overflow
// buckets, too small and we waste a lot of space. I wrote
// a simple program to check some stats for different loads:
// (64-bit, 8 byte keys and values)
//  loadFactor    %overflow  bytes/entry     hitprobe    missprobe
//        4.00         2.13        20.77         3.00         4.00
//        4.50         4.05        17.30         3.25         4.50
//        5.00         6.85        14.77         3.50         5.00
//        5.50        10.55        12.94         3.75         5.50
//        6.00        15.27        11.67         4.00         6.00
//        6.50        20.90        10.79         4.25         6.50
//        7.00        27.14        10.15         4.50         7.00
//        7.50        34.03         9.73         4.75         7.50
//        8.00        41.10         9.40         5.00         8.00
//
// %overflow   = percentage of buckets which have an overflow bucket
// bytes/entry = overhead bytes used per key/value pair
// hitprobe    = # of entries to check when looking up a present key
// missprobe   = # of entries to check when looking up an absent key
//
// Keep in mind this data is for maximally loaded tables, i.e. just
// before the table grows. Typical tables will be somewhat less loaded.

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// Maximum number of key/value pairs a bucket can hold.
	bucketCntBits = 3
	bucketCnt     = 1 << bucketCntBits // 哈希桶的数量 8

	// Maximum average load of a bucket that triggers growth is 6.5.
	// Represent as loadFactorNum/loadFactDen, to allow integer math.
	// 最大的负载因子为 6.5 ，转换为 loadFactorNum/loadFactDen ，来支持整型计算
	loadFactorNum = 13
	loadFactorDen = 2

	// Maximum key or value size to keep inline (instead of mallocing per element).
	// Must fit in a uint8.
	// Fast versions cannot handle big values - the cutoff size for
	// fast versions in cmd/compile/internal/gc/walk.go must be at most this value.
	// key value 能够内联的最大内存大小（而不是为每个元素分配）
	maxKeySize   = 128
	maxValueSize = 128

	// data offset should be the size of the bmap struct, but needs to be
	// aligned correctly. For amd64p32 this means 64-bit alignment
	// even though pointers are 32 bit.
	// 数据的偏移
	dataOffset = unsafe.Offsetof(struct {
		b bmap
		v int64
	}{}.v)

	// Possible tophash values. We reserve a few possibilities for special marks.
	// Each bucket (including its overflow buckets, if any) will have either all or none of its
	// entries in the evacuated* states (except during the evacuate() method, which only happens
	// during map writes and thus no one else can observe the map during that time).
	// tophash 可能的值，以下是一些特殊标记，只有在 evacuate() 函数期间才有 evacuated* 值
	// emptyRest 		此单元格为空，并且高索引和overflows也没有非空单元格可
	// emptyOne 		此单元格为空
	// evacuatedX		此单元格已经迁移了，进行的是平移迁移，也就是迁移到相同的下标下
	// evacuatedY		此单元格已经迁移了，进行的是向后迁移，也就是迁移到相同的下标后面
	// evacuatedEmpty	此单元格已经迁移了，原来的没有 emptyOne
	// minTopHash 		tophash 最小的值
	emptyRest      = 0 // this cell is empty, and there are no more non-empty cells at higher indexes or overflows.
	emptyOne       = 1 // this cell is empty
	evacuatedX     = 2 // key/value is valid.  Entry has been evacuated to first half of larger table.
	evacuatedY     = 3 // same as above, but evacuated to second half of larger table.
	evacuatedEmpty = 4 // cell is empty, bucket is evacuated.
	minTopHash     = 5 // minimum tophash for a normal filled cell.

	// flags
	iterator     = 1 // there may be an iterator using buckets						// 可能在迭代 buckets
	oldIterator  = 2 // there may be an iterator using oldbuckets					// 可能在迭代 oldbuckets
	hashWriting  = 4 // a goroutine is writing to the map							// 有 goroutine 在写 map
	sameSizeGrow = 8 // the current map growth is to a new map of the same size		// 相同大小的扩容，扩容前后哈希桶数量相等，空的k/v太多了，做清理

	// sentinel bucket ID for iterator checks
	noCheck = 1<<(8*sys.PtrSize) - 1 // 哨兵标志
)

// isEmpty reports whether the given tophash array entry represents an empty bucket entry.
// isEmpty 返回 tophash 是否为空
func isEmpty(x uint8) bool {
	return x <= emptyOne
}

// A header for a Go map.
// GO map 头信息
type hmap struct {
	// Note: the format of the hmap is also encoded in cmd/compile/internal/gc/reflect.go.
	// Make sure this stays in sync with the compiler's definition.
	count     int    // # live cells == size of map.  Must be first (used by len() builtin) 		// Map中元素数量
	flags     uint8  //																				// 读、写、扩容、迭代等标记，用于记录map当前状态
	B         uint8  // log_2 of # of buckets (can hold up to loadFactor * 2^B items)				// 2^B 为桶的数量，（1<< B * 6.5）为最多元素的数量
	noverflow uint16 // approximate number of overflow buckets; see incrnoverflow for details		// 溢出桶个数，当溢出桶个数过多时，这个值是一个近似值
	hash0     uint32 // hash seed																	// 计算key哈希值的随机值，保证一个key在不同map中存放的位置是随机的

	buckets    unsafe.Pointer // array of 2^B Buckets. may be nil if count==0.						// 桶指针
	oldbuckets unsafe.Pointer // previous bucket array of half the size, non-nil only when growing	// 桶指针（只有扩容的时候才使用，指向旧的桶）
	nevacuate  uintptr        // progress counter for evacuation (buckets less than this have been evacuated) // 用于桶迁移，已迁移哈希桶个数

	extra *mapextra // optional fields			// 当 bucket 中元素超过8个元素，通过 extra 来扩展 bucket
}

// mapextra holds fields that are not present on all maps.
// mapextra 记录不在 maps 中的字段
type mapextra struct {
	// If both key and value do not contain pointers and are inline, then we mark bucket
	// type as containing no pointers. This avoids scanning such maps.
	// However, bmap.overflow is a pointer. In order to keep overflow buckets
	// alive, we store pointers to all overflow buckets in hmap.extra.overflow and hmap.extra.oldoverflow.
	// overflow and oldoverflow are only used if key and value do not contain pointers.
	// overflow contains overflow buckets for hmap.buckets.
	// oldoverflow contains overflow buckets for hmap.oldbuckets.
	// 如果 key 和 value 都不包含指针，并且可以 inline (<=128字节)，那么就标记 bucket(bmap) 为没有指针的类型，这样就可以
	// 避免 GC 扫描整个 map 。然而 bmap.overflow 也是一个指针，所以就将其放到 hmap.extra.overflow 和 hmap.extra.oldoverflow
	// 中，来保持引用，不会被 GC 回收了。
	// 当kev/value不为指针时，溢出桶存放到mapextra结构中，overflow存放buckets中的溢出桶，oldoverflow存放oldbuckets中的溢出桶
	overflow    *[]*bmap // 以切片形式存放buckets中的每个溢出桶
	oldoverflow *[]*bmap // 以切片形式存放oldbuckets中的每个溢出桶

	// nextOverflow holds a pointer to a free overflow bucket.
	// nextOverflow 指向空闲溢出桶 ，预分配的
	nextOverflow *bmap
}

// A bucket for a Go map.
// Go Map 桶结构
type bmap struct {
	// tophash generally contains the top byte of the hash value
	// for each key in this bucket. If tophash[0] < minTopHash,
	// tophash[0] is a bucket evacuation state instead.
	// tophash 通常包含 hash 值的高 8 位 ， 如果 tophash[0] < minTopHash ，则 tophash[0] 是迁移状态
	tophash [bucketCnt]uint8 // bucketCnt = 8
	// Followed by bucketCnt keys and then bucketCnt values.
	// NOTE: packing all the keys together and then all the values together makes the
	// code a bit more complicated than alternating key/value/key/value/... but it allows
	// us to eliminate padding which would be needed for, e.g., map[int64]int8.
	// Followed by an overflow pointer.
	// 接下来就是 bucketCnt 个 keys，再就是 bucketCnt 个 values。
	// 所有的 keys 一起，所有的 value 在一起，而不是 key/value 相间，可以消除填充。
	// 后面再跟一个溢出指针 overflow ，overflow 组成链
	//
	// tophash[0] ... tophash[7] key[0] ... key[7] value[0] ... value[7] [overflow *bmap]
}

// A hash iteration structure.
// If you modify hiter, also change cmd/compile/internal/gc/reflect.go to indicate
// the layout of this structure.
// 迭代器
type hiter struct {
	// key, value 必须在第一和第二个字段，在 cmd/compile/internal/gc/range.go 中会这么来获取 key value
	key         unsafe.Pointer // Must be in first position.  Write nil to indicate iteration end (see cmd/internal/gc/range.go).
	value       unsafe.Pointer // Must be in second position (see cmd/internal/gc/range.go).
	t           *maptype       // map 类型
	h           *hmap          // 对应的 map
	buckets     unsafe.Pointer // bucket ptr at hash_iter initialization time 		// 初始化的时候设置的 buckets
	bptr        *bmap          // current bucket									// 当前的 bucket
	overflow    *[]*bmap       // keeps overflow buckets of hmap.buckets alive		// 让 hmap.buckets alive
	oldoverflow *[]*bmap       // keeps overflow buckets of hmap.oldbuckets alive	// 让 hmap.buckets alive
	startBucket uintptr        // bucket iteration started at						// 开始的 bucket
	offset      uint8          // intra-bucket offset to start from during iteration (should be big enough to hold bucketCnt-1) // bucket 内偏移
	wrapped     bool           // already wrapped around from end of bucket array to beginning // 是否以及从最后遍历到前面来了，以及绕回来了
	B           uint8          // B
	i           uint8          // i
	bucket      uintptr        // bucket 位置
	checkBucket uintptr        // checkBucket
}

// bucketShift returns 1<<b, optimized for code generation.
func bucketShift(b uint8) uintptr {
	if sys.GoarchAmd64|sys.GoarchAmd64p32|sys.Goarch386 != 0 {
		b &= sys.PtrSize*8 - 1 // help x86 archs remove shift overflow checks
	}
	return uintptr(1) << b
}

// bucketMask returns 1<<b - 1, optimized for code generation.
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}

// tophash calculates the tophash value for hash.
// tophash 计算 hash值的 tophash
func tophash(hash uintptr) uint8 {
	// 取高 8 位作为 tophash
	top := uint8(hash >> (sys.PtrSize*8 - 8))
	// 如果小于 minTopHas 则加上 minTopHash ，小于 minTopHash 为特殊标记
	if top < minTopHash {
		top += minTopHash
	}
	return top
}

// evacuated 判断是否已经迁移
func evacuated(b *bmap) bool {
	h := b.tophash[0]
	return h > emptyOne && h < minTopHash
}

// 返回 bmap 的溢出指针
func (b *bmap) overflow(t *maptype) *bmap {
	// 溢出指针在 bmap 的最后面
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize))
}

// 设置 bmap 的溢出指针
func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize)) = ovf
}

// 返回 bmap 的 keys 头指针
func (b *bmap) keys() unsafe.Pointer {
	return add(unsafe.Pointer(b), dataOffset)
}

// incrnoverflow increments h.noverflow.
// noverflow counts the number of overflow buckets.
// This is used to trigger same-size map growth.
// See also tooManyOverflowBuckets.
// To keep hmap small, noverflow is a uint16.
// When there are few buckets, noverflow is an exact count.
// When there are many buckets, noverflow is an approximate count.
// incrnoverflow 增加 h.noverflow 。noverflow 统计溢出桶个数。用来触发相同大小的 map 扩容。
// 当桶小的时候，noverflow 是精确值；当 桶很多的时候，noverflow 是近似值。
func (h *hmap) incrnoverflow() {
	// We trigger same-size map growth if there are
	// as many overflow buckets as buckets.
	// We need to be able to count to 1<<h.B.
	// 如果溢出桶和桶的数量一样，则触发相同大小的 map 扩容。我们需要统计到 1<<h.B 。
	// 如果 h.B < 16 ，则直接 noverflow++ 。
	if h.B < 16 {
		h.noverflow++
		return
	}
	// Increment with probability 1/(1<<(h.B-15)).
	// When we reach 1<<15 - 1, we will have approximately
	// as many overflow buckets as buckets.
	// 增加的概率为： 1/(1<<(h.B-15)) 。当达到 1<<15 - 1 ，将有大约和桶一样多的溢出桶。
	mask := uint32(1)<<(h.B-15) - 1
	// Example: if h.B == 18, then mask == 7,
	// and fastrand & 7 == 0 with probability 1/8.
	if fastrand()&mask == 0 {
		h.noverflow++
	}
}

// 新建溢出桶
func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
	var ovf *bmap
	if h.extra != nil && h.extra.nextOverflow != nil {
		// We have preallocated overflow buckets available.
		// See makeBucketArray for more details.
		// 有预分配的溢出桶
		ovf = h.extra.nextOverflow
		if ovf.overflow(t) == nil {
			// We're not at the end of the preallocated overflow buckets. Bump the pointer.
			// 不是最后一个预分配的溢出桶，将后续的设置到 h.extra.nextOverflow
			// makeBucketArray 中可以看到，创建的溢出桶中，只有最后一个设置了 overflow ，所以如果为 nil 表示不是最后一个
			h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf), uintptr(t.bucketsize)))
		} else {
			// This is the last preallocated overflow bucket.
			// Reset the overflow pointer on this bucket,
			// which was set to a non-nil sentinel value.
			// 这个是最后预分配的溢出桶，
			ovf.setoverflow(t, nil)
			h.extra.nextOverflow = nil
		}
	} else {
		// 没有预分配的溢出桶， 则新建一个
		ovf = (*bmap)(newobject(t.bucket))
	}
	// 增加 noverflow
	h.incrnoverflow()
	// 如果不是指针，则加入到 h.extra.overflow
	if t.bucket.kind&kindNoPointers != 0 {
		h.createOverflow()
		*h.extra.overflow = append(*h.extra.overflow, ovf)
	}
	b.setoverflow(t, ovf)
	return ovf
}

// createOverflow 创建溢出桶，初始化下 h.extra.overflow
func (h *hmap) createOverflow() {
	if h.extra == nil {
		h.extra = new(mapextra)
	}
	if h.extra.overflow == nil {
		h.extra.overflow = new([]*bmap)
	}
}

// make map
func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	if int64(int(hint)) != hint {
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// makehmap_small implements Go map creation for make(map[k]v) and
// make(map[k]v, hint) when hint is known to be at most bucketCnt
// at compile time and the map needs to be allocated on the heap.
// makehmap_small 实现 make(map[k]v) 和 (map[k]v, hint) hint<=bucketCnt ， 并不初始化桶。
//
func makemap_small() *hmap {
	h := new(hmap)
	h.hash0 = fastrand() // 初始化随机种子
	return h
}

// makemap implements Go map creation for make(map[k]v, hint).
// If the compiler has determined that the map or the first bucket
// can be created on the stack, h and/or bucket may be non-nil.
// If h != nil, the map can be created directly in h.
// If h.buckets != nil, bucket pointed to can be used as the first bucket.
// makemap 实现 make(map[k]v, hint)
func makemap(t *maptype, hint int, h *hmap) *hmap {
	// 计算内存大小 hint * t.bucket.size
	mem, overflow := math.MulUintptr(uintptr(hint), t.bucket.size)
	if overflow || mem > maxAlloc { // 溢出或超过最大内存限制
		hint = 0
	}

	// initialize Hmap
	// 初始化 hmap
	if h == nil {
		h = new(hmap)
	}
	h.hash0 = fastrand() // 初始化随机种子

	// Find the size parameter B which will hold the requested # of elements.
	// For hint < 0 overLoadFactor returns false since hint < bucketCnt.
	// 计算 B
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++
	}
	h.B = B

	// allocate initial hash table
	// if B == 0, the buckets field is allocated lazily later (in mapassign)
	// If hint is large zeroing this memory could take a while.
	// 分配初始化哈希表。B=0的时候，在 mapassign 延迟分配
	if h.B != 0 { // B != 0 时初始化桶指针buckets
		var nextOverflow *bmap
		// 分配 buckets
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			// 如果有多余的，设置到 nextOverflow
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}

// makeBucketArray initializes a backing array for map buckets.
// 1<<b is the minimum number of buckets to allocate.
// dirtyalloc should either be nil or a bucket array previously
// allocated by makeBucketArray with the same t and b parameters.
// If dirtyalloc is nil a new backing array will be alloced and
// otherwise dirtyalloc will be cleared and reused as backing array.
// makeBucketArray 初始化哈希桶
// 1<<b 是最小分配数目
// 参数 dirtyalloc 要么为空，要么是之前使用相同的 t 和 b 参数调用 makeBucketArray 分配的哈希桶数组。
// dirtyalloc 为 nil ，生产新的； 否则会清除作为
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	// base 代表用户预期的桶的数量，即hash数组的真实大小，2^B
	base := bucketShift(b)
	nbuckets := base
	// For small b, overflow buckets are unlikely.
	// Avoid the overhead of the calculation.
	// 对于小 b 不太可能有溢出桶，避免计算开销。 也就是 base <= 8 的时候
	if b >= 4 {
		// Add on the estimated number of overflow buckets
		// required to insert the median number of elements
		// used with this value of b.
		// 加上一些预计的溢出桶数。
		nbuckets += bucketShift(b - 4)
		// 计算需要的内存
		sz := t.bucket.size * nbuckets
		// 计算 mallocgc 分配的实际大小
		up := roundupsize(sz)
		if up != sz {
			// 重新计算 nbuckets
			nbuckets = up / t.bucket.size
		}
	}

	if dirtyalloc == nil {
		buckets = newarray(t.bucket, int(nbuckets))
	} else {
		// dirtyalloc was previously generated by
		// the above newarray(t.bucket, int(nbuckets))
		// but may not be empty.
		// dirtyalloc 由上面 newarray 生产的，可能不为空
		buckets = dirtyalloc
		size := t.bucket.size * nbuckets
		// 根据是由有指针，清空下
		if t.bucket.kind&kindNoPointers == 0 {
			memclrHasPointers(buckets, size)
		} else {
			memclrNoHeapPointers(buckets, size)
		}
	}

	// 如果 nbuckets 不等于 base， 则把多余的放到 nextOverflow 中
	if base != nbuckets {
		// We preallocated some overflow buckets.
		// To keep the overhead of tracking these overflow buckets to a minimum,
		// we use the convention that if a preallocated overflow bucket's overflow
		// pointer is nil, then there are more available by bumping the pointer.
		// We need a safe non-nil pointer for the last overflow bucket; just use buckets.
		// 预分配一些溢出桶
		// 为了最小化跟踪溢出桶的开销，规定：如果溢出桶的 overflow 指针为空，则后面还有更多的溢出桶，
		// 在最后设置 overflow 为非空的指针，直接用 buckets 就好了。
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.bucketsize)))
		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.bucketsize)))
		last.setoverflow(t, (*bmap)(buckets))
	}
	return buckets, nextOverflow
}

// mapaccess1 returns a pointer to h[key].  Never returns nil, instead
// it will return a reference to the zero object for the value type if
// the key is not in the map.
// NOTE: The returned pointer may keep the whole map live, so don't
// hold onto it for very long.
// mapaccess1 返回 h[key] 。当 key 不存在 map 中，永远不会返回 nil ，而是返回零值（zeroVal）的引用。
// 注意：返回的值不要一直持有，这样会让 map 一直存活。
// mapaccess1 为 v := m[k] 的时候调用，只返回值
func mapaccess1(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess1)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	// h 为空，返回 zeroVal
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.key.alg.hash(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0])
	}
	// 如果处于写状态，panic
	if h.flags&hashWriting != 0 {
		throw("concurrent map read and map write")
	}
	alg := t.key.alg
	hash := alg.hash(key, uintptr(h.hash0))                      // 计算哈希值
	m := bucketMask(h.B)                                         // 获取掩码
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.bucketsize))) // 通过低 hash&m (也就是低 B 位) 来计算处于哪一个槽，得到哈希桶
	if c := h.oldbuckets; c != nil {
		// 在扩容，判断是否为相等大小扩容
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			// 这里是翻倍扩容，B 已经+1了，所以需要 m>>1
			m >>= 1
		}
		// 计算之前的哈希桶
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.bucketsize)))
		// 如果还没有迁移过，则 b = oldb
		if !evacuated(oldb) {
			b = oldb
		}
	}
	// 获取高 8 位
	top := tophash(hash)
bucketloop:
	// 遍历哈希桶和对应的溢出桶（也是哈希桶），来查找
	for ; b != nil; b = b.overflow(t) {
		// 遍历哈希桶 8 个位置
		for i := uintptr(0); i < bucketCnt; i++ {
			// 存高 8 位，是为了快速判断，过滤不满足的
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					// emptyRest 表示高索引和后面的溢出桶都没有非空的元素了，直接跳出循环，未找到
					break bucketloop
				}
				continue
			}
			// 此时 b.tophash[i] == top ，再判断 key 值是否相等
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				// k 是指针类型，解指针，获取对应的值
				k = *((*unsafe.Pointer)(k))
			}
			// 判断是否相等
			if alg.equal(key, k) {
				// 找到了，返回 value
				v := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.valuesize))
				if t.indirectvalue() {
					// v 是指针类型，解指针，获取对应的值
					v = *((*unsafe.Pointer)(v))
				}
				return v
			}
		}
	}
	// 未找到
	return unsafe.Pointer(&zeroVal[0])
}

// mapaccess2 为 v,ok := m[k] 的时候调用，返回值和是否找到。 和 mapaccess1 实现几乎一致。
func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess2)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.key.alg.hash(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0]), false
	}
	if h.flags&hashWriting != 0 {
		throw("concurrent map read and map write")
	}
	alg := t.key.alg
	hash := alg.hash(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + (hash&m)*uintptr(t.bucketsize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(unsafe.Pointer(uintptr(c) + (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if alg.equal(key, k) {
				v := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.valuesize))
				if t.indirectvalue() {
					v = *((*unsafe.Pointer)(v))
				}
				return v, true
			}
		}
	}
	return unsafe.Pointer(&zeroVal[0]), false
}

// returns both key and value. Used by map iterator
// mapaccessK 返回 key 和 value ，迭代器中使用
func mapaccessK(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer) {
	if h == nil || h.count == 0 {
		return nil, nil
	}
	alg := t.key.alg
	hash := alg.hash(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + (hash&m)*uintptr(t.bucketsize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(unsafe.Pointer(uintptr(c) + (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if alg.equal(key, k) {
				v := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.valuesize))
				if t.indirectvalue() {
					v = *((*unsafe.Pointer)(v))
				}
				return k, v
			}
		}
	}
	return nil, nil
}

// cmd/compile/internal/gc/walk.go 中可以看到，当 val > 1024(maxZero，也就是 sizeof(zeroVal))的时候调用这个
// 否则调用 mapaccess1 ，> 1024 就不能引用 maxZero 了
func mapaccess1_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) unsafe.Pointer {
	v := mapaccess1(t, h, key)
	if v == unsafe.Pointer(&zeroVal[0]) {
		return zero
	}
	return v
}

func mapaccess2_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) (unsafe.Pointer, bool) {
	v := mapaccess1(t, h, key)
	if v == unsafe.Pointer(&zeroVal[0]) {
		return zero, false
	}
	return v, true
}

// Like mapaccess, but allocates a slot for the key if it is not present in the map.
// mapassign 类似于 mapaccess ，但是当没有 key ，也分配对应的单元
// mapassign 赋值： 实现是查找一个空的bucket，把key赋值到bucket上，然后把val的地址返回，然后直接通过汇编做内存拷贝
func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if h == nil {
		panic(plainError("assignment to entry in nil map"))
	}
	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(mapassign)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled {
		msanread(key, t.key.size)
	}
	// 并发写
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}
	alg := t.key.alg
	hash := alg.hash(key, uintptr(h.hash0))

	// Set hashWriting after calling alg.hash, since alg.hash may panic,
	// in which case we have not actually done a write.
	// 调用 alg.hash 后再设置 hashWriting ，因为 alg.hash 可能 panic ，在这种情况下，我们还没有真正的写数据
	h.flags ^= hashWriting

	if h.buckets == nil {
		h.buckets = newobject(t.bucket) // newarray(t.bucket, 1)
	}

again:
	// 计算 hash 对应的哈希桶在 map 中的的位置
	bucket := hash & bucketMask(h.B)
	// 如果在迁移
	if h.growing() {
		// 确保 bucket 对应的 oldbucket 已经迁移完了
		growWork(t, h, bucket)
	}
	// 获取哈希桶
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + bucket*uintptr(t.bucketsize)))
	top := tophash(hash) // 获取高 8 位，用于快速过来不满足的

	var inserti *uint8         // 插入的索引位置
	var insertk unsafe.Pointer // 插入的 key 的位置
	var val unsafe.Pointer     // 插入的 val 的位置
bucketloop:
	// 遍历哈希桶和对应的溢出桶（也是哈希桶），来查找
	for {
		// 遍历哈希桶 8 个位置
		for i := uintptr(0); i < bucketCnt; i++ {
			// 存高 8 位，是为了快速判断，过滤不满足的
			if b.tophash[i] != top {
				// 如果为 nil ，则记录插入位置
				if isEmpty(b.tophash[i]) && inserti == nil {
					inserti = &b.tophash[i]
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
					val = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.valuesize))
				}
				if b.tophash[i] == emptyRest {
					// emptyRest 表示高索引和后面的溢出桶都没有非空的元素了，直接跳出循环，未找到
					break bucketloop
				}
				continue
			}
			// 此时 b.tophash[i] == top ，再判断 key 值是否相等
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				// k 是指针类型，解指针，获取对应的值
				k = *((*unsafe.Pointer)(k))
			}
			// 判断是否相等，不相等则继续找
			if !alg.equal(key, k) {
				continue
			}
			// 此处 key == k
			// already have a mapping for key. Update it.
			// 已经有对应的映射， 判断 key 是否要更新，如果需要则更新下
			// 可以参考：src/cmd/compile/internal/gc/reflect.go
			// 当key类型为float32\float64\complex64\complex64\interface\string，或字段里面包含这些类型的都需要更新key。
			if t.needkeyupdate() {
				typedmemmove(t.key, k, key)
			}
			// 获取插入 val 的指针
			val = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.valuesize))
			// 找到了之前的值
			goto done
		}
		// 迭代溢出桶
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// Did not find mapping for key. Allocate new cell & add entry.
	// 没有找到 key 的映射，分配并添加

	// If we hit the max load factor or we have too many overflow buckets,
	// and we're not already in the middle of growing, start growing.
	// 如果达到了负载因子或者有太多的溢出桶，并且不在扩容的时候，那么开始扩容吧
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		hashGrow(t, h)
		goto again // Growing the table invalidates everything, so try again // 扩容的过程导致之前的无效，再次尝试
	}

	// 如果没有找到插入位置，则新建一个溢出桶，并记录插入位置为新溢出桶的第一个位置
	if inserti == nil {
		// all current buckets are full, allocate a new one.
		newb := h.newoverflow(t, b)
		inserti = &newb.tophash[0]
		insertk = add(unsafe.Pointer(newb), dataOffset)
		val = add(insertk, bucketCnt*uintptr(t.keysize))
	}

	// store new key/value at insert position
	if t.indirectkey() {
		// key 是指针类型，设置对应的零指针
		kmem := newobject(t.key)
		*(*unsafe.Pointer)(insertk) = kmem
		insertk = kmem
	}
	if t.indirectvalue() {
		// value 是指针类型，设置对应的零指针
		vmem := newobject(t.elem)
		*(*unsafe.Pointer)(val) = vmem
	}
	// 设置 key, tophash, count
	typedmemmove(t.key, insertk, key)
	*inserti = top
	h.count++

done:
	// 并发写
	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	// &^ 是 bit clear ，清除 flags 中hashWriting 中为 1 的位，也就是清除 hashWriting 标记
	h.flags &^= hashWriting
	if t.indirectvalue() {
		// val 是指针类型，解指针，获取对应的值
		val = *((*unsafe.Pointer)(val))
	}
	// 返回的是 value 对应的指针，汇编会负责去填充
	return val
}

// map 删除
func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapdelete)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	// map 为空，直接返回
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.key.alg.hash(key, 0) // see issue 23734
		}
		return
	}
	// 并发写
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	alg := t.key.alg
	hash := alg.hash(key, uintptr(h.hash0))

	// Set hashWriting after calling alg.hash, since alg.hash may panic,
	// in which case we have not actually done a write (delete).
	// 调用 alg.hash 后再设置 hashWriting ，因为 alg.hash 可能 panic ，在这种情况下，我们还没有真正的写数据
	h.flags ^= hashWriting

	bucket := hash & bucketMask(h.B)
	if h.growing() {
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize)))
	bOrig := b
	top := tophash(hash)
search:
	// 遍历哈希桶和对应的溢出桶（也是哈希桶），来查找
	for ; b != nil; b = b.overflow(t) {
		// 遍历哈希桶 8 个位置
		for i := uintptr(0); i < bucketCnt; i++ {
			// 存高 8 位，是为了快速判断，过滤不满足的
			if b.tophash[i] != top {
				// emptyRest 表示高索引和后面的溢出桶都没有非空的元素了，直接跳出循环，未找到
				if b.tophash[i] == emptyRest {
					break search
				}
				continue
			}
			// 此时 b.tophash[i] == top ，再判断 key 值是否相等
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			k2 := k
			if t.indirectkey() {
				// k 是指针类型，解指针，获取对应的值
				k2 = *((*unsafe.Pointer)(k2))
			}
			// 判断是否相等，不相等则继续找
			if !alg.equal(key, k2) {
				continue
			}
			// Only clear key if there are pointers in it.
			// 只有当 key 包含指针的时候才清除
			if t.indirectkey() {
				// key 是指针类型，设置对应的指针为 nil 即可
				*(*unsafe.Pointer)(k) = nil
			} else if t.key.kind&kindNoPointers == 0 {
				// key 是包含指针的类型， 调用 memclrHasPointers 来清理
				memclrHasPointers(k, t.key.size)
			}
			// 获取 value 对应的地址，并清理
			v := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.valuesize))
			if t.indirectvalue() {
				// value 是指针类型，设置对应的指针为 nil 即可
				*(*unsafe.Pointer)(v) = nil
			} else if t.elem.kind&kindNoPointers == 0 {
				// value 是包含指针的类型， 调用 memclrHasPointers 来清理
				memclrHasPointers(v, t.elem.size)
			} else {
				// 否则调用 memclrNoHeapPointers 来清理
				memclrNoHeapPointers(v, t.elem.size)
			}
			// 设置 emptyOne 标记
			b.tophash[i] = emptyOne
			// If the bucket now ends in a bunch of emptyOne states,
			// change those to emptyRest states.
			// It would be nice to make this a separate function, but
			// for loops are not currently inlineable.
			// 如果是是以一堆 emptyOne 状态结束，修改成 emptyRest 状态
			if i == bucketCnt-1 {
				// 后面还有溢出桶 		 && 溢出桶中还有非空的
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					goto notLast // 不是最后一个非空的
				}
			} else {
				// 后面还有非空的
				if b.tophash[i+1] != emptyRest {
					goto notLast // 不是最后一个非空的
				}
			}
			// 表示 b.tophash[i] 后面都是空的
			// 然后，又将此值删除了，那么往前走，将满足的都设置为 emptyRest
			for {
				b.tophash[i] = emptyRest
				// i == 0，找前面一个哈希桶
				if i == 0 {
					// 找到头了
					if b == bOrig {
						break // beginning of initial bucket, we're done.
					}
					// Find previous bucket, continue at its last entry.
					// 找前一个哈希桶
					c := b
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
					}
					i = bucketCnt - 1
				} else {
					// 哈希桶内，往前移一格
					i--
				}
				// 找到不为 emptyOne 的时候，跳出循环
				if b.tophash[i] != emptyOne {
					break
				}
			}
		notLast:
			// 数量减 1 ，再跳出循环
			h.count--
			break search
		}
	}

	// 并发写
	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	// &^ 是 bit clear ，清除 flags 中 hashWriting 中为 1 的位，也就是清除 hashWriting 标记
	h.flags &^= hashWriting
}

// mapiterinit initializes the hiter struct used for ranging over maps.
// The hiter struct pointed to by 'it' is allocated on the stack
// by the compilers order pass or on the heap by reflect_mapiterinit.
// Both need to have zeroed hiter since the struct contains pointers.
// mapiterinit 初始化 hiter 结构体，用来 for :=range m 使用。
// 指向 hiter 的 it 通过编译器在栈上分配，或者通过 reflect_mapiterinit 在堆上分配，都需要清零。
func mapiterinit(t *maptype, h *hmap, it *hiter) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiterinit))
	}
	// map 为空，返回
	if h == nil || h.count == 0 {
		return
	}

	if unsafe.Sizeof(hiter{})/sys.PtrSize != 12 {
		throw("hash_iter size incorrect") // see cmd/compile/internal/gc/reflect.go
	}
	it.t = t
	it.h = h

	// grab snapshot of bucket state
	// 创建当前 map 状态快照
	it.B = h.B
	it.buckets = h.buckets
	if t.bucket.kind&kindNoPointers != 0 {
		// Allocate the current slice and remember pointers to both current and old.
		// This preserves all relevant overflow buckets alive even if
		// the table grows and/or overflow buckets are added to the table
		// while we are iterating.
		// 分配当前切片并将指针记住到当前和老的。
		// 这将使得所有相关的溢出桶 alive ，即使在迭代的时候 map 扩容、添加溢出桶。
		h.createOverflow()
		it.overflow = h.extra.overflow
		it.oldoverflow = h.extra.oldoverflow
	}

	// decide where to start
	// 决定从哪里开始，生产随机数
	r := uintptr(fastrand())
	if h.B > 31-bucketCntBits {
		r += uintptr(fastrand()) << 31
	}
	// 开始遍历的 bucket 以及 bucket 中的 offset
	it.startBucket = r & bucketMask(h.B)
	it.offset = uint8(r >> h.B & (bucketCnt - 1))

	// iterator state
	// 迭代状态
	it.bucket = it.startBucket

	// Remember we have an iterator.
	// Can run concurrently with another mapiterinit().
	// 以及有一个迭代，可以并发 mapiterinit
	if old := h.flags; old&(iterator|oldIterator) != iterator|oldIterator {
		atomic.Or8(&h.flags, iterator|oldIterator)
	}
	// 迭代下一个
	mapiternext(it)
}

// 迭代下一个
func mapiternext(it *hiter) {
	h := it.h
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiternext))
	}
	// 并发迭代与写
	if h.flags&hashWriting != 0 {
		throw("concurrent map iteration and map write")
	}
	// 初始值
	t := it.t
	bucket := it.bucket
	b := it.bptr
	i := it.i
	checkBucket := it.checkBucket
	alg := t.key.alg

next:
	//
	if b == nil {
		// 判断是否已经循环遍历所有的 bucket
		if bucket == it.startBucket && it.wrapped {
			// end of iteration
			// 结束迭代
			it.key = nil
			it.value = nil
			return
		}
		if h.growing() && it.B == h.B {
			// Iterator was started in the middle of a grow, and the grow isn't done yet.
			// If the bucket we're looking at hasn't been filled in yet (i.e. the old
			// bucket hasn't been evacuated) then we need to iterate through the old
			// bucket and only return the ones that will be migrated to this bucket.
			oldbucket := bucket & it.h.oldbucketmask()
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
			// b 是否已经迁移
			if !evacuated(b) {
				// 还没有迁移
				checkBucket = bucket
			} else {
				// 已经迁移过，需要重新计算 b
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
				checkBucket = noCheck
			}
		} else {
			// 没有在迁移 或者
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
			checkBucket = noCheck
		}
		bucket++
		if bucket == bucketShift(it.B) {
			bucket = 0
			it.wrapped = true
		}
		i = 0
	}
	// 遍历哈希桶 8 个位置
	for ; i < bucketCnt; i++ {
		offi := (i + it.offset) & (bucketCnt - 1)
		// 跳过空位置
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty {
			// TODO: emptyRest is hard to use here, as we start iterating
			// in the middle of a bucket. It's feasible, just tricky.
			// emptyRest 在这里不好使用，因为我们在 bucket 中间开始迭代的。
			continue
		}
		// 获取 key
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.keysize))
		if t.indirectkey() {
			// k 是指针类型，解指针，获取对应的值
			k = *((*unsafe.Pointer)(k))
		}
		// 获取 value
		v := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+uintptr(offi)*uintptr(t.valuesize))

		// 过滤不会被迁移过来的数据
		if checkBucket != noCheck && !h.sameSizeGrow() {
			// Special case: iterator was started during a grow to a larger size
			// and the grow is not done yet. We're working on a bucket whose
			// oldbucket has not been evacuated yet. Or at least, it wasn't
			// evacuated when we started the bucket. So we're iterating
			// through the oldbucket, skipping any keys that will go
			// to the other new bucket (each oldbucket expands to two
			// buckets during a grow).
			if t.reflexivekey() || alg.equal(k, k) {
				// If the item in the oldbucket is not destined for
				// the current new bucket in the iteration, skip it.
				hash := alg.hash(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					continue
				}
			} else {
				// Hash isn't repeatable if k != k (NaNs).  We need a
				// repeatable and randomish choice of which direction
				// to send NaNs during evacuation. We'll use the low
				// bit of tophash to decide which way NaNs go.
				// NOTE: this case is why we need two evacuate tophash
				// values, evacuatedX and evacuatedY, that differ in
				// their low bit.
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					continue
				}
			}
		}
		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.reflexivekey() || alg.equal(k, k)) {
			// This is the golden data, we can return it.
			// OR
			// key!=key, so the entry can't be deleted or updated, so we can just return it.
			// That's lucky for us because when key!=key we can't look it up successfully.
			// 两中情况可以直接返回：
			// 1. 数据没有迁移
			// 2. key != key ，因此不能删除也不能更新
			it.key = k
			if t.indirectvalue() {
				// v 是指针类型，解指针，获取对应的值
				v = *((*unsafe.Pointer)(v))
			}
			it.value = v
		} else {
			// The hash table has grown since the iterator was started.
			// The golden data for this key is now somewhere else.
			// Check the current hash table for the data.
			// This code handles the case where the key
			// has been deleted, updated, or deleted and reinserted.
			// NOTE: we need to regrab the key as it has potentially been
			// updated to an equal() but not identical key (e.g. +0.0 vs -0.0).
			// 自从迭代器启动后，map已经扩容，key 对应的数据已经在其他地方了。 重新在 map 中查找。
			rk, rv := mapaccessK(t, h, k)
			if rk == nil {
				// 已经被删除
				continue // key has been deleted
			}
			it.key = rk
			it.value = rv
		}
		it.bucket = bucket
		if it.bptr != b { // avoid unnecessary write barrier; see issue 14921
			it.bptr = b
		}
		it.i = i + 1
		it.checkBucket = checkBucket
		return
	}
	// 继续遍历其他的溢出桶
	b = b.overflow(t)
	i = 0
	goto next
}

// mapclear deletes all keys from a map.
// mapclear 清空 map
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	// h 为空，返回
	if h == nil || h.count == 0 {
		return
	}
	// 并发写
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}
	// 设置 hashWriting 标志
	h.flags ^= hashWriting

	h.flags &^= sameSizeGrow
	h.oldbuckets = nil
	h.nevacuate = 0
	h.noverflow = 0
	h.count = 0

	// Keep the mapextra allocation but clear any extra information.
	// 清除 mapextra
	if h.extra != nil {
		*h.extra = mapextra{}
	}

	// makeBucketArray clears the memory pointed to by h.buckets
	// and recovers any overflow buckets by generating them
	// as if h.buckets was newly alloced.
	// makeBucketArray 清空 h.buckets 指向的内存，并且恢复所有溢出桶，就好像新分配了 h.buckets 一样。
	// 分配 buckets
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	// 如果有多余的，设置到 nextOverflow
	if nextOverflow != nil {
		// If overflow buckets are created then h.extra
		// will have been allocated during initial bucket creation.
		h.extra.nextOverflow = nextOverflow
	}

	// 并发写
	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	// &^ 是 bit clear ，清除 flags 中 hashWriting 中为 1 的位，也就是清除 hashWriting 标记
	h.flags &^= hashWriting
}

// map 扩容
func hashGrow(t *maptype, h *hmap) {
	// If we've hit the load factor, get bigger.
	// Otherwise, there are too many overflow buckets,
	// so keep the same number of buckets and "grow" laterally.
	// 如果达到了负载因子，则翻倍扩容，否则相同大小大小扩容 sameSizeGrow 。
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		bigger = 0
		h.flags |= sameSizeGrow
	}
	oldbuckets := h.buckets
	// 申请一个大数组，作为新的buckets
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	// flags 为清除 iterator 和 oldIterator 状态后的值
	flags := h.flags &^ (iterator | oldIterator)
	// 如果之前有 iterator 状态， 则更新为 oldIterator 状态
	if h.flags&iterator != 0 {
		flags |= oldIterator
	}
	// commit the grow (atomic wrt gc)
	// 提交扩容，更新字段
	h.B += bigger
	h.flags = flags
	h.oldbuckets = oldbuckets
	h.buckets = newbuckets
	h.nevacuate = 0
	h.noverflow = 0

	if h.extra != nil && h.extra.overflow != nil {
		// Promote current overflow buckets to the old generation.
		// 将当前溢出桶提升到老一代
		if h.extra.oldoverflow != nil {
			throw("oldoverflow is not nil")
		}
		h.extra.oldoverflow = h.extra.overflow
		h.extra.overflow = nil
	}
	// 跟新 nextOverflow
	if nextOverflow != nil {
		if h.extra == nil {
			h.extra = new(mapextra)
		}
		h.extra.nextOverflow = nextOverflow
	}

	// the actual copying of the hash table data is done incrementally
	// by growWork() and evacuate().
	// 实际的迁移工作是在 growWork() 和 evacuate() 中完成的
}

// overLoadFactor reports whether count items placed in 1<<B buckets is over loadFactor.
// overLoadFactor 判断是否超过负载因子
func overLoadFactor(count int, B uint8) bool {
	return count > bucketCnt && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// tooManyOverflowBuckets reports whether noverflow buckets is too many for a map with 1<<B buckets.
// Note that most of these overflow buckets must be in sparse use;
// if use was dense, then we'd have already triggered regular map growth.
// tooManyOverflowBuckets 判断溢出桶的数量对于 1<<B 个桶来说是否过多
// 注意，这些溢出桶中的大多数必须处于稀疏状态；如果使用密集，那么我们已经触发了常规 map 扩容。
// 如果 noverflow >= 哈希桶数 ，则认为过多， >= 1<<B 也算
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// If the threshold is too low, we do extraneous work.
	// If the threshold is too high, maps that grow and shrink can hold on to lots of unused memory.
	// "too many" means (approximately) as many overflow buckets as regular buckets.
	// See incrnoverflow for more details.
	if B > 15 {
		B = 15
	}
	// The compiler doesn't see here that B < 16; mask B to generate shorter shift code.
	return noverflow >= uint16(1)<<(B&15)
}

// growing reports whether h is growing. The growth may be to the same size or bigger.
// growing 判断是否正在扩容
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// sameSizeGrow reports whether the current growth is to a map of the same size.
// sameSizeGrow 返回 map 是否相等大小扩容
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0
}

// noldbuckets calculates the number of buckets prior to the current map growth.
// noldbuckets 计算扩容之前的 buckets 数
func (h *hmap) noldbuckets() uintptr {
	oldB := h.B
	if !h.sameSizeGrow() {
		oldB--
	}
	return bucketShift(oldB)
}

// oldbucketmask provides a mask that can be applied to calculate n % noldbuckets().
// oldbucketmask 计算扩容之前掩码
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

// growWork 扩充工作
func growWork(t *maptype, h *hmap, bucket uintptr) {
	// make sure we evacuate the oldbucket corresponding
	// to the bucket we're about to use
	// 确保 bucket 对应的 oldbucket 已经迁移完了，因为要开始使用 bucket 了
	// 这里是要操作 bucket 了，先把这个先迁移
	evacuate(t, h, bucket&h.oldbucketmask())

	// evacuate one more oldbucket to make progress on growing
	// 如果还在扩容中，再顺带搬迁一个bucket
	if h.growing() {
		evacuate(t, h, h.nevacuate)
	}
}

// bucket 判断是否已经迁移
func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.bucketsize)))
	return evacuated(b)
}

// evacDst is an evacuation destination.
// evacDst 迁移目的地结构
type evacDst struct {
	b *bmap          // current destination bucket
	i int            // key/val index into b
	k unsafe.Pointer // pointer to current key storage
	v unsafe.Pointer // pointer to current value storage
}

// 迁移
func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
	newbit := h.noldbuckets()
	// evacuated 判断是否已经迁移过了
	if !evacuated(b) {
		// TODO: reuse overflow buckets instead of using new ones, if there
		// is no iterator using the old buckets.  (If !oldIterator.)

		// xy contains the x and y (low and high) evacuation destinations.
		// x 为迁移到之前相同的位置； y 为翻倍扩容的时候，扩容到高于原始索引的位置。
		var xy [2]evacDst
		x := &xy[0]
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.bucketsize))) // oldbucket 平移迁移，还是原来的位置
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		x.v = add(x.k, bucketCnt*uintptr(t.keysize))

		if !h.sameSizeGrow() {
			// Only calculate y pointers if we're growing bigger.
			// Otherwise GC can see bad pointers.
			// 仅当翻倍扩容的时候才计算 y ，否则 GC 可以看到坏指针。
			y := &xy[1]
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.bucketsize))) // oldbucket 向后迁移，需要加 newbit
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			y.v = add(y.k, bucketCnt*uintptr(t.keysize))
		}
		// 遍历哈希桶和对应的溢出桶（也是哈希桶）
		for ; b != nil; b = b.overflow(t) {
			k := add(unsafe.Pointer(b), dataOffset)
			v := add(k, bucketCnt*uintptr(t.keysize))
			// 遍历哈希桶 8 个位置
			for i := 0; i < bucketCnt; i, k, v = i+1, add(k, uintptr(t.keysize)), add(v, uintptr(t.valuesize)) {
				top := b.tophash[i]
				// 如果为空，则设置为 evacuatedEmpty
				if isEmpty(top) {
					b.tophash[i] = evacuatedEmpty
					continue
				}
				if top < minTopHash {
					throw("bad map state")
				}
				k2 := k
				if t.indirectkey() {
					// k 是指针类型
					k2 = *((*unsafe.Pointer)(k2))
				}
				var useY uint8
				if !h.sameSizeGrow() {
					// Compute hash to make our evacuation decision (whether we need
					// to send this key/value to bucket x or bucket y).
					// 翻倍扩容，计算哈希值
					hash := t.key.alg.hash(k2, uintptr(h.hash0))
					if h.flags&iterator != 0 && !t.reflexivekey() && !t.key.alg.equal(k2, k2) {
						// If key != key (NaNs), then the hash could be (and probably
						// will be) entirely different from the old hash. Moreover,
						// it isn't reproducible. Reproducibility is required in the
						// presence of iterators, as our evacuation decision must
						// match whatever decision the iterator made.
						// Fortunately, we have the freedom to send these keys either
						// way. Also, tophash is meaningless for these kinds of keys.
						// We let the low bit of tophash drive the evacuation decision.
						// We recompute a new random tophash for the next level so
						// these keys will get evenly distributed across all buckets
						// after multiple grows.
						// 如果 key != key  ，则哈希值与就哈希值完全不同。并且是不可复制的(!t.reflexivekey())。
						// 在迭代情况下，需要满足可以复制性，所以我们的迁移决策必须匹配迭代模式的。
						// 幸运的是，我们可以自由的迁移 key 。让 tophash 的低位来决策迁移。
						// 我们为下一个级别重新计算了一个随机的 tophash  ，因此在多次增长之后，这些密钥将在所有存储桶中平均分配。
						useY = top & 1 // 用低位来决策迁移
						top = tophash(hash)
					} else {
						// 比如 4 迁移到 8 的时候 ，newbit = 0100
						// hash&newbit == 0 则平移迁移 ，否则向后迁移(这时候需要使用 y)
						if hash&newbit != 0 { // 用高位来决策迁移
							useY = 1
						}
					}
				}

				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY {
					throw("bad evacuatedN")
				}
				// 存储 tophash 状态， dst 为迁移的目的地
				b.tophash[i] = evacuatedX + useY // evacuatedX + 1 == evacuatedY
				dst := &xy[useY]                 // evacuation destination

				// 当前的 dst 空间不够了，新生成一个溢出桶
				if dst.i == bucketCnt {
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					dst.v = add(dst.k, bucketCnt*uintptr(t.keysize))
				}
				// 设置 dst.b 的 tophash
				dst.b.tophash[dst.i&(bucketCnt-1)] = top // mask dst.i as an optimization, to avoid a bounds check
				if t.indirectkey() {
					// k 是指针类型，设置指针
					*(*unsafe.Pointer)(dst.k) = k2 // copy pointer
				} else {
					// k 不是指针类型，内存拷贝
					typedmemmove(t.key, dst.k, k) // copy value
				}
				if t.indirectvalue() {
					// v 是指针类型，设置指针
					*(*unsafe.Pointer)(dst.v) = *(*unsafe.Pointer)(v)
				} else {
					// v 不是指针类型，内存拷贝
					typedmemmove(t.elem, dst.v, v)
				}
				// 继续下一个
				dst.i++
				// These updates might push these pointers past the end of the
				// key or value arrays.  That's ok, as we have the overflow pointer
				// at the end of the bucket to protect against pointing past the
				// end of the bucket.
				dst.k = add(dst.k, uintptr(t.keysize))
				dst.v = add(dst.v, uintptr(t.valuesize))
			}
		}
		// Unlink the overflow buckets & clear key/value to help GC.
		// 接触溢出桶的链接，
		if h.flags&oldIterator == 0 && t.bucket.kind&kindNoPointers == 0 {
			b := add(h.oldbuckets, oldbucket*uintptr(t.bucketsize))
			// Preserve b.tophash because the evacuation
			// state is maintained there.
			// 保留 b.tophash ，因为迁移状态在这里维护
			ptr := add(b, dataOffset)
			n := uintptr(t.bucketsize) - dataOffset
			memclrHasPointers(ptr, n)
		}
	}

	// 如果迁移的是 nevacuate 的地方 ，则表示我们需要尝试把 nevacuate 往后移了，看看是否是已经迁移过了
	if oldbucket == h.nevacuate {
		advanceEvacuationMark(h, t, newbit)
	}
}

func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	// 对迁移进度 hmap.nevacuate 进行累积计数
	h.nevacuate++
	// Experiments suggest that 1024 is overkill by at least an order of magnitude.
	// Put it in there as a safeguard anyway, to ensure O(1) behavior.
	// 一次最多统计 1024 个，太多了影响性能
	stop := h.nevacuate + 1024
	if stop > newbit {
		stop = newbit
	}
	// bucketEvacuated 判断是否已经迁移了，如果迁移了，则 h.nevacuate++
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		h.nevacuate++
	}
	// 在所有的旧桶都被分流后清空哈希的 oldbuckets 和 oldoverflow 字段
	if h.nevacuate == newbit { // newbit == # of oldbuckets
		// Growing is all done. Free old main bucket array.
		h.oldbuckets = nil
		// Can discard old overflow buckets as well.
		// If they are still referenced by an iterator,
		// then the iterator holds a pointers to the slice.
		if h.extra != nil {
			h.extra.oldoverflow = nil
		}
		h.flags &^= sameSizeGrow
	}
}

func ismapkey(t *_type) bool {
	return t.alg.hash != nil
}

// Reflect stubs. Called from ../reflect/asm_*.s

//go:linkname reflect_makemap reflect.makemap
func reflect_makemap(t *maptype, cap int) *hmap {
	// Check invariants and reflects math.
	if !ismapkey(t.key) {
		throw("runtime.reflect_makemap: unsupported map key type")
	}
	if t.key.size > maxKeySize && (!t.indirectkey() || t.keysize != uint8(sys.PtrSize)) ||
		t.key.size <= maxKeySize && (t.indirectkey() || t.keysize != uint8(t.key.size)) {
		throw("key size wrong")
	}
	if t.elem.size > maxValueSize && (!t.indirectvalue() || t.valuesize != uint8(sys.PtrSize)) ||
		t.elem.size <= maxValueSize && (t.indirectvalue() || t.valuesize != uint8(t.elem.size)) {
		throw("value size wrong")
	}
	if t.key.align > bucketCnt {
		throw("key align too big")
	}
	if t.elem.align > bucketCnt {
		throw("value align too big")
	}
	if t.key.size%uintptr(t.key.align) != 0 {
		throw("key size not a multiple of key align")
	}
	if t.elem.size%uintptr(t.elem.align) != 0 {
		throw("value size not a multiple of value align")
	}
	if bucketCnt < 8 {
		throw("bucketsize too small for proper alignment")
	}
	if dataOffset%uintptr(t.key.align) != 0 {
		throw("need padding in bucket (key)")
	}
	if dataOffset%uintptr(t.elem.align) != 0 {
		throw("need padding in bucket (value)")
	}

	return makemap(t, cap, nil)
}

//go:linkname reflect_mapaccess reflect.mapaccess
func reflect_mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	val, ok := mapaccess2(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		val = nil
	}
	return val
}

//go:linkname reflect_mapassign reflect.mapassign
func reflect_mapassign(t *maptype, h *hmap, key unsafe.Pointer, val unsafe.Pointer) {
	p := mapassign(t, h, key)
	typedmemmove(t.elem, p, val)
}

//go:linkname reflect_mapdelete reflect.mapdelete
func reflect_mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	mapdelete(t, h, key)
}

//go:linkname reflect_mapiterinit reflect.mapiterinit
func reflect_mapiterinit(t *maptype, h *hmap) *hiter {
	it := new(hiter)
	mapiterinit(t, h, it)
	return it
}

//go:linkname reflect_mapiternext reflect.mapiternext
func reflect_mapiternext(it *hiter) {
	mapiternext(it)
}

//go:linkname reflect_mapiterkey reflect.mapiterkey
func reflect_mapiterkey(it *hiter) unsafe.Pointer {
	return it.key
}

//go:linkname reflect_mapitervalue reflect.mapitervalue
func reflect_mapitervalue(it *hiter) unsafe.Pointer {
	return it.value
}

//go:linkname reflect_maplen reflect.maplen
func reflect_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(reflect_maplen))
	}
	return h.count
}

//go:linkname reflect_ismapkey reflect.ismapkey
func reflect_ismapkey(t *_type) bool {
	return ismapkey(t)
}

// key 不存在的时候，并且 value 的内存大小 <= 1024 ，返回这个
const maxZero = 1024 // must match value in cmd/compile/internal/gc/walk.go
var zeroVal [maxZero]byte
