// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Lock-free stack.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// lfstack is the head of a lock-free stack.
//
// The zero value of lfstack is an empty list.
//
// This stack is intrusive. Nodes must embed lfnode as the first field.
//
// The stack does not keep GC-visible pointers to nodes, so the caller
// is responsible for ensuring the nodes are not garbage collected
// (typically by allocating them from manually-managed memory).
//
// lfstack是 lock-free stack 头。零值为空列表。是侵入式的。节点必须将 lfnode 嵌入为第一个字段。
// 不保留指向节点的 GC 可见指针，因此调用方负责确保不对节点进行垃圾回收（通常是通过从手动管理的内存中分配节点）。
type lfstack uint64

// push 头插法
func (head *lfstack) push(node *lfnode) {
	node.pushcnt++                                  // 累计
	new := lfstackPack(node, node.pushcnt)          // 打包
	if node1 := lfstackUnpack(new); node1 != node { // 解包测试下（有些情况这里可能真的会发生）
		print("runtime: lfstack.push invalid packing: node=", node, " cnt=", hex(node.pushcnt), " packed=", hex(new), " -> node=", node1, "\n")
		throw("lfstack.push")
	}
	for { // 循环直到成功
		// 先获取当前的头节点，然后插入到新的后面，再 cas 替换下
		old := atomic.Load64((*uint64)(head))
		node.next = old
		if atomic.Cas64((*uint64)(head), old, new) {
			break
		}
	}
}

// pop 从头部弹出
func (head *lfstack) pop() unsafe.Pointer {
	for { // 循环直到成功
		// 先获取当前的头节点
		old := atomic.Load64((*uint64)(head))
		if old == 0 { // 表示是空节点
			return nil
		}
		node := lfstackUnpack(old) // 解包
		// 获取当前头节点的下一个
		next := atomic.Load64(&node.next)
		// cas 将下一个设置为当前头节点
		if atomic.Cas64((*uint64)(head), old, next) {
			return unsafe.Pointer(node)
		}
	}
}

// 判断是否为空
func (head *lfstack) empty() bool {
	return atomic.Load64((*uint64)(head)) == 0
}

// lfnodeValidate panics if node is not a valid address for use with
// lfstack.push. This only needs to be called when node is allocated.
// lfnodeValidate 验证节点的有效性
func lfnodeValidate(node *lfnode) {
	if lfstackUnpack(lfstackPack(node, ^uintptr(0))) != node {
		printlock()
		println("runtime: bad lfnode address", hex(uintptr(unsafe.Pointer(node))))
		throw("bad lfnode address")
	}
}
