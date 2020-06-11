// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements sysSocket and accept for platforms that
// provide a fast path for setting SetNonblock and CloseOnExec.

// +build dragonfly freebsd linux netbsd openbsd

package net

import (
	"internal/poll"
	"os"
	"syscall"
)

// Wrapper around the socket system call that marks the returned file
// descriptor as nonblocking and close-on-exec.
// sysSocket 包装 socket 系统调用，将返回的文件描述符标记为 nonblocking close-on-exec 。
func sysSocket(family, sotype, proto int) (int, error) {
	// socketFunc = syscall.Socket
	// 最终调用系统调用： int socket(int domain, int type, int protocol);
	s, err := socketFunc(family, sotype|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, proto)
	// On Linux the SOCK_NONBLOCK and SOCK_CLOEXEC flags were
	// introduced in 2.6.27 kernel and on FreeBSD both flags were
	// introduced in 10 kernel. If we get an EINVAL error on Linux
	// or EPROTONOSUPPORT error on FreeBSD, fall back to using
	// socket without them.
	// Linux 在 2.6.27 内核中引入了 SOCK_NONBLOCK 和 SOCK_CLOEXEC 标志，在FreeBSD上，这两种标志都在10内核中引入了。
	// 如果我们在 Linux 上收到 EINVAL 错误或在 FreeBSD 上收到 EPROTONOSUPPORT 错误，请退回到使用没有它们的套接字。
	switch err {
	case nil:
		return s, nil
	default:
		return -1, os.NewSyscallError("socket", err)
	case syscall.EPROTONOSUPPORT, syscall.EINVAL:
	}

	// 这里就是上面提到的在低版本的系统上，我们分开来设置 SOCK_NONBLOCK 和 SOCK_CLOEXEC
	// 由于这里 SYS_SOCKET 调用不支持 O_CLOEXEC ，是分开执行的，不再是原子操作，在设置 SOCK_CLOEXEC 的时候对 syscall.ForkLock 加锁
	// 避免子继承不必要的文件描述符
	// See ../syscall/exec_unix.go for description of ForkLock.
	syscall.ForkLock.RLock()
	s, err = socketFunc(family, sotype, proto)
	if err == nil {
		syscall.CloseOnExec(s)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, os.NewSyscallError("socket", err)
	}
	if err = syscall.SetNonblock(s, true); err != nil {
		poll.CloseFunc(s)
		return -1, os.NewSyscallError("setnonblock", err)
	}
	return s, nil
}
