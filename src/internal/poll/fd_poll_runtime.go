// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix darwin dragonfly freebsd linux netbsd openbsd windows solaris

package poll

import (
	"errors"
	"sync"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

// runtimeNano returns the current value of the runtime clock in nanoseconds.
//go:linkname runtimeNano runtime.nanotime
func runtimeNano() int64

// 实现在 src/runtime/netpoll.go 中，使用 //go:linkname 链接的
func runtime_pollServerInit()
func runtime_pollOpen(fd uintptr) (uintptr, int)
func runtime_pollClose(ctx uintptr)
func runtime_pollWait(ctx uintptr, mode int) int
func runtime_pollWaitCanceled(ctx uintptr, mode int) int
func runtime_pollReset(ctx uintptr, mode int) int
func runtime_pollSetDeadline(ctx uintptr, d int64, mode int)
func runtime_pollUnblock(ctx uintptr)
func runtime_isPollServerDescriptor(fd uintptr) bool

type pollDesc struct {
	runtimeCtx uintptr
}

var serverInit sync.Once

// init 初始化 pollDesc ， 这里会初始化 netpoll（只执行一次） ，然后加入到 netpoll
func (pd *pollDesc) init(fd *FD) error {
	// 初始化 netpoll ，linux 下是初始化 epoll 相关的， epollcreate1 or epollcreate
	serverInit.Do(runtime_pollServerInit)
	// 加入到 netpoll 中， linux 是加入到 epoll 中，ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET, epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
	ctx, errno := runtime_pollOpen(uintptr(fd.Sysfd))
	if errno != 0 {
		if ctx != 0 {
			// 非阻塞轮询，取消阻塞在 pd 的 goroutine ，并加入到运行队列
			runtime_pollUnblock(ctx)
			// 从 netpoll 中移除， linux 是从 epoll 中移除，epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
			runtime_pollClose(ctx)
		}
		return syscall.Errno(errno)
	}
	pd.runtimeCtx = ctx
	return nil
}

// 关闭
func (pd *pollDesc) close() {
	if pd.runtimeCtx == 0 {
		return
	}
	// 从 netpoll 中移除，linux 是从 epoll 中移除，epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
	runtime_pollClose(pd.runtimeCtx)
	pd.runtimeCtx = 0
}

// 调用 runtime_pollUnblock 函数取消阻塞在 pd 中的读写 goroutine 。
// Evict evicts fd from the pending list, unblocking any I/O running on fd.
func (pd *pollDesc) evict() {
	if pd.runtimeCtx == 0 {
		return
	}
	// 非阻塞轮询，取消阻塞在 pd 的 goroutine 并加入到运行队列
	runtime_pollUnblock(pd.runtimeCtx)
}

// 准备读/写
func (pd *pollDesc) prepare(mode int, isFile bool) error {
	if pd.runtimeCtx == 0 {
		return nil
	}
	// 重置 pd
	res := runtime_pollReset(pd.runtimeCtx, mode)
	return convertErr(res, isFile)
}

// 准备读
func (pd *pollDesc) prepareRead(isFile bool) error {
	return pd.prepare('r', isFile)
}

// 准备写
func (pd *pollDesc) prepareWrite(isFile bool) error {
	return pd.prepare('w', isFile)
}

// 等待读/写
func (pd *pollDesc) wait(mode int, isFile bool) error {
	if pd.runtimeCtx == 0 {
		return errors.New("waiting for unsupported file type")
	}
	res := runtime_pollWait(pd.runtimeCtx, mode)
	return convertErr(res, isFile)
}

// 等待读
func (pd *pollDesc) waitRead(isFile bool) error {
	return pd.wait('r', isFile)
}

// 等待写
func (pd *pollDesc) waitWrite(isFile bool) error {
	return pd.wait('w', isFile)
}

// wait 相同，但是忽略 超时/关闭 错误
func (pd *pollDesc) waitCanceled(mode int) {
	if pd.runtimeCtx == 0 {
		return
	}
	runtime_pollWaitCanceled(pd.runtimeCtx, mode)
}

// 是否可以轮询
func (pd *pollDesc) pollable() bool {
	return pd.runtimeCtx != 0
}

// 转换错误
func convertErr(res int, isFile bool) error {
	// 0 成功，1 关闭，2 超时
	switch res {
	case 0:
		return nil
	case 1:
		return errClosing(isFile)
	case 2:
		return ErrTimeout
	}
	println("unreachable: ", res)
	panic("unreachable")
}

// SetDeadline sets the read and write deadlines associated with fd.
// 设置读写截止时间
func (fd *FD) SetDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'r'+'w')
}

// SetReadDeadline sets the read deadline associated with fd.
// 设置读截止时间
func (fd *FD) SetReadDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'r')
}

// SetWriteDeadline sets the write deadline associated with fd.
// 设置写截止时间
func (fd *FD) SetWriteDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'w')
}

// 设置读写截止时间的具体实现，最终调用 runtime_pollSetDeadline
func setDeadlineImpl(fd *FD, t time.Time, mode int) error {
	var d int64
	if !t.IsZero() {
		d = int64(time.Until(t))
		// d < 0 表示已经到截至时间了
		if d == 0 {
			// 不要混淆没有 deadline 和 deadline 就是现在， 没有 deadline 的时候 d = 0
			d = -1 // don't confuse deadline right now with no deadline
		}
	}
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	if fd.pd.runtimeCtx == 0 {
		return ErrNoDeadline
	}
	runtime_pollSetDeadline(fd.pd.runtimeCtx, d, mode)
	return nil
}

// IsPollDescriptor reports whether fd is the descriptor being used by the poller.
// This is only used for testing.
// IsPollDescriptor 判读 fd 是否是轮询器正在使用的文件描述符。这仅用于测试。
func IsPollDescriptor(fd uintptr) bool {
	return runtime_isPollServerDescriptor(fd)
}
