// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/bytealg"
	"unsafe"
)

// The constant is known to the compiler.
// There is no fundamental theory behind this number.
const tmpStringBufSize = 32

type tmpBuf [tmpStringBufSize]byte

// concatstrings implements a Go string concatenation x+y+z+...
// The operands are passed in the slice a.
// If buf != nil, the compiler has determined that the result does not
// escape the calling function, so the string data can be stored in buf
// if small enough.
// concatstrings 字符串串联 x+y+z+... 操作数在切片 a 中传递。a = []string{x,y,z...}
// 如果 buf != nil ，则编译器已经确定结果不会逃调用函数，因此如果足够小，则可以将字符串数据存储在buf中。
func concatstrings(buf *tmpBuf, a []string) string {
	idx := 0   // 记录最后一个非空字符串的下标
	l := 0     // 记录总长度
	count := 0 // 记录非空字符串的个数
	for i, x := range a {
		n := len(x)
		if n == 0 {
			continue
		}
		// 溢出了
		if l+n < l {
			throw("string concatenation too long")
		}
		// 记录 l count idx
		l += n
		count++
		idx = i
	}
	if count == 0 {
		return ""
	}

	// If there is just one string and either it is not on the stack
	// or our result does not escape the calling frame (buf != nil),
	// then we can return that string directly.
	// 如果只有一个字符串，并且结果不会逃逸调用帧（buf！= nil），或者它不在栈中，那么我们可以直接返回该字符串。
	if count == 1 && (buf != nil || !stringDataOnStack(a[idx])) {
		return a[idx]
	}
	//
	s, b := rawstringtmp(buf, l)
	for _, x := range a {
		copy(b, x)
		b = b[len(x):]
	}
	return s
}

// 链接字符串，以下对字符串长度做了特化处理，有 2，3，4，5

func concatstring2(buf *tmpBuf, a [2]string) string {
	return concatstrings(buf, a[:])
}

func concatstring3(buf *tmpBuf, a [3]string) string {
	return concatstrings(buf, a[:])
}

func concatstring4(buf *tmpBuf, a [4]string) string {
	return concatstrings(buf, a[:])
}

func concatstring5(buf *tmpBuf, a [5]string) string {
	return concatstrings(buf, a[:])
}

// Buf is a fixed-size buffer for the result,
// it is not nil if the result does not escape.
// Buf 固定大小的缓冲区，用于返回结果的，如果结果没有逃逸，则不为 nil 。
func slicebytetostring(buf *tmpBuf, b []byte) (str string) {
	l := len(b)
	// 如果长度为 0 ，直接返回空字符串
	if l == 0 {
		// Turns out to be a relatively common case.
		// Consider that you want to parse out data between parens in "foo()bar",
		// you find the indices and convert the subslice to string.
		return ""
	}
	if raceenabled {
		racereadrangepc(unsafe.Pointer(&b[0]),
			uintptr(l),
			getcallerpc(),
			funcPC(slicebytetostring))
	}
	if msanenabled {
		msanread(unsafe.Pointer(&b[0]), uintptr(l))
	}
	// 如果长度为 1 ，返回静态表
	if l == 1 {
		stringStructOf(&str).str = unsafe.Pointer(&staticbytes[b[0]])
		stringStructOf(&str).len = 1
		return
	}

	// 如果 buf 不为空，并且足够容纳 b ，则使用它； 否则申请内存
	var p unsafe.Pointer
	if buf != nil && len(b) <= len(buf) {
		p = unsafe.Pointer(buf)
	} else {
		p = mallocgc(uintptr(len(b)), nil, false)
	}
	stringStructOf(&str).str = p
	stringStructOf(&str).len = len(b)
	// 把 b 处的内存直接复制到 p 处
	memmove(p, (*(*slice)(unsafe.Pointer(&b))).array, uintptr(len(b)))
	return
}

// stringDataOnStack reports whether the string's data is
// stored on the current goroutine's stack.
// stringDataOnStack 判断字符串是否在当前 goroutine 的栈上
func stringDataOnStack(s string) bool {
	ptr := uintptr(stringStructOf(&s).str)
	stk := getg().stack
	// 看是否在栈中间 [lo, hi)
	return stk.lo <= ptr && ptr < stk.hi
}

