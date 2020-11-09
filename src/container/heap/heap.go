// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package heap provides heap operations for any type that implements
// heap.Interface. A heap is a tree with the property that each node is the
// minimum-valued node in its subtree.
//
// The minimum element in the tree is the root, at index 0.
//
// A heap is a common way to implement a priority queue. To build a priority
// queue, implement the Heap interface with the (negative) priority as the
// ordering for the Less method, so Push adds items while Pop removes the
// highest-priority item from the queue. The Examples include such an
// implementation; the file example_pq_test.go has the complete source.
//
package heap

import "sort"

// The Interface type describes the requirements
// for a type using the routines in this package.
// Any type that implements it may be used as a
// min-heap with the following invariants (established after
// Init has been called or if the data is empty or sorted):
//
//	!h.Less(j, i) for 0 <= i < h.Len() and 2*i+1 <= j <= 2*i+2 and j < h.Len()
//
// Note that Push and Pop in this interface are for package heap's
// implementation to call. To add and remove things from the heap,
// use heap.Push and heap.Pop.
// 最小堆需要实现的接口
type Interface interface {
	sort.Interface      // 内嵌 sort.Interface ，实现了： Len() int , Less(i, j int) bool , Swap(i, j int)
	Push(x interface{}) // add x as element Len()					// Push 接口实现为：在最后加入一个元素
	Pop() interface{}   // remove and return element Len() - 1.		// Pop 接口实现为：移除最后一个元素
}

// Init establishes the heap invariants required by the other routines in this package.
// Init is idempotent with respect to the heap invariants
// and may be called whenever the heap invariants may have been invalidated.
// The complexity is O(n) where n = h.Len().
// Init 初始化一个堆。一个堆在使用任何堆操作之前应先初始化。Init 函数对于堆的约束性是幂等的（多次执行无意义），
// 并可能在任何时候堆的约束性被破坏时被调用。本函数复杂度为 O(n) ，其中 n 等于 h.Len() 。
func Init(h Interface) {
	// heapify
	n := h.Len()
	for i := n/2 - 1; i >= 0; i-- {
		down(h, i, n)
	}
}

// Push pushes the element x onto the heap.
// The complexity is O(log n) where n = h.Len().
// Push 加入元素
func Push(h Interface, x interface{}) {
	// h.Push 加入元素在最后面
	h.Push(x)
	// 把最后一个元素上浮
	up(h, h.Len()-1)
}

// Pop removes and returns the minimum element (according to Less) from the heap.
// The complexity is O(log n) where n = h.Len().
// Pop is equivalent to Remove(h, 0).
// Pop 移除并返回堆中最小的元素。
func Pop(h Interface) interface{} {
	n := h.Len() - 1
	// 将第一个元素与最后一个元素替换
	h.Swap(0, n)
	// 然后再下沉第一个元素，这里 n 为 h.Len() - 1 了，最后一个元素没有参与
	down(h, 0, n)
	return h.Pop() // Pop 接口实现为：移除最后一个元素
}

// Remove removes and returns the element at index i from the heap.
// The complexity is O(log n) where n = h.Len().
// Remove 移除并返回堆中第 i 个元素。
func Remove(h Interface, i int) interface{} {
	n := h.Len() - 1
	// 最后一个元素的话，不需要处理
	if n != i {
		// 否则将其与最后一个元素替换
		h.Swap(i, n)
		// 然后再下沉第 i 个元素，这里 n 为 h.Len() - 1 了，最后一个元素没有参与
		if !down(h, i, n) {
			// 如果没有下沉，则上浮
			up(h, i)
		}
	}
	return h.Pop()
}

// Fix re-establishes the heap ordering after the element at index i has changed its value.
// Changing the value of the element at index i and then calling Fix is equivalent to,
// but less expensive than, calling Remove(h, i) followed by a Push of the new value.
// The complexity is O(log n) where n = h.Len().
// Fix 在修改第i个元素后，调用本函数修复堆，比删除第 i 个元素后插入新元素更有效率。
func Fix(h Interface, i int) {
	if !down(h, i, h.Len()) {
		up(h, i)
	}
}

// up 上浮 j 节点，以满足最小堆，时间复杂度 O(log(n))
func up(h Interface, j int) {
	for {
		// 获取父节点
		i := (j - 1) / 2 // parent
		// 直到根节点，或者 j 大于等与父节点(i) ，此时表示有序了
		if i == j || !h.Less(j, i) {
			break
		}
		// 否则，j < 父节点，子节点和父节点替换
		// 这里子孩子 j < 父节点，则一定另一个子节点 >= 父节点 > j
		h.Swap(i, j)
		// 再继续遍历替换后的父节点
		j = i
	}
}

// down 下沉 i0 节点，n 为堆长度，以满足最小堆，时间复杂度 O(log(n))
// 返回是否下沉了
func down(h Interface, i0, n int) bool {
	i := i0
	for {
		// 左孩子
		j1 := 2*i + 1
		// 左孩子已经越界了，表示调整完了，结束
		if j1 >= n || j1 < 0 { // j1 < 0 after int overflow
			break
		}
		// 左孩子
		j := j1 // left child
		// j2 为右孩子，如果右孩子 < 左孩子，则取右孩子
		if j2 := j1 + 1; j2 < n && h.Less(j2, j1) {
			j = j2 // = 2*i + 2  // right child
		}
		// 如果左右孩子中较小的 <= 父节点，则满足
		if !h.Less(j, i) {
			break
		}
		// 否则，左右孩子中较小的 > 父节点，把较小的那个子孩子上浮
		h.Swap(i, j)
		i = j
	}
	return i > i0
}
