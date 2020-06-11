// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix darwin dragonfly freebsd linux netbsd openbsd solaris windows

package net

import (
	"internal/bytealg"
	"runtime"
	"syscall"
)

// Boolean to int.
func boolint(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ipv4AddrToInterface(ip IP) (*Interface, error) {
	ift, err := Interfaces()
	if err != nil {
		return nil, err
	}
	for _, ifi := range ift {
		ifat, err := ifi.Addrs()
		if err != nil {
			return nil, err
		}
		for _, ifa := range ifat {
			switch v := ifa.(type) {
			case *IPAddr:
				if ip.Equal(v.IP) {
					return &ifi, nil
				}
			case *IPNet:
				if ip.Equal(v.IP) {
					return &ifi, nil
				}
			}
		}
	}
	if ip.Equal(IPv4zero) {
		return nil, nil
	}
	return nil, errNoSuchInterface
}

func interfaceToIPv4Addr(ifi *Interface) (IP, error) {
	if ifi == nil {
		return IPv4zero, nil
	}
	ifat, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	for _, ifa := range ifat {
		switch v := ifa.(type) {
		case *IPAddr:
			if v.IP.To4() != nil {
				return v.IP, nil
			}
		case *IPNet:
			if v.IP.To4() != nil {
				return v.IP, nil
			}
		}
	}
	return nil, errNoSuchInterface
}

func setIPv4MreqToInterface(mreq *syscall.IPMreq, ifi *Interface) error {
	if ifi == nil {
		return nil
	}
	ifat, err := ifi.Addrs()
	if err != nil {
		return err
	}
	for _, ifa := range ifat {
		switch v := ifa.(type) {
		case *IPAddr:
			if a := v.IP.To4(); a != nil {
				copy(mreq.Interface[:], a)
				goto done
			}
		case *IPNet:
			if a := v.IP.To4(); a != nil {
				copy(mreq.Interface[:], a)
				goto done
			}
		}
	}
done:
	if bytealg.Equal(mreq.Multiaddr[:], IPv4zero.To4()) {
		return errNoSuchMulticastInterface
	}
	return nil
}

func setReadBuffer(fd *netFD, bytes int) error {
	err := fd.pfd.SetsockoptInt(syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
	runtime.KeepAlive(fd)
	return wrapSyscallError("setsockopt", err)
}

func setWriteBuffer(fd *netFD, bytes int) error {
	err := fd.pfd.SetsockoptInt(syscall.SOL_SOCKET, syscall.SO_SNDBUF, bytes)
	runtime.KeepAlive(fd)
	return wrapSyscallError("setsockopt", err)
}

// 设置 keepalive
func setKeepAlive(fd *netFD, keepalive bool) error {
	// 调用 setsockopt 系统调用: setsockopt(fd.Sysfd, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, keepalive)
	err := fd.pfd.SetsockoptInt(syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, boolint(keepalive))
	runtime.KeepAlive(fd)
	return wrapSyscallError("setsockopt", err)
}

// setLinger 设置 SOCKET 在 CLOSE 时候是否等待缓冲区发送完成。
// 如果 sec < 0（默认值），则操作系统将在后台完成数据发送。
// 如果sec == 0，则操作系统将丢弃所有未发送或未确认的数据。
// 如果 sec > 0 ，则与 sec < 0 一样在后台发送数据。在某些操作系统上，经过 sec 秒后，可能会丢弃所有剩余的未发送数据。
func setLinger(fd *netFD, sec int) error {
	var l syscall.Linger
	if sec >= 0 {
		l.Onoff = 1
		l.Linger = int32(sec)
	} else {
		l.Onoff = 0
		l.Linger = 0
	}
	// 调用 setsockopt 系统调用: setsockopt(fd.Sysfd, syscall.SOL_SOCKET, syscall.SO_LINGER, &l)
	err := fd.pfd.SetsockoptLinger(syscall.SOL_SOCKET, syscall.SO_LINGER, &l)
	runtime.KeepAlive(fd)
	return wrapSyscallError("setsockopt", err)
}
