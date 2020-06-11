// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build amd64 amd64p32 386

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then did an immediate gosave.、
// 调整 Gobuf ，就好像它使用上下文 ctxt 执行了对 fn 的调用，然后立即执行了 gosave 一样。
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	// newg 的栈顶，目前 newg 栈上只有 fn 函数的参数， sp 指向的是 fn 的第一参数
	sp := buf.sp
	if sys.RegSize > sys.PtrSize {
		sp -= sys.PtrSize
		*(*uintptr)(unsafe.Pointer(sp)) = 0
	}
	sp -= sys.PtrSize // 栈空间是高地址向低地址增长，为返回地址预留空间
	// 这里在伪装 fn 是被 goexit 函数调用的，使得 fn 执行完后返回到 goexit 继续执行，从而完成清理工作
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc // 在栈上放入 goexit+1 的地址
	buf.sp = sp                              // 新设置 newg 的栈顶寄存器
	// 这里才真正让 newg 的 pc 寄存器指向 fn 函数，等到 newg 被调度起来运行时，调度器会把 buf.pc 放入 cpu 的 IP 寄存器，
	// 从而使 newg 得以在 cpu 上真正的运行起来。
	buf.pc = uintptr(fn)
	buf.ctxt = ctxt
}
