// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package runtime

import "unsafe"

// epoll 实现 netpoll
// 一些系统调用函数： epollcreate ， epollcreate1 ， epollctl ， epollwait

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32

//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
func closeonexec(fd int32) // 调用 fcntl(fd, F_SETFD, FD_CLOEXEC) ，实现在 runtime/sys_linux_amd64.s 中

var (
	// 全局的 epoll fd
	epfd int32 = -1 // epoll descriptor
)

// netpollinit 初始化 epoll
func netpollinit() {
	epfd = epollcreate1(_EPOLL_CLOEXEC)
	if epfd >= 0 {
		return
	}
	epfd = epollcreate(1024)
	if epfd >= 0 {
		closeonexec(epfd)
		return
	}
	println("runtime: epollcreate failed with", -epfd)
	throw("runtime: netpollinit failed")
}

// netpolldescriptor 返回 epoll 使用的 fd
func netpolldescriptor() uintptr {
	return uintptr(epfd)
}

// netpollopen 添加 fd 到 epoll 中
func netpollopen(fd uintptr, pd *pollDesc) int32 {
	var ev epollevent
	// 边缘触发模式，关注事件为： _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP
	ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET
	// 设置 data ， epoll_wait 可以拿到此数据
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
	// 调用 epollctl ，指定为 _EPOLL_CTL_ADD ，表示添加到 epoll
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
}

// netpollclose 从 epoll 中移除
func netpollclose(fd uintptr) int32 {
	var ev epollevent
	// 调用 epollctl ，指定为 _EPOLL_CTL_DEL ，表示从 epoll 中移除
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// polls for ready network connections
// returns list of goroutines that become runnable
// 轮询准备就绪的网络连接，返回可运行的goroutine列表。 block 表示是否阻塞，直到找到 g
func netpoll(block bool) gList {
	if epfd == -1 { // 还未初始化
		return gList{}
	}
	// waitms 默认为 -1 表示 epollwait 的时候一直等待
	waitms := int32(-1)
	if !block {
		waitms = 0 // 如果不阻塞，则设置为 0 ，表示 epollwait 的时候不等待
	}
	var events [128]epollevent
retry:
	// 系统调用 epollwait 获取就绪的网络事件
	n := epollwait(epfd, &events[0], int32(len(events)), waitms)
	if n < 0 {
		if n != -_EINTR {
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		// 未获取到，重新尝试
		goto retry
	}
	var toRun gList
	for i := int32(0); i < n; i++ {
		ev := &events[i]
		if ev.events == 0 {
			continue
		}
		var mode int32
		// 读事件
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'r'
		}
		// 些事件
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'w'
		}
		// 如果有需要处理的事件
		if mode != 0 {
			// 获取 pd ， pd 是在 netpollopen 的时候设置的
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data))

			// 让 pd ready ，将可运行的 goroutine （如果有）添加到 toRun
			netpollready(&toRun, pd, mode)
		}
	}
	// 如果是阻塞的，并且没有获取到，则重新尝试
	if block && toRun.empty() {
		goto retry
	}
	// 返回可运行的 G
	return toRun
}
