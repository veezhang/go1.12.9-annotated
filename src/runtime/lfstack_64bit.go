// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build amd64 arm64 mips64 mips64le ppc64 ppc64le s390x wasm

package runtime

import "unsafe"

const (
	// addrBits is the number of bits needed to represent a virtual address.
	// addrBits 是表示虚拟地址所需的位数
	//
	// See heapAddrBits for a table of address space sizes on
	// various architectures. 48 bits is enough for all
	// architectures except s390x.
	// 在 heapAddrBits(src/runtime/malloc.go) 中有各种体系结构上的地址空间大小的表， 除了 s390x ，其他的 48 位都够了。
	//
	// On AMD64, virtual addresses are 48-bit (or 57-bit) numbers sign extended to 64.
	// We shift the address left 16 to eliminate the sign extended part and make
	// room in the bottom for the count.
	// 在 AMD64 上，虚拟地址是 48 位（或 57 位），左移动 16 位预留出来进行计数。
	//
	// On s390x, virtual addresses are 64-bit. There's not much we
	// can do about this, so we just hope that the kernel doesn't
	// get to really high addresses and panic if it does.
	// 在 s390x 上，虚拟地址是 64 位，也无能为力，只能寄希望内核不会用高地址，如果真的出现则 panic 。
	addrBits = 48

	// In addition to the 16 bits taken from the top, we can take 3 from the
	// bottom, because node must be pointer-aligned, giving a total of 19 bits
	// of count.
	// 除了从顶部开始的16位，我们还可以从底部开始的3位，因为节点必须是指针对齐的，因此总共有19位计数。
	cntBits = 64 - addrBits + 3

	// On AIX, 64-bit addresses are split into 36-bit segment number and 28-bit
	// offset in segment.  Segment numbers in the range 0x0A0000000-0x0AFFFFFFF(LSA)
	// are available for mmap.
	// We assume all lfnode addresses are from memory allocated with mmap.
	// We use one bit to distinguish between the two ranges.
	// 在 AIX 上，将 64 位地址分为 36 位段号和 28 位段偏移量。 mmap 可以使用 0x0A0000000-0x0AFFFFFFF(LSA) 范围内的段号。
	// 我们假设所有 lfnode 地址都来自通过 mmap 分配的内存。 我们用 1 位来区分两个范围。
	aixAddrBits = 57
	aixCntBits  = 64 - aixAddrBits + 3
)

// lfstack 打包
func lfstackPack(node *lfnode, cnt uintptr) uint64 {
	if GOARCH == "ppc64" && GOOS == "aix" {
		return uint64(uintptr(unsafe.Pointer(node)))<<(64-aixAddrBits) | uint64(cnt&(1<<aixCntBits-1))
	}
	// node 左移动 (64-addrBits) ，也就是高 addrBits 位存地址， cnt 存在后面，这里多 3 位不会影响 node 的指针，因为字节对齐
	return uint64(uintptr(unsafe.Pointer(node)))<<(64-addrBits) | uint64(cnt&(1<<cntBits-1))
}

// lfstack 解包
func lfstackUnpack(val uint64) *lfnode {
	if GOARCH == "amd64" {
		// amd64 systems can place the stack above the VA hole, so we need to sign extend
		// val before unpacking.
		return (*lfnode)(unsafe.Pointer(uintptr(int64(val) >> cntBits << 3)))
	}
	if GOARCH == "ppc64" && GOOS == "aix" {
		return (*lfnode)(unsafe.Pointer(uintptr((val >> aixCntBits << 3) | 0xa<<56)))
	}
	// 右移动 cntBits 后再左移动 3 ，清除 cnt 占用字节对齐的那 3 位时设的值
	return (*lfnode)(unsafe.Pointer(uintptr(val >> cntBits << 3)))
}
