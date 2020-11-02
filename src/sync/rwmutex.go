// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// A RWMutex must not be copied after first use.
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
// RWMutex 读写锁
type RWMutex struct {
	w           Mutex  // held if there are pending writers 					// 写操作的锁
	writerSem   uint32 // semaphore for writers to wait for completing readers	// 写入操作的信号量
	readerSem   uint32 // semaphore for readers to wait for completing writers 	// 读操作的信号量
	readerCount int32  // number of pending readers								// 当前读操作的个数
	readerWait  int32  // number of departing readers 							// 当前写入操作需要等待读操作解锁的个数
}

const rwmutexMaxReaders = 1 << 30 // 最大读数量

// RLock locks rw for reading.
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.
// RLock 获取读锁
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// 小于 0 ，表示已加写锁
	if atomic.AddInt32(&rw.readerCount, 1) < 0 {
		// A writer is pending, wait for it.
		//
		runtime_SemacquireMutex(&rw.readerSem, false)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
// RUnlock 释放读锁
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	// 解锁，则 readerCount -= 1
	// 如果 r < 0 ，表示有 writer 正在想要获取写锁
	if r := atomic.AddInt32(&rw.readerCount, -1); r < 0 {
		// 判断是否没有获取读锁就想去释放读锁或者获取写锁释放读锁，直接抛异常
		if r+1 == 0 || r+1 == -rwmutexMaxReaders {
			race.Enable()
			throw("sync: RUnlock of unlocked RWMutex")
		}
		// A writer is pending.
		// Lock 的时候写入了 readerWait ，RUnlock 的时候减 1
		// 当 readerWait == 1 的时候，表示没有持有写锁的 goroutine 了
		if atomic.AddInt32(&rw.readerWait, -1) == 0 {
			// The last reader unblocks the writer.
			// 最后一个 reader 唤醒 writer
			runtime_Semrelease(&rw.writerSem, false)
		}
	}
	if race.Enabled {
		race.Enable()
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
// Lock 获取写锁
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// First, resolve competition with other writers.
	// 加速，防止其他的 writers 竞争
	rw.w.Lock()
	// Announce to readers there is a pending writer.
	// 向 readers 宣布有一个待定的 writer 。 通过 readerCount -= rwmutexMaxReaders
	// r 表示之前有多少个 readers
	r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders
	// Wait for active readers.
	// 如果之前有 readers ，则设置 readerWait = r
	if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 {
		// 如果有持有写锁的 goroutine ，则等待 RUnlock 中最后一个 reader 唤醒 writer
		runtime_SemacquireMutex(&rw.writerSem, false)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
// Unlock 释放写锁
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// Announce to readers there is no active writer.
	// 向 readers 宣布已经没有 writer 了。 通过 readerCount += rwmutexMaxReaders
	// r 表示之前有多少个 readers
	r := atomic.AddInt32(&rw.readerCount, rwmutexMaxReaders)
	if r >= rwmutexMaxReaders { // 还没有开始加写锁的时候，就开始释放写锁了
		race.Enable()
		throw("sync: Unlock of unlocked RWMutex")
	}
	// Unblock blocked readers, if any.
	// 唤醒所有的 readers
	for i := 0; i < int(r); i++ {
		runtime_Semrelease(&rw.readerSem, false)
	}
	// Allow other writers to proceed.
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
// RLocker 返回 rw.RLock 和 rw.RUnlock 实现的 Locker 接口。
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