// rawstringtmp 返回长度为 l 的字符串和 []byte，并且字符串指向 []byte 的真实数据。
func rawstringtmp(buf *tmpBuf, l int) (s string, b []byte) {
	if buf != nil && l <= len(buf) {
		b = buf[:l]
		s = slicebytetostringtmp(b)
	} else {
		s, b = rawstring(l)
	}
	return
}

// slicebytetostringtmp returns a "string" referring to the actual []byte bytes.
//
// Callers need to ensure that the returned string will not be used after
// the calling goroutine modifies the original slice or synchronizes with
// another goroutine.
//
// The function is only called when instrumenting
// and otherwise intrinsified by the compiler.
//
// Some internal compiler optimizations use this function.
// - Used for m[T1{... Tn{..., string(k), ...} ...}] and m[string(k)]
//   where k is []byte, T1 to Tn is a nesting of struct and array literals.
// - Used for "<"+string(b)+">" concatenation where b is []byte.
// - Used for string(b)=="foo" comparison where b is []byte.
//
// slicebytetostringtmp 返回字符串指到 []byte 切片真正的字节。调用者需要确保返回的字符串切片修改后，不能使用返回的字符串。
// 该函数仅在检测时调用，否则由编译器内化。
func slicebytetostringtmp(b []byte) string {
	if raceenabled && len(b) > 0 {
		racereadrangepc(unsafe.Pointer(&b[0]),
			uintptr(len(b)),
			getcallerpc(),
			funcPC(slicebytetostringtmp))
	}
	if msanenabled && len(b) > 0 {
		msanread(unsafe.Pointer(&b[0]), uintptr(len(b)))
	}
	return *(*string)(unsafe.Pointer(&b))
}

// 字符串转 []byte
func stringtoslicebyte(buf *tmpBuf, s string) []byte {
	var b []byte
	if buf != nil && len(s) <= len(buf) {
		*buf = tmpBuf{}
		b = buf[:len(s)]
	} else {
		b = rawbyteslice(len(s))
	}
	copy(b, s)
	return b
}

// 字符串转 []rune
func stringtoslicerune(buf *[tmpStringBufSize]rune, s string) []rune {
	// two passes.
	// unlike slicerunetostring, no race because strings are immutable.
	// 与 slicerunetostring 不同，这里没有竞争，因为字符串是不可变的。
	n := 0
	for range s {
		n++
	}

	var a []rune
	if buf != nil && n <= len(buf) {
		*buf = [tmpStringBufSize]rune{}
		a = buf[:n]
	} else {
		a = rawruneslice(n)
	}

	n = 0
	for _, r := range s {
		a[n] = r
		n++
	}
	return a
}

func slicerunetostring(buf *tmpBuf, a []rune) string {
	if raceenabled && len(a) > 0 {
		racereadrangepc(unsafe.Pointer(&a[0]),
			uintptr(len(a))*unsafe.Sizeof(a[0]),
			getcallerpc(),
			funcPC(slicerunetostring))
	}
	if msanenabled && len(a) > 0 {
		msanread(unsafe.Pointer(&a[0]), uintptr(len(a))*unsafe.Sizeof(a[0]))
	}
	var dum [4]byte
	size1 := 0
	for _, r := range a {
		size1 += encoderune(dum[:], r)
	}
	s, b := rawstringtmp(buf, size1+3)
	size2 := 0
	for _, r := range a {
		// check for race
		if size2 >= size1 {
			break
		}
		size2 += encoderune(b[size2:], r)
	}
	return s[:size2]
}

// stringStruct 是 string 的底层数据结构
type stringStruct struct {
	str unsafe.Pointer
	len int
}

// Variant with *byte pointer type for DWARF debugging.
type stringStructDWARF struct {
	str *byte
	len int
}

// stringStructOf string 转 stringStruct
func stringStructOf(sp *string) *stringStruct {
	return (*stringStruct)(unsafe.Pointer(sp))
}

// int64 转字符串
func intstring(buf *[4]byte, v int64) (s string) {
	// 小于 runeSelf(0x80)，表示只有一个字符，也是使用了staticbytes
	if v >= 0 && v < runeSelf {
		stringStructOf(&s).str = unsafe.Pointer(&staticbytes[v])
		stringStructOf(&s).len = 1
		return
	}

	var b []byte
	if buf != nil {
		b = buf[:]
		s = slicebytetostringtmp(b)
	} else {
		s, b = rawstring(4)
	}
	if int64(rune(v)) != v {
		v = runeError
	}
	// 解析 rune ，存到 b 中， 并返回占用的字节数
	n := encoderune(b, rune(v))
	return s[:n]
}

