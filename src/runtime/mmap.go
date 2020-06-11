// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !plan9
// +build !solaris
// +build !windows
// +build !nacl
// +build !linux !amd64
// +build !linux !arm64
// +build !js
// +build !darwin
// +build !aix

package runtime

import "unsafe"

// mmap calls the mmap system call. It is implemented in assembly.
// We only pass the lower 32 bits of file offset to the
// assembly routine; the higher bits (if required), should be provided
// by the assembly routine as 0.
// The err result is an OS error code such as ENOMEM.
// mmap调用mmap系统调用。它是在汇编中实现的。我们只将文件偏移量的低32位传递给汇编例程。较高的位（如果需要）应由汇编例程提供为0。错误结果是OS错误代码，例如ENOMEM。
func mmap(addr unsafe.Pointer, n uintptr, prot, flags, fd int32, off uint32) (p unsafe.Pointer, err int)

// munmap calls the munmap system call. It is implemented in assembly.
// munmap调用munmap系统调用。 它是在汇编中实现的。
func munmap(addr unsafe.Pointer, n uintptr)
