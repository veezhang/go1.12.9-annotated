// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"internal/poll"
	"io"
)

// splice transfers data from r to c using the splice system call to minimize
// copies from and to userspace. c must be a TCP connection. Currently, splice
// is only enabled if r is a TCP or a stream-oriented Unix connection.
//
// If splice returns handled == false, it has performed no work.
// splice 使用 splice 系统调用将数据从 r 传输到 c ，以最小化来回用户空间的数据拷贝。 c必须是TCP连接。
// 当前，仅当 r 是 TCP 或 面向流的Unix连接时才启用 splice 。
//
// 如果返回的 handled == false，则它不执行任何工作。
func splice(c *netFD, r io.Reader) (written int64, err error, handled bool) {
	var remain int64 = 1 << 62 // by default, copy until EOF
	lr, ok := r.(*io.LimitedReader)
	if ok {
		remain, r = lr.N, lr.R
		// 如果是 io.LimitedReader ， 并且没有空间了
		if remain <= 0 {
			return 0, nil, true
		}
	}

	var s *netFD
	if tc, ok := r.(*TCPConn); ok {
		s = tc.fd
	} else if uc, ok := r.(*UnixConn); ok {
		if uc.fd.net != "unix" {
			return 0, nil, false
		}
		s = uc.fd
	} else {
		return 0, nil, false
	}

	// 调用 poll.Splice ，最终会调用 splice 系统调用
	written, handled, sc, err := poll.Splice(&c.pfd, &s.pfd, remain)
	if lr != nil {
		lr.N -= written
	}
	return written, wrapSyscallError(sc, err), handled
}
