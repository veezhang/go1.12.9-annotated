// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// A WaitGroup waits for a collection of goroutines to finish.
// The main goroutine calls Add to set the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls Done when finished. At the same time,
// Wait can be used to block until all goroutines have finished.
//
// A WaitGroup must not be copied after first use.
// WaitGroup 用于等待一组 goroutines 执行完。 主 Goroutine 调用 Add 来设置需要等待的 Goroutine 的数量，
// 然后每个 Goroutine 运行并调用 Done 来确认已经执行网完毕，同时，Wait 可以用于阻塞并等待所有 Goroutine 完。
type WaitGroup struct {
	noCopy noCopy

	// 64-bit value: high 32 bits are counter, low 32 bits are waiter count.
	// 64-bit atomic operations require 64-bit alignment, but 32-bit
	// compilers do not ensure it. So we allocate 12 bytes and then use
	// the aligned 8 bytes in them as state, and the other 4 as storage
	// for the sema.
	// 64 位值: 高 32 位用于计数，低 32 位用于等待计数(等待的 goroutines 数)
	// 64 位的原子操作要求 64 位对齐，但 32 位编译器无法保证这个要求
	// 因此分配 12 字节，然后用其中对齐的 8 字节作为状态，其他 4 字节用于存储 sema
	state1 [3]uint32
}

// state returns pointers to the state and sema fields stored within wg.state1.
// state 返回 wg.state1 中存储的 state 和 sema 字段
func (wg *WaitGroup) state() (statep *uint64, semap *uint32) {
	if uintptr(unsafe.Pointer(&wg.state1))%8 == 0 { // 8 字节对齐
		// 如果 8 字节对齐，则使用 state1[0,1] 作为 state ， state1[2] 作为 sema
		return (*uint64)(unsafe.Pointer(&wg.state1)), &wg.state1[2]
	} else { // 8 字节没对齐
		// 如果 8 字节没对齐，则使用 state1[1,2] 作为 state ， state1[0] 作为 sema
		// 此时 state1[1] 一定是 8 字节对齐的
		return (*uint64)(unsafe.Pointer(&wg.state1[1])), &wg.state1[0]
	}
}

// Add adds delta, which may be negative, to the WaitGroup counter.
// If the counter becomes zero, all goroutines blocked on Wait are released.
// If the counter goes negative, Add panics.
//
// Note that calls with a positive delta that occur when the counter is zero
// must happen before a Wait. Calls with a negative delta, or calls with a
// positive delta that start when the counter is greater than zero, may happen
// at any time.
// Typically this means the calls to Add should execute before the statement
// creating the goroutine or other event to be waited for.
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned.
// See the WaitGroup example.
// Add 将 delta（可能为负）加到 WaitGroup 的计数器上，如果计数器归零，则所有阻塞在 Wait 的 Goroutine 被释放，
// 如果计数器为负，则 panic 。
func (wg *WaitGroup) Add(delta int) {
	// 首先获取 state 指针和 sema 指针
	statep, semap := wg.state()
	if race.Enabled {
		_ = *statep // trigger nil deref early
		if delta < 0 {
			// Synchronize decrements with Wait.
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}
	// 将 delta 加到 statep 的前 32 位上，即加到计数器上
	state := atomic.AddUint64(statep, uint64(delta)<<32)
	// 计数器的值
	v := int32(state >> 32)
	// 等待的 goroutines 数
	w := uint32(state)
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		race.Read(unsafe.Pointer(semap))
	}
	// 如果实际计数为负则直接 panic，因此是不允许计数为负值的
	if v < 0 {
		panic("sync: negative WaitGroup counter")
	}
	// 如果等待的 goroutines 不为零，但 delta 是处于增加的状态，而且存储计数与 delta 的值相同，则立即 panic
	// 表示有等待的 goroutines ，并且在 Add 之前为 0
	if w != 0 && delta > 0 && v == int32(delta) {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// 正常情况，Add会让v增加，Done会让v减少，如果没有全部Done掉，此处v总是会大于0的，直到v为0才往下走
	// 而w代表是有多少个goruntine在等待done的信号，wait中通过compareAndSwap对这个w进行加1
	if v > 0 || w == 0 {
		return
	}
	// v == 0 && w > 0

	// This goroutine has set counter to 0 when waiters > 0.
	// Now there can't be concurrent mutations of state:
	// - Adds must not happen concurrently with Wait,
	// - Wait does not increment waiters if it sees counter == 0.
	// Still do a cheap sanity check to detect WaitGroup misuse.
	// 当v为0(Done掉了所有)或者w不为0(已经开始等待)才会到这里，但是在这个过程中又有一次Add，导致statep变化，panic
	if *statep != state {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// Reset waiters count to 0.
	// 将信号量发出，触发wait结束
	*statep = 0
	for ; w != 0; w-- {
		// 释放
		runtime_Semrelease(semap, false)
	}
}

// Done decrements the WaitGroup counter by one.
// Done 减少 WaitGroup 计数，直接使用 wg.Add(-1)
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait blocks until the WaitGroup counter is zero.
// Wait 会保持阻塞直到 WaitGroup 计数器归零
func (wg *WaitGroup) Wait() {
	// 首先获取 state 指针和 sema 指针
	statep, semap := wg.state()
	if race.Enabled {
		_ = *statep // trigger nil deref early
		race.Disable()
	}
	// 只有当计数器归零才会结束
	for {
		// 原子读 state
		state := atomic.LoadUint64(statep)
		// 计数器的值
		v := int32(state >> 32)
		// 等待的 goroutines 数
		w := uint32(state)
		// 如果计数器已经归零，则直接退出循环
		if v == 0 {
			// Counter is 0, no need to wait.
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
		// Increment waiters count.
		// 增加等待计数，此处的原语会比较 statep 和 state 的值，如果相同则等待计数加 1
		// 加失败了在 for 循环中继续
		if atomic.CompareAndSwapUint64(statep, state, state+1) {
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				race.Write(unsafe.Pointer(semap))
			}
			// 会阻塞到存储原语是否 > 0（即睡眠），如果 *semap > 0 则会减 1，因此最终的 semap 理论为 0
			runtime_Semacquire(semap)
			// 在这种情况下，如果 *semap 不等于 0 ，则说明使用失误，直接 panic
			if *statep != 0 {
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
	}
}
