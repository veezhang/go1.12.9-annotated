// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package strings

// stringFinder efficiently finds strings in a source text. It's implemented
// using the Boyer-Moore string search algorithm:
// https://en.wikipedia.org/wiki/Boyer-Moore_string_search_algorithm
// https://www.cs.utexas.edu/~moore/publications/fstrpos.pdf (note: this aged
// document uses 1-based indexing)
// stringFinder 查找字符串， BM 算法
type stringFinder struct {
	// pattern is the string that we are searching for in the text.
	pattern string

	// badCharSkip[b] contains the distance between the last byte of pattern
	// and the rightmost occurrence of b in pattern. If b is not in pattern,
	// badCharSkip[b] is len(pattern).
	// badCharSkip[b] 包含 pattern 的最后一个字节与 pattern 中最右的b出现之间的距离。 如果b不在模式中，则 badCharSkip[b] 为 len(pattern) 。
	//
	// Whenever a mismatch is found with byte b in the text, we can safely
	// shift the matching frame at least badCharSkip[b] until the next time
	// the matching char could be in alignment.
	// 只要发现 text 中的字节 b 不匹配，我们就可以安全地将匹配帧至少移至 badCharSkip[b] ，直到下次匹配的 char 可以对齐为止。
	badCharSkip [256]int // 坏字符表

	// goodSuffixSkip[i] defines how far we can shift the matching frame given
	// that the suffix pattern[i+1:] matches, but the byte pattern[i] does
	// not. There are two cases to consider:
	// goodSuffixSkip[i] 定义了如果后缀模式 pattern[i+1:] 匹配，我们可以将匹配帧移动多远，而字节模式 pattern[i] 不匹配。 有两种情况需要考虑：
	//
	// 1. The matched suffix occurs elsewhere in pattern (with a different
	// byte preceding it that we might possibly match). In this case, we can
	// shift the matching frame to align with the next suffix chunk. For
	// example, the pattern "mississi" has the suffix "issi" next occurring
	// (in right-to-left order) at index 1, so goodSuffixSkip[3] ==
	// shift+len(suffix) == 3+4 == 7.
	// 1. 匹配的后缀出现在模式中的其他位置（我们可能会匹配一个不同的字节）。 在这种情况下，我们可以移动匹配的框架以使其与下一个后缀块对齐。
	// 例如，pattern "mississi" 的下一个下标 "issi" 出现在索引 1 处（从右到左顺序），因此 goodSuffixSkip[3] == shift+len(suffix) == 3+4 == 7 。
	//
	//
	// 2. If the matched suffix does not occur elsewhere in pattern, then the
	// matching frame may share part of its prefix with the end of the
	// matching suffix. In this case, goodSuffixSkip[i] will contain how far
	// to shift the frame to align this portion of the prefix to the
	// suffix. For example, in the pattern "abcxxxabc", when the first
	// mismatch from the back is found to be in position 3, the matching
	// suffix "xxabc" is not found elsewhere in the pattern. However, its
	// rightmost "abc" (at position 6) is a prefix of the whole pattern, so
	// goodSuffixSkip[3] == shift+len(suffix) == 6+5 == 11.
	// 2. 如果匹配的后缀未在 pattern 中的其他位置出现，则匹配的帧可能会与匹配的后缀的末尾共享部分前缀。 在这种情况下，goodSuffixSkip[i] 将
	// 包含将帧移动多远以使前缀的此部分与后缀对齐。 例如，在 pattern "abcxxxabc" 中，当发现从后面开始的第一个不匹配项位于位置 3 时，在 pattern
	// 的其他位置找不到匹配的后缀 "xxabc" 。 但是，其最右边的 "abc"（在位置6）是整个 pattern 的前缀，因此 goodSuffixSkip[3] == shift+len(suffix) == 6+5 == 11 。
	goodSuffixSkip []int // 好后缀
}

func makeStringFinder(pattern string) *stringFinder {
	f := &stringFinder{
		pattern:        pattern,
		goodSuffixSkip: make([]int, len(pattern)),
	}
	// last is the index of the last character in the pattern.
	// last 是 pattern 中最后一个字符的索引
	last := len(pattern) - 1

	// Build bad character table.
	// Bytes not in the pattern can skip one pattern's length.
	// 构建 badCharSkip ，初始化为 len(pattern)
	// 构建坏字符表 badCharSkip 。不在模式中的字节可以跳过一个 pattern 的长度。
	for i := range f.badCharSkip {
		f.badCharSkip[i] = len(pattern)
	}
	// The loop condition is < instead of <= so that the last byte does not
	// have a zero distance to itself. Finding this byte out of place implies
	// that it is not in the last position.
	// 循环条件是 < 而不是 <= ，因此最后一个字节与其自身之间的距离不为零。 如果发现此字节不正确，则表明它不在最后一个位置。
	// 从前往后遍历，更新存在的字符的 badCharSkip
	for i := 0; i < last; i++ {
		f.badCharSkip[pattern[i]] = last - i
	}

	// Build good suffix table.
	// First pass: set each value to the next index which starts a prefix of
	// pattern.
	// 构建好后缀表 goodSuffixSkip
	// First ：好后缀的前缀匹配。上面说的第 2 点。
	lastPrefix := last
	for i := last; i >= 0; i-- {
		// 是否有好后缀的前缀匹配，比如 DDABDDD 中 D, DD 就是
		if HasPrefix(pattern, pattern[i+1:]) {
			lastPrefix = i + 1
		}
		// lastPrefix is the shift, and (last-i) is len(suffix).
		// 比如上面的：(abcxxxabc, last = 8)
		// goodSuffixSkip[3] == shift+len(suffix) == 6+5 == 11
		// i = 3 的时候 lastPrefix = 3 + 1 = 4, (last-i) = 8 - 3 = 5 , lastPrefix + last - i = 4 + 5 = 9
		f.goodSuffixSkip[i] = lastPrefix + last - i
	}
	// Second pass: find repeats of pattern's suffix starting from the front.
	// Second： 从前面开始查找 pattern 相同的后缀。上面说得第 1 点。
	for i := 0; i < last; i++ {
		// 获取最长后缀
		lenSuffix := longestCommonSuffix(pattern, pattern[1:i+1])
		if pattern[i-lenSuffix] != pattern[last-lenSuffix] {
			// (last-i) is the shift, and lenSuffix is len(suffix).
			f.goodSuffixSkip[last-lenSuffix] = lenSuffix + last - i
		}
	}

	return f
}

// 最长公共前缀
func longestCommonSuffix(a, b string) (i int) {
	for ; i < len(a) && i < len(b); i++ {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			break
		}
	}
	return
}

// next returns the index in text of the first occurrence of the pattern. If
// the pattern is not found, it returns -1.
// next 返回第一次出现的 pattern 的索引。 如果找不到该模式，则返回-1。
func (f *stringFinder) next(text string) int {
	i := len(f.pattern) - 1
	for i < len(text) {
		// Compare backwards from the end until the first unmatching character.
		// 从后往前比较
		j := len(f.pattern) - 1
		for j >= 0 && text[i] == f.pattern[j] {
			i--
			j--
		}
		//  匹配上了
		if j < 0 {
			return i + 1 // match
		}
		// 没匹配上，去 badCharSkip 和 goodSuffixSkip 中较大者
		i += max(f.badCharSkip[text[i]], f.goodSuffixSkip[j])
	}
	return -1
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
