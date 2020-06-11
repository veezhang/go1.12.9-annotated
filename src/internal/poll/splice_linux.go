// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package poll

import (
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	// spliceNonblock makes calls to splice(2) non-blocking.
	// spliceNonblock 非阻塞调用系统调用 splice
	spliceNonblock = 0x2

	// maxSpliceSize is the maximum amount of data Splice asks
	// the kernel to move in a single call to splice(2).
	// maxSpliceSize 是 Splice 要求内核在一次对 splice(2) 的调用中移动的最大数据量。
	maxSpliceSize = 4 << 20 // 4M
)

// Splice transfers at most remain bytes of data from src to dst, using the
// splice system call to minimize copies of data from and to userspace.
//
// Splice creates a temporary pipe, to serve as a buffer for the data transfer.
// src and dst must both be stream-oriented sockets.
//
// If err != nil, sc is the system call which caused the error.
//
// Splice 尽可能多的从 src 到 dst 拷贝数据，使用 splice 系统调用减少来回用户空间的数据拷贝。
// Splice 创建一个临时管道，用作数据传输的缓冲区。 src 和 dst 都必须是面向流的套接字
// 如果 err！= nil，则 sc 是导致错误的系统调用。
func Splice(dst, src *FD, remain int64) (written int64, handled bool, sc string, err error) {
	prfd, pwfd, sc, err := newTempPipe()
	// 创建临时管道失败
	if err != nil {
		return 0, false, sc, err
	}
	defer destroyTempPipe(prfd, pwfd)
	var inPipe, n int
	// 开始转换数据 src -> dst
	for err == nil && remain > 0 {
		max := maxSpliceSize
		if int64(max) > remain {
			max = int(remain)
		}
		// src -> pwfd
		inPipe, err = spliceDrain(pwfd, src, max)
		// The operation is considered handled if splice returns no
		// error, or an error other than EINVAL. An EINVAL means the
		// kernel does not support splice for the socket type of src.
		// The failed syscall does not consume any data so it is safe
		// to fall back to a generic copy.
		//
		// spliceDrain should never return EAGAIN, so if err != nil,
		// Splice cannot continue.
		//
		// If inPipe == 0 && err == nil, src is at EOF, and the
		// transfer is complete.
		handled = handled || (err != syscall.EINVAL)
		if err != nil || (inPipe == 0 && err == nil) {
			break
		}
		// prfd -> dst
		n, err = splicePump(dst, prfd, inPipe)
		if n > 0 {
			written += int64(n)
			remain -= int64(n)
		}
	}
	if err != nil {
		return written, handled, "splice", err
	}
	return written, true, "", nil
}

// spliceDrain moves data from a socket to a pipe.
//
// Invariant: when entering spliceDrain, the pipe is empty. It is either in its
// initial state, or splicePump has emptied it previously.
//
// Given this, spliceDrain can reasonably assume that the pipe is ready for
// writing, so if splice returns EAGAIN, it must be because the socket is not
// ready for reading.
//
// If spliceDrain returns (0, nil), src is at EOF.
// spliceDrain 将数据从 socket 移动到 pipe 。
func spliceDrain(pipefd int, sock *FD, max int) (int, error) {
	if err := sock.readLock(); err != nil {
		return 0, err
	}
	defer sock.readUnlock()
	if err := sock.pd.prepareRead(sock.isFile); err != nil {
		return 0, err
	}
	for {
		n, err := splice(pipefd, sock.Sysfd, max, spliceNonblock)
		if err != syscall.EAGAIN {
			return n, err
		}
		if err := sock.pd.waitRead(sock.isFile); err != nil {
			return n, err
		}
	}
}

