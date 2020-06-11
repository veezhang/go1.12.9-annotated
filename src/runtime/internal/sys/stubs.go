// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sys

// Declarations for runtime services implemented in C or assembly.

const PtrSize = 4 << (^uintptr(0) >> 63)           // unsafe.Sizeof(uintptr(0)) but an ideal const
const RegSize = 4 << (^Uintreg(0) >> 63)           // unsafe.Sizeof(uintreg(0)) but an ideal const
const SpAlign = 1*(1-GoarchArm64) + 16*GoarchArm64 // SP alignment: 1 normally, 16 for ARM64

var DefaultGoroot string // set at link time

// AIX requires a larger stack for syscalls.
// AIX需要更大的堆栈来进行系统调用。
// GoosAix系统，则 StackGuardMultiplier = 2；否则 StackGuardMultiplier = StackGuardMultiplierDefault
// GO_GCFLAGS有 "-N" (禁止优化) 的时候 StackGuardMultiplierDefault = 2，否则 StackGuardMultiplierDefault = 1 
const StackGuardMultiplier = StackGuardMultiplierDefault*(1-GoosAix) + 2*GoosAix
