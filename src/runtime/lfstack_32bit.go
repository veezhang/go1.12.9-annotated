// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build 386 arm nacl mips mipsle

package runtime

import "unsafe"

// On 32-bit systems, the stored uint64 has a 32-bit pointer and 32-bit count.
// 在 32 位系统上，存储 uint64(lfnode) 中有 32 位的指针， 32 位的计数

// lfstack 打包
func lfstackPack(node *lfnode, cnt uintptr) uint64 {
	// 高 32 位为 node ，地 32 位为 cnt
	return uint64(uintptr(unsafe.Pointer(node)))<<32 | uint64(cnt)
}

// lfstack 解包
func lfstackUnpack(val uint64) *lfnode {
	return (*lfnode)(unsafe.Pointer(uintptr(val >> 32)))
}
