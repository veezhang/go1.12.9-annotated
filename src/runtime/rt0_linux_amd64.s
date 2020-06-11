// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "textflag.h"

//  linux amd64 系统的启动函数
TEXT _rt0_amd64_linux(SB),NOSPLIT,$-8
    JMP _rt0_amd64(SB) // 跳转到_rt0_amd64函数， 在 asm_amd64.s 中。

TEXT _rt0_amd64_linux_lib(SB),NOSPLIT,$0
    JMP _rt0_amd64_lib(SB)
