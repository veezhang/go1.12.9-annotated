// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bytealg

import "internal/cpu"

const MaxBruteForce = 64

func init() {
	// 如果有 AVX2 指令，设置为 63 ，否则设置为 31
	if cpu.X86.HasAVX2 {
		MaxLen = 63
	} else {
		MaxLen = 31
	}
}

// Cutover reports the number of failures of IndexByte we should tolerate
// before switching over to Index.
// n is the number of bytes processed so far.
// See the bytes.Index implementation for details.
// Cutover 报告了切换到 Index 之前我们应该容忍的IndexByte失败次数。
// n 是到目前为止已处理的字节数。
// 有关详细信息，请参见bytes.Index实现。
func Cutover(n int) int {
	// 1 error per 8 characters, plus a few slop to start.
	// 每 8 个字符 1 个错误，加上一些起始倾斜。
	return (n + 16) / 8
}
