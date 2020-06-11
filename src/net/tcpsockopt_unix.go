// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix freebsd linux netbsd

package net

import (
	"runtime"
	"syscall"
	"time"
)

// 设置 KeepAlive 周期
func setKeepAlivePeriod(fd *netFD, d time.Duration) error {
	// The kernel expects seconds so round to next highest second.
	// 内核期望将秒数舍入到下一个最高秒。
	d += (time.Second - time.Nanosecond)
	secs := int(d.Seconds())
	// TCP_KEEPINTVL 设置探测间隔
	if err := fd.pfd.SetsockoptInt(syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, secs); err != nil {
		return wrapSyscallError("setsockopt", err)
	}
	// TCP_KEEPIDLE 多久没数据后探测
	err := fd.pfd.SetsockoptInt(syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, secs)
	runtime.KeepAlive(fd)
	return wrapSyscallError("setsockopt", err)
}
