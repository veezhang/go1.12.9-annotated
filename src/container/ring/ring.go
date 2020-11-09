// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package ring implements operations on circular lists.
package ring

// A Ring is an element of a circular list, or ring.
// Rings do not have a beginning or end; a pointer to any ring element
// serves as reference to the entire ring. Empty rings are represented
// as nil Ring pointers. The zero value for a Ring is a one-element
// ring with a nil Value.
// Ring 是循环链表，或者叫环。环形链表没有头尾；指向环形链表任一元素的指针都可以作为整个环形链表看待。
// Ring 零值是具有一个（Value字段为nil的）元素的 ring 。
type Ring struct {
	next, prev *Ring       // 包含一个指向下一个和上一个 Ring 的指针
	Value      interface{} // for use by client; untouched by this library
}

// 初始化
func (r *Ring) init() *Ring {
	// 初始化指向自己，有一个元素 Value 为 nil
	r.next = r
	r.prev = r
	return r
}

// Next returns the next ring element. r must not be empty.
// Next 获取当前元素的下个元素
func (r *Ring) Next() *Ring {
	if r.next == nil {
		// 如果还未初始化，测初始化下
		return r.init()
	}
	return r.next
}

// Prev returns the previous ring element. r must not be empty.
// Next 获取当前元素的上个元素
func (r *Ring) Prev() *Ring {
	if r.next == nil {
		return r.init()
	}
	return r.prev
}

// Move moves n % r.Len() elements backward (n < 0) or forward (n >= 0)
// in the ring and returns that ring element. r must not be empty.
// Move 返回移动n个位置（n>=0向前移动，n<0向后移动）后的元素
func (r *Ring) Move(n int) *Ring {
	if r.next == nil {
		return r.init()
	}
	switch {
	case n < 0:
		for ; n < 0; n++ {
			r = r.prev
		}
	case n > 0:
		for ; n > 0; n-- {
			r = r.next
		}
	}
	return r
}

// New creates a ring of n elements.
// New 创建一个长度为n的环形链表
func New(n int) *Ring {
	if n <= 0 {
		return nil
	}
	r := new(Ring)
	p := r
	for i := 1; i < n; i++ {
		p.next = &Ring{prev: p}
		p = p.next
	}
	p.next = r
	r.prev = p
	return r
}

// Link connects ring r with ring s such that r.Next()
// becomes s and returns the original value for r.Next().
// r must not be empty.
//
// If r and s point to the same ring, linking
// them removes the elements between r and s from the ring.
// The removed elements form a subring and the result is a
// reference to that subring (if no elements were removed,
// the result is still the original value for r.Next(),
// and not nil).
//
// If r and s point to different rings, linking
// them creates a single ring with the elements of s inserted
// after r. The result points to the element following the
// last element of s after insertion.
// Link 连接 r 和 s ，并返回 r 原本的后继元素 r.Next()
func (r *Ring) Link(s *Ring) *Ring {
	n := r.Next()
	if s != nil {
		p := s.Prev()
		// Note: Cannot use multiple assignment because
		// evaluation order of LHS is not specified.
		r.next = s
		s.prev = r
		n.prev = p
		p.next = n
	}
	return n
}

// Unlink removes n % r.Len() elements from the ring r, starting
// at r.Next(). If n % r.Len() == 0, r remains unchanged.
// The result is the removed subring. r must not be empty.
// Unlink 删除链表中 n % r.Len() 个元素，从 r.Next() 开始删除。如果  n % r.Len() ，不修改r。返回删除的元素构成的 Ring。
func (r *Ring) Unlink(n int) *Ring {
	if n <= 0 {
		return nil
	}
	// 将当前 r 和后面第 n+1%r.Len() 个元素串起来，则这中间 n 个元素就不再 r 中了，也就是删除了
	return r.Link(r.Move(n + 1))
}

// Len computes the number of elements in ring r.
// It executes in time proportional to the number of elements.
// Len 求环长度，返回环中元素数量。
func (r *Ring) Len() int {
	n := 0
	if r != nil {
		n = 1
		for p := r.Next(); p != r; p = p.next {
			n++
		}
	}
	return n
}

// Do calls function f on each element of the ring, in forward order.
// The behavior of Do is undefined if f changes *r.
// Do 对链表中任意元素执行 f 操作，如果 f 改变了 r ，则该操作造成的后果是不可预期的。
func (r *Ring) Do(f func(interface{})) {
	if r != nil {
		f(r.Value)
		for p := r.Next(); p != r; p = p.next {
			f(p.Value)
		}
	}
}
