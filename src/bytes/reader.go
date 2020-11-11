// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bytes

import (
	"errors"
	"io"
	"unicode/utf8"
)

// A Reader implements the io.Reader, io.ReaderAt, io.WriterTo, io.Seeker,
// io.ByteScanner, and io.RuneScanner interfaces by reading from
// a byte slice.
// Unlike a Buffer, a Reader is read-only and supports seeking.
// The zero value for Reader operates like a Reader of an empty slice.
// Reader 实现 []byte 的接口： io.Reader, io.ReaderAt, io.WriterTo, io.Seeker, io.ByteScanner, io.RuneScanner
type Reader struct {
	s        []byte
	i        int64 // current reading index				// 读取下标
	prevRune int   // index of previous rune; or < 0	// 前一个读取的
}

// Len returns the number of bytes of the unread portion of the
// slice.
// Len 返回还未读的长度
func (r *Reader) Len() int {
	if r.i >= int64(len(r.s)) {
		return 0
	}
	return int(int64(len(r.s)) - r.i)
}

// Size returns the original length of the underlying byte slice.
// Size is the number of bytes available for reading via ReadAt.
// The returned value is always the same and is not affected by calls
// to any other method.
// Size 返回原始长度，与是否读了无关
func (r *Reader) Size() int64 { return int64(len(r.s)) }

// Read implements the io.Reader interface.
// Read 实现 io.Reader 接口
func (r *Reader) Read(b []byte) (n int, err error) {
	// 已经读完了，返回 EOF
	if r.i >= int64(len(r.s)) {
		return 0, io.EOF
	}
	// 读取 r.s[r.i:] 到 b 中
	r.prevRune = -1
	n = copy(b, r.s[r.i:])
	r.i += int64(n)
	return
}

// ReadAt implements the io.ReaderAt interface.
// ReadAt 实现 io.ReaderAt 接口
func (r *Reader) ReadAt(b []byte, off int64) (n int, err error) {
	// cannot modify state - see io.ReaderAt
	if off < 0 {
		return 0, errors.New("bytes.Reader.ReadAt: negative offset")
	}
	// 读越界，返回 EOF
	if off >= int64(len(r.s)) {
		return 0, io.EOF
	}
	// 读取 r.s[off:] 到 b 中
	n = copy(b, r.s[off:])
	if n < len(b) {
		err = io.EOF // 读完了
	}
	return
}

// ReadByte implements the io.ByteReader interface.
// ReadByte 实现 io.ByteReader 接口
func (r *Reader) ReadByte() (byte, error) {
	r.prevRune = -1
	// 已经读完了，返回 EOF
	if r.i >= int64(len(r.s)) {
		return 0, io.EOF
	}
	// 读取一个字节
	b := r.s[r.i]
	r.i++
	return b, nil
}

// UnreadByte complements ReadByte in implementing the io.ByteScanner interface.
// UnreadByte 实现 io.ByteScanner 接口
func (r *Reader) UnreadByte() error {
	if r.i <= 0 {
		return errors.New("bytes.Reader.UnreadByte: at beginning of slice")
	}
	// 恢复一个已读的 byte
	r.prevRune = -1
	r.i--
	return nil
}

// ReadRune implements the io.RuneReader interface.
// ReadRune 实现 io.RuneReader 接口
func (r *Reader) ReadRune() (ch rune, size int, err error) {
	// 已经读完了，返回 EOF
	if r.i >= int64(len(r.s)) {
		r.prevRune = -1
		return 0, 0, io.EOF
	}
	// 记录前一个读取的位置
	r.prevRune = int(r.i)
	// 如果是字符
	if c := r.s[r.i]; c < utf8.RuneSelf {
		r.i++
		return rune(c), 1, nil
	}
	// 非字符
	ch, size = utf8.DecodeRune(r.s[r.i:])
	r.i += int64(size)
	return
}

// UnreadRune complements ReadRune in implementing the io.RuneScanner interface.
// UnreadRune 实现 io.RuneScanner 接口
func (r *Reader) UnreadRune() error {
	if r.i <= 0 {
		return errors.New("bytes.Reader.UnreadRune: at beginning of slice")
	}
	// 前一个不是 ReadRune ，只能记一个
	if r.prevRune < 0 {
		return errors.New("bytes.Reader.UnreadRune: previous operation was not ReadRune")
	}
	// 恢复一个已读的 rune
	r.i = int64(r.prevRune)
	r.prevRune = -1
	return nil
}

// Seek implements the io.Seeker interface.
// Seek 实现 io.Seeker 接口
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.prevRune = -1
	var abs int64 // 绝对位置
	switch whence {
	case io.SeekStart: // 从头部
		abs = offset
	case io.SeekCurrent: // 从当前
		abs = r.i + offset
	case io.SeekEnd: // 从尾部
		abs = int64(len(r.s)) + offset
	default:
		return 0, errors.New("bytes.Reader.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("bytes.Reader.Seek: negative position")
	}
	r.i = abs
	return abs, nil
}

// WriteTo implements the io.WriterTo interface.
// WriteTo 实现 io.WriterTo 接口
func (r *Reader) WriteTo(w io.Writer) (n int64, err error) {
	r.prevRune = -1
	if r.i >= int64(len(r.s)) {
		return 0, nil
	}
	b := r.s[r.i:]
	m, err := w.Write(b)
	if m > len(b) {
		panic("bytes.Reader.WriteTo: invalid Write count")
	}
	r.i += int64(m)
	n = int64(m)
	if m != len(b) && err == nil {
		err = io.ErrShortWrite
	}
	return
}

// Reset resets the Reader to be reading from b.
// Reset 重设
func (r *Reader) Reset(b []byte) { *r = Reader{b, 0, -1} }

// NewReader returns a new Reader reading from b.
// NewReader 创建 bytes.Reader
func NewReader(b []byte) *Reader { return &Reader{b, 0, -1} }
