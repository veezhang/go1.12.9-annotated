// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
// Mutex 互斥锁
type Mutex struct {
	state int32  // 锁标识位， 0bit-锁标记 | 1bit-唤醒标记 | 2bit-饥饿标记 | 其他-waiter数
	sema  uint32 // 信号量，用于唤醒 goroutine
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked      = 1 << iota // mutex is locked // 锁标志位
	mutexWoken                   // 唤醒标志位
	mutexStarving                // 饥饿标志位
	mutexWaiterShift = iota      // 记录等待携程的数量，需要右移 mutexWaiterShift(3) 位

	// Mutex fairness.
	// 公平锁
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	// Mutex 可能处于两种不同的模式：正常模式和饥饿模式。
	// 在正常模式中，等待者按照 FIFO 的顺序排队获取锁，但是一个被唤醒的等待者有时候并不能获取 mutex，
	// 它还需要和新到来的 goroutine 们竞争 mutex 的使用权。新到来的 goroutine 存在一个优势，它们已经
	// 在 CPU 上运行且它们数量很多， 因此一个被唤醒的等待者有很大的概率获取不到锁，在这种情况下它处在
	// 等待队列的前面。 如果一个 goroutine 等待 mutex 释放的时间超过 1ms，它就会将 mutex 切换到饥饿模式。
	//
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	// 在饥饿模式中，mutex 的所有权直接从解锁的 goroutine 递交到等待队列中排在最前方的 goroutine。 新到达
	// 的 goroutine 们不要尝试去获取 mutex，即使它看起来是在解锁状态，也不要试图自旋， 而是排到等待队列的尾部。
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	// 如果一个等待者获得 mutex 的所有权，并且看到以下两种情况中的任一种：
	// 1.它是等待队列中的最后一个 2. 它等待的时间少于 1ms
	// 它便将 mutex 切换回正常操作模式
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	// 正常模式下的性能会更好，因为一个 goroutine 能在即使有很多阻塞的等待者时多次连续的
	// 获得一个 mutex，饥饿模式的重要性则在于避免了病态情况下的尾部延迟。
	starvationThresholdNs = 1e6 // 饥饿切换阀值 1e6 纳秒 ==> 1毫秒
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
// Lock 对申请锁的情况分为三种：
// 1. 无冲突，通过 CAS 操作把当前状态设置为加锁状态
// 2. 有冲突，开始自旋，并等待锁释放，如果其他 goroutine 在这段时间内释放该锁，直接获得该锁；如果没有释放则为下一种情况
// 3. 有冲突，且已经过了自旋阶段，通过调用 semrelease 让 goroutine 进入等待状态
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// CAS 修改状态，成功表示未锁住状态，否则当前已锁住
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}

	// 当前已经锁住，开始竞争 Mutex

	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 不要在饥饿模式下自旋，因为所有权将会移交到等待着的 goroutine ，此 goroutine 如论如何也无法获取互斥锁
		// mutexLocked 状态 && 非 mutexStarving 状态 && 可以自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			// 主动自旋是有意义的，因为会尝试唤醒锁，这样上个协程此时 unlock 的话，就不会唤醒其他协程
			// 设置 mutexWoken 标识位，条件：非 mutexWoken 状态 && 有阻塞的 goroutine
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 自旋一段时间
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}
		// 自旋结束 or 没有自旋
		// 下面计算需要获取 Mutex ， new 该怎么设置
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 不要试图获取饥饿的 Mutex ，新的 goroutine 必须排队。
		if old&mutexStarving == 0 { // 不是饥饿状态，需要加锁
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 { // mutexLocked 状态 Or mutexStarving 状态
			new += 1 << mutexWaiterShift // 增加等待的 goroutine +1
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 切换饥饿模式， 条件：超过 1ms (starving = true) && 处于 mutexLocked 状态
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		// 竞争失败后， mutexWoken 标识位要清除
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 既然当前协程被唤醒了，需要将 state 置为未唤醒
			new &^= mutexWoken
		}
		// 上面计算完了， 这里更新 state 状态
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// CAS 成功，表示加锁成功了
			// 如果之前不是 mutexLocked 或 mutexStarving 状态，则表示已经获取锁成功，不需要等待了
			// 否则表示还需要等待，需要将当前 goroutine 加入等待队列等操作
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 自旋过的 goroutine 使用 lifo 方式进入信号量等待队列
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			// 锁请求失败，进入休眠状态，等待信号唤醒后重新开始循环，一直阻塞在这里
			runtime_SemacquireMutex(&m.sema, queueLifo)
			// 这里此 goroutine 被唤醒了
			// 更新 starving ，如果超过 1ms 就进入饥饿模式
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 { // 是饥饿状态，被唤醒的则获取 Mutex，修改下状态
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 如果此 goroutine 被唤醒，并且处于饥饿模式，则所有权已移交给我们
				// 此时状态还没切换， old 应该不处于 mutexLocked(不可能别的 goroutine 获取了 Mutex) 或 mutexWoken 状态
				// 并且此 goroutine 应该也在等待者中， old>>mutexWaiterShift 不应该 == 0
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 调整 state
				// state += mutexLocked - 1<<mutexWaiterShift - mutexStarving?
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 只有当前 goroutine 在等待，并且还未超过 1ms ，则退出饥饿状态
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			// 更新状态失败，则继续
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
// 解锁， 与特定的 goroutine 没有关联，可以一个 goroutine 加锁，另一个解锁
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// 解锁，状态 state 直接减去 mutexLocked
	new := atomic.AddInt32(&m.state, -mutexLocked)
	// 重复unlock 直接 panic
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {
		// 不处于饥饿模式
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果没有等待着的 goroutine ，或者已经存在一个 goroutine 被唤醒或者得到锁，或处于饥饿模式
			// 无需唤醒任何等待状态的 goroutine
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 设置唤醒位
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 释放信号量， FIFO 方式释放goroutine
				// 唤醒一个阻塞的 goroutine，但不是唤醒第一个等待者
				runtime_Semrelease(&m.sema, false)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 饥饿模式: 直接将 mutex 所有权交给等待队列最前端的 goroutine
		runtime_Semrelease(&m.sema, true)
	}
}