// splicePump moves all the buffered data from a pipe to a socket.
//
// Invariant: when entering splicePump, there are exactly inPipe
// bytes of data in the pipe, from a previous call to spliceDrain.
//
// By analogy to the condition from spliceDrain, splicePump
// only needs to poll the socket for readiness, if splice returns
// EAGAIN.
//
// If splicePump cannot move all the data in a single call to
// splice(2), it loops over the buffered data until it has written
// all of it to the socket. This behavior is similar to the Write
// step of an io.Copy in userspace.
// splicePump 将所有缓冲的数据从 pipe 移到 socket 。
func splicePump(sock *FD, pipefd int, inPipe int) (int, error) {
	if err := sock.writeLock(); err != nil {
		return 0, err
	}
	defer sock.writeUnlock()
	if err := sock.pd.prepareWrite(sock.isFile); err != nil {
		return 0, err
	}
	written := 0
	for inPipe > 0 {
		n, err := splice(sock.Sysfd, pipefd, inPipe, spliceNonblock)
		// Here, the condition n == 0 && err == nil should never be
		// observed, since Splice controls the write side of the pipe.
		if n > 0 {
			inPipe -= n
			written += n
			continue
		}
		if err != syscall.EAGAIN {
			return written, err
		}
		if err := sock.pd.waitWrite(sock.isFile); err != nil {
			return written, err
		}
	}
	return written, nil
}

// splice wraps the splice system call. Since the current implementation
// only uses splice on sockets and pipes, the offset arguments are unused.
// splice returns int instead of int64, because callers never ask it to
// move more data in a single call than can fit in an int32.
// splice 包装了 splice 系统调用。 由于当前实现仅在套接字和管道上使用拼接，因此未使用offset参数。
// splice 返回 int 而不是 int64 ，因为调用者从不要求它在单个调用中移动超出int32容量的数据。
func splice(out int, in int, max int, flags int) (int, error) {
	n, err := syscall.Splice(in, nil, out, nil, max, flags)
	return int(n), err
}

// 是否禁用 splice
var disableSplice unsafe.Pointer

// newTempPipe sets up a temporary pipe for a splice operation.
// newTempPipe 为 splice 操作设置一个临时管道
func newTempPipe() (prfd, pwfd int, sc string, err error) {
	p := (*bool)(atomic.LoadPointer(&disableSplice))
	// 如果禁用了
	if p != nil && *p {
		return -1, -1, "splice", syscall.EINVAL
	}

	var fds [2]int
	// pipe2 was added in 2.6.27 and our minimum requirement is 2.6.23, so it
	// might not be implemented. Falling back to pipe is possible, but prior to
	// 2.6.29 splice returns -EAGAIN instead of 0 when the connection is
	// closed.
	// 在 2.6.27 中添加了 pipe2 ，我们的最低要求是 2.6.23 ，因此可能无法实现。 可以回退到管道，但是在 2.6.29 之前，关闭连接时，接头返回 -EAGAIN 而不是 0 。
	// 创建 pipe , 设置 O_CLOEXEC | O_NONBLOCK
	const flags = syscall.O_CLOEXEC | syscall.O_NONBLOCK
	if err := syscall.Pipe2(fds[:], flags); err != nil {
		return -1, -1, "pipe2", err
	}

	if p == nil {
		p = new(bool)
		defer atomic.StorePointer(&disableSplice, unsafe.Pointer(p))

		// F_GETPIPE_SZ was added in 2.6.35, which does not have the -EAGAIN bug.
		// F_GETPIPE_SZ 在 2.6.35 中添加，没有 -EAGAIN bug 。
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fds[0]), syscall.F_GETPIPE_SZ, 0); errno != 0 {
			*p = true
			destroyTempPipe(fds[0], fds[1])
			return -1, -1, "fcntl", errno
		}
	}

	return fds[0], fds[1], "", nil
}

// destroyTempPipe destroys a temporary pipe.
// destroyTempPipe 销毁临时管道
func destroyTempPipe(prfd, pwfd int) error {
	err := CloseFunc(prfd)
	err1 := CloseFunc(pwfd)
	if err == nil {
		return err1
	}
	return err
}