// rawstring allocates storage for a new string. The returned
// string and byte slice both refer to the same storage.
// The storage is not zeroed. Callers should use
// b to set the string contents and then drop b.
// rawstring 为新字符串分配存储空间。 返回的字符串和 []byte 均引用同一存储。 存储未归零。 调用者应使用 b 设置字符串内容，然后丢弃 b 。
func rawstring(size int) (s string, b []byte) {
	p := mallocgc(uintptr(size), nil, false)

	stringStructOf(&s).str = p
	stringStructOf(&s).len = size

	*(*slice)(unsafe.Pointer(&b)) = slice{p, size, size}

	return
}

// rawbyteslice allocates a new byte slice. The byte slice is not zeroed.
// rawbyteslice 分配 []byte ，内存没有清零，多分配的会清零
func rawbyteslice(size int) (b []byte) {
	cap := roundupsize(uintptr(size))
	p := mallocgc(cap, nil, false)
	if cap != uintptr(size) {
		memclrNoHeapPointers(add(p, uintptr(size)), cap-uintptr(size))
	}

	*(*slice)(unsafe.Pointer(&b)) = slice{p, size, int(cap)}
	return
}

// rawruneslice allocates a new rune slice. The rune slice is not zeroed.
// rawbyteslice 分配 []rune ，内存没有清零，多分配的会清零
func rawruneslice(size int) (b []rune) {
	if uintptr(size) > maxAlloc/4 {
		throw("out of memory")
	}
	mem := roundupsize(uintptr(size) * 4)
	p := mallocgc(mem, nil, false)
	if mem != uintptr(size)*4 {
		memclrNoHeapPointers(add(p, uintptr(size)*4), mem-uintptr(size)*4)
	}

	*(*slice)(unsafe.Pointer(&b)) = slice{p, size, int(mem / 4)}
	return
}

// used by cmd/cgo
// 下面好几个都是 cmd/cgo 使用的

// 字节指针 + 长度 => 字符切片
func gobytes(p *byte, n int) (b []byte) {
	if n == 0 {
		return make([]byte, 0)
	}

	if n < 0 || uintptr(n) > maxAlloc {
		panic(errorString("gobytes: length out of range"))
	}

	// 分配并复制
	bp := mallocgc(uintptr(n), nil, false)
	memmove(bp, unsafe.Pointer(p), uintptr(n))

	*(*slice)(unsafe.Pointer(&b)) = slice{bp, n, n}
	return
}

// 字节指针 => 字符串
func gostring(p *byte) string {
	l := findnull(p)
	if l == 0 {
		return ""
	}
	s, b := rawstring(l)
	memmove(unsafe.Pointer(&b[0]), unsafe.Pointer(p), uintptr(l))
	return s
}

// 字节指针 + 长度 => 字符串
func gostringn(p *byte, l int) string {
	if l == 0 {
		return ""
	}
	s, b := rawstring(l)
	// 这里 b 用过后就丢弃了，跟 rawstring 中注释说明的一致
	memmove(unsafe.Pointer(&b[0]), unsafe.Pointer(p), uintptr(l))
	return s
}

// s 包含 t 的下标
func index(s, t string) int {
	if len(t) == 0 {
		return 0
	}
	for i := 0; i < len(s); i++ {
		if s[i] == t[0] && hasPrefix(s[i:], t) {
			return i
		}
	}
	return -1
}

// 判断 s 是否包含 t
func contains(s, t string) bool {
	return index(s, t) >= 0
}

// 判断 s 是否包含前缀 prefix
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

const (
	maxUint = ^uint(0)
	maxInt  = int(maxUint >> 1)
)

// atoi parses an int from a string s.
// The bool result reports whether s is a number
// representable by a value of type int.
// atoi 字符串转 int
func atoi(s string) (int, bool) {
	// 空字符串
	if s == "" {
		return 0, false
	}

	// 负号判断
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}

	// un 定义为 uint ，防止溢出
	un := uint(0)
	// 遍历字符串来解析
	for i := 0; i < len(s); i++ {
		c := s[i]
		// 不为数字，直接返回
		if c < '0' || c > '9' {
			return 0, false
		}
		// 溢出了
		if un > maxUint/10 {
			// overflow
			return 0, false
		}
		un *= 10
		un1 := un + uint(c) - '0'
		// 溢出
		if un1 < un {
			// overflow
			return 0, false
		}
		un = un1
	}

	// 正数，如果大于 maxInt 则溢出
	if !neg && un > uint(maxInt) {
		return 0, false
	}
	// 负数，如果大于 maxInt + 1 则溢出
	if neg && un > uint(maxInt)+1 {
		return 0, false
	}

	// 转为 int ，并加上符号位
	n := int(un)
	if neg {
		n = -n
	}

	return n, true
}

// atoi32 is like atoi but for integers
// that fit into an int32.
// atoi 字符串转 int32
func atoi32(s string) (int32, bool) {
	if n, ok := atoi(s); n == int(int32(n)) {
		return int32(n), ok
	}
	return 0, false
}

// findnull 找 NULL
//go:nosplit
func findnull(s *byte) int {
	// 为 nil
	if s == nil {
		return 0
	}

	// Avoid IndexByteString on Plan 9 because it uses SSE instructions
	// on x86 machines, and those are classified as floating point instructions,
	// which are illegal in a note handler.
	// 在 Plan 9 上避免使用 IndexByteString ，因为它在 x86 机器上使用 SSE 指令，并且这些指令被归类为浮点指令。
	if GOOS == "plan9" {
		p := (*[maxAlloc/2 - 1]byte)(unsafe.Pointer(s))
		l := 0
		for p[l] != 0 {
			l++
		}
		return l
	}

	// pageSize is the unit we scan at a time looking for NULL.
	// It must be the minimum page size for any architecture Go
	// runs on. It's okay (just a minor performance loss) if the
	// actual system page size is larger than this value.
	// pageSize 是我们一次扫描以查找 NULL 的单位。 它必须是 Go 运行的任何体系结构的最小页面大小。
	// 如果实际的系统页面大小大于此值，则可以（只有很小的性能损失）。
	const pageSize = 4096

	offset := 0
	ptr := unsafe.Pointer(s)
	// IndexByteString uses wide reads, so we need to be careful
	// with page boundaries. Call IndexByteString on
	// [ptr, endOfPage) interval.
	// IndexByteString 使用宽读取，因此我们需要注意页面边界。 以 [ptr, endOfPage) 间隔调用 IndexByteString
	safeLen := int(pageSize - uintptr(ptr)%pageSize)

	for {
		t := *(*string)(unsafe.Pointer(&stringStruct{ptr, safeLen}))
		// Check one page at a time.
		// 一次检测一个 page
		if i := bytealg.IndexByteString(t, 0); i != -1 {
			return offset + i
		}
		// Move to next page
		// 到下一个 page
		ptr = unsafe.Pointer(uintptr(ptr) + uintptr(safeLen))
		offset += safeLen
		safeLen = pageSize
	}
}

// 双字节查找 NULL
func findnullw(s *uint16) int {
	if s == nil {
		return 0
	}
	p := (*[maxAlloc/2/2 - 1]uint16)(unsafe.Pointer(s))
	l := 0
	for p[l] != 0 {
		l++
	}
	return l
}

// 零拷贝 字节指针 => 字符串
//go:nosplit
func gostringnocopy(str *byte) string {
	ss := stringStruct{str: unsafe.Pointer(str), len: findnull(str)}
	s := *(*string)(unsafe.Pointer(&ss))
	return s
}

// 零拷贝 双字符字节指针 => 字符串
func gostringw(strw *uint16) string {
	var buf [8]byte
	str := (*[maxAlloc/2/2 - 1]uint16)(unsafe.Pointer(strw))
	n1 := 0
	for i := 0; str[i] != 0; i++ {
		n1 += encoderune(buf[:], rune(str[i]))
	}
	s, b := rawstring(n1 + 4)
	n2 := 0
	for i := 0; str[i] != 0; i++ {
		// check for race
		if n2 >= n1 {
			break
		}
		n2 += encoderune(b[n2:], rune(str[i]))
	}
	b[n2] = 0 // for luck
	return s[:n2]
}
