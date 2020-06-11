// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package strings

import (
	"io"
	"sync"
)

// Replacer replaces a list of strings with replacements.
// It is safe for concurrent use by multiple goroutines.
// Replacer 替换字符串，并发安全
type Replacer struct {
	once   sync.Once // guards buildOnce method
	r      replacer  // 接口，根据具体数据生成不同的
	oldnew []string  // 旧的新的 字符串对，把旧的替换成新的。
}

// replacer is the interface that a replacement algorithm needs to implement.
// replacer 是替换算法需要实现的接口
type replacer interface {
	Replace(s string) string                              // 替换, 然后返回
	WriteString(w io.Writer, s string) (n int, err error) // 替换，然后写入 w
}

// NewReplacer returns a new Replacer from a list of old, new string
// pairs. Replacements are performed in the order they appear in the
// target string, without overlapping matches.
// NewReplacer 根据 旧的新的 字符串对来创建 Replacer 。把旧的替换成新的。
func NewReplacer(oldnew ...string) *Replacer {
	if len(oldnew)%2 == 1 {
		panic("strings.NewReplacer: odd argument count")
	}
	return &Replacer{oldnew: append([]string(nil), oldnew...)}
}

// 构建，也就根据 oldnew 生成 replacer 接口的具体实现。
func (r *Replacer) buildOnce() {
	r.r = r.build()
	r.oldnew = nil
}

// 优先顺序：
// makeSingleStringReplacer			只有一对需要替换， 使用 BM 算法
// byteReplacer						一个字节替换一个字节
// byteStringReplacer				一个字节替换一个字符串
// makeGenericReplacer				通用的算法（其它情况）
func (b *Replacer) build() replacer {
	oldnew := b.oldnew
	// 如果只有一对，生成 makeSingleStringReplacer
	if len(oldnew) == 2 && len(oldnew[0]) > 1 {
		return makeSingleStringReplacer(oldnew[0], oldnew[1])
	}

	// allNewBytes 表示所有旧的新的字符串长度都为 1 ， 久的如果长度都不为 1 会使用 makeGenericReplacer
	allNewBytes := true
	for i := 0; i < len(oldnew); i += 2 {
		// 只要有旧的长度不为 1 ，使用 makeGenericReplacer 来创建
		if len(oldnew[i]) != 1 {
			return makeGenericReplacer(oldnew)
		}
		if len(oldnew[i+1]) != 1 {
			allNewBytes = false
		}
	}

	// 所有旧的新的字符串长度都为 1 ， 也就是把 1 个久字节替换成 1 个新字节， 使用 byteReplacer
	if allNewBytes {
		r := byteReplacer{}
		for i := range r {
			r[i] = byte(i)
		}
		// The first occurrence of old->new map takes precedence
		// over the others with the same old string.
		// 第一次出现的优先，所以从后往前遍历
		for i := len(oldnew) - 2; i >= 0; i -= 2 {
			o := oldnew[i][0]
			n := oldnew[i+1][0]
			r[o] = n
		}
		return &r
	}

	// 这里久的长度为 1 ，新的长度不为 1 ，也就是把一个旧的 字节 替换成新的字符串
	r := byteStringReplacer{toReplace: make([]string, 0, len(oldnew)/2)}
	// The first occurrence of old->new map takes precedence
	// over the others with the same old string.
	// 第一次出现的优先，所以从后往前遍历
	for i := len(oldnew) - 2; i >= 0; i -= 2 {
		o := oldnew[i][0] // o 是旧的一个字节
		n := oldnew[i+1]  // n 是新的字符串
		// To avoid counting repetitions multiple times.
		// 避免多次重复计算
		if r.replacements[o] == nil {
			// We need to use string([]byte{o}) instead of string(o),
			// to avoid utf8 encoding of o.
			// E. g. byte(150) produces string of length 2.
			// 使用 string([]byte{o}) 而不是 string(o)，以避免对 o 进行 utf8 编码。 例如 byte(150) 产生长度为 2 的字符串。
			r.toReplace = append(r.toReplace, string([]byte{o}))
		}
		r.replacements[o] = []byte(n)

	}
	return &r
}

// Replace returns a copy of s with all replacements performed.
// Replace 返回已经替换好了的 s 的副本
func (r *Replacer) Replace(s string) string {
	r.once.Do(r.buildOnce) // 先build
	return r.r.Replace(s)  // 然后执行替换
}

// WriteString writes s to w with all replacements performed.
// WriteString 将已经替换好了的 s 的副本写入 w
func (r *Replacer) WriteString(w io.Writer, s string) (n int, err error) {
	r.once.Do(r.buildOnce)
	return r.r.WriteString(w, s)
}

// 字典树（又称查找树，键树）：
// 1. 根节点不包含字符，除根节点外每一个节点都只包含一个字符。
// 2. 从根节点到某一节点，路径上经过的字符连接起来，为该节点对应的字符串。
// 3. 每个节点的所有子节点包含的字符都不相同。
//
// 这里做了一些优化，如果只有一个子节点的时候，使用 prefix 来减少内存

// trieNode 跟字典树有一些不同，每一个节点不只是包含一个字符，有分叉的时候再分。
// trieNode is a node in a lookup trie for prioritized key/value pairs. Keys
// and values may be empty. For example, the trie containing keys "ax", "ay",
// "bcbc", "x" and "xy" could have eight nodes:
//
//  n0  -
//  n1  a-
//  n2  .x+
//  n3  .y+
//  n4  b-
//  n5  .cbc+
//  n6  x+
//  n7  .y+
//
// n0 is the root node, and its children are n1, n4 and n6; n1's children are
// n2 and n3; n4's child is n5; n6's child is n7. Nodes n0, n1 and n4 (marked
// with a trailing "-") are partial keys, and nodes n2, n3, n5, n6 and n7
// (marked with a trailing "+") are complete keys.
type trieNode struct {
	// value is the value of the trie node's key/value pair. It is empty if
	// this node is not a complete key.
	value string // 键值对的值，如果 key 不完整则为空。
	// priority is the priority (higher is more important) of the trie node's
	// key/value pair; keys are not necessarily matched shortest- or longest-
	// first. Priority is positive if this node is a complete key, and zero
	// otherwise. In the example above, positive/zero priorities are marked
	// with a trailing "+" or "-".
	// priority 是 trie 节点的键/值对的优先级（越高，则越重要）； 键不一定与最短或最长优先匹配。 如果此节点是完整 key，则优先级为正，否则为零。
	// 在上面的示例中，正/零优先级用结尾的 "+" 或 "-" 标记。
	priority int

	// A trie node may have zero, one or more child nodes:
	//  * if the remaining fields are zero, there are no children.
	//  * if prefix and next are non-zero, there is one child in next.
	//  * if table is non-zero, it defines all the children.
	// trie 节点可能有 0, 1 或者多个子节点：
	//  * 如果下面的字符段为空，则没有子节点
	//  * 如果 prefix 和 next 不为空，则有一个子节点
	//  * 如果 table 不为空，则 table 中定义了多有的子节点
	//
	// Prefixes are preferred over tables when there is one child, but the
	// root node always uses a table for lookup efficiency.
	// 当有一个子节点时， prefix 优先于 table ，但 root 节点始终使用 table 来提高查找效率。

	// prefix is the difference in keys between this trie node and the next.
	// In the example above, node n4 has prefix "cbc" and n4's next node is n5.
	// Node n5 has no children and so has zero prefix, next and table fields.
	// prefix 是当前节点和下一个节点之间的差异。
	prefix string
	next   *trieNode

	// table is a lookup table indexed by the next byte in the key, after
	// remapping that byte through genericReplacer.mapping to create a dense
	// index. In the example above, the keys only use 'a', 'b', 'c', 'x' and
	// 'y', which remap to 0, 1, 2, 3 and 4. All other bytes remap to 5, and
	// genericReplacer.tableSize will be 5. Node n0's table will be
	// []*trieNode{ 0:n1, 1:n4, 3:n6 }, where the 0, 1 and 3 are the remapped
	// 'a', 'b' and 'x'.
	// table 是根据下一个字节索引的查询表，通过 genericReplacer.mapping 重新映射该字节以创建密集索引。
	// 在上面的示例中，键仅使用 'a', 'b', 'c', 'x'，它们重新映射为0、1、2、3和4。所有其他字节都重新映射为5， genericReplacer.tableSize 将为 5。
	// 节点 n0 的表将为 []*trieNode{ 0:n1, 1:n4, 3:n6 } ，其中0、1和3是重新映射的 'a'，'b'和'x ' 。
	table []*trieNode
}

// 添加键值对
func (t *trieNode) add(key, val string, priority int, r *genericReplacer) {
	// 如果 key 为空串
	if key == "" {
		if t.priority == 0 {
			t.value = val
			t.priority = priority
		}
		return
	}

	// 如果 prefix 不为空，处理包含有 prefix 的节点
	if t.prefix != "" {
		// Need to split the prefix among multiple nodes.
		// 需要在多个节点之间分割前缀，找 prefix 和 key 的最长公共前缀
		var n int // length of the longest common prefix
		for ; n < len(t.prefix) && n < len(key); n++ {
			if t.prefix[n] != key[n] {
				break
			}
		}
		if n == len(t.prefix) { // key 包含 prefix，直接放到 next 中
			t.next.add(key[n:], val, priority, r)
		} else if n == 0 { // key 和 prefix 第一个字节就不同
			// First byte differs, start a new lookup table here. Looking up
			// what is currently t.prefix[0] will lead to prefixNode, and
			// looking up key[0] will lead to keyNode.
			//  第一个字节就不同， 如果 prefix 只有一个字节，直接取 t.next 作为 prefixNode ；否则 prefixNode = &trieNode{ prefix: t.prefix[1:], next: t.next}
			var prefixNode *trieNode
			if len(t.prefix) == 1 { // 如果 prefix 只有一个字节，则直接使用 next
				prefixNode = t.next
			} else { // 如果 prefix 不止 1 个字节，则新建一个节点
				prefixNode = &trieNode{
					prefix: t.prefix[1:],
					next:   t.next,
				}
			}
			// 新建 keyNode 并初始化 , 这里子节点不止一个了，有 prefixNode 和 keyNode ，都需要设置到 table 中
			keyNode := new(trieNode)
			t.table = make([]*trieNode, r.tableSize)
			t.table[r.mapping[t.prefix[0]]] = prefixNode
			t.table[r.mapping[key[0]]] = keyNode
			t.prefix = ""
			t.next = nil
			// 继续处理 key[1:]
			keyNode.add(key[1:], val, priority, r)
		} else { // key 和 prefix 部分相同
			// Insert new node after the common section of the prefix.
			// 在前缀的公共部分之后插入新节点。
			next := &trieNode{
				prefix: t.prefix[n:],
				next:   t.next,
			}
			t.prefix = t.prefix[:n]
			t.next = next
			// 继续处理 key[n:]
			next.add(key[n:], val, priority, r)
		}
	} else if t.table != nil { // 没有 prefix 需要处理，有 table 的时候就插入到 table
		// Insert into existing table.
		// 插入到 table 中
		m := r.mapping[key[0]]
		if t.table[m] == nil {
			t.table[m] = new(trieNode)
		}
		// 继续处理 key[1:]
		t.table[m].add(key[1:], val, priority, r)
	} else { // 没有 prefix 需要处理，也没有 table ，设置 prefix 为 key ，然后 val 加入到 next 中，也就是只有一个子节点
		t.prefix = key
		t.next = new(trieNode)
		t.next.add("", val, priority, r)
	}
}

// 查找
func (r *genericReplacer) lookup(s string, ignoreRoot bool) (val string, keylen int, found bool) {
	// Iterate down the trie to the end, and grab the value and keylen with
	// the highest priority.
	// 遍历 trie 树，并以最高优先级获取 key 和 keylen 。
	bestPriority := 0
	node := &r.root
	n := 0
	for node != nil {
		// 如果有更大的优先级（忽略 root 的话，node 不能是 root ）
		// 只有 key 完整的 node 的 priority 才为正，所以 node.priority > bestPriority 不会找到一半的，一定是完整的 key
		if node.priority > bestPriority && !(ignoreRoot && node == &r.root) {
			bestPriority = node.priority
			val = node.value
			keylen = n
			found = true
		}

		if s == "" { // s 为空了，结束
			break
		}
		if node.table != nil {
			// mapping 映射中没有 s[0] 字符，说明所有的旧值中都没有此字符，结束
			index := r.mapping[s[0]]
			if int(index) == r.tableSize {
				break
			}
			// 当前 node 可能没有上述字符，此时为 s = s[1:] ， node == nil 就会结束的
			node = node.table[index]
			s = s[1:]
			n++
		} else if node.prefix != "" && HasPrefix(s, node.prefix) {
			// 如果只有一个节点，并且 s 是否包含前缀 node.prefix
			n += len(node.prefix)
			s = s[len(node.prefix):]
			node = node.next
		} else {
			// 上述面的都不满足则跳出
			break
		}
	}
	return
}

// 以下是 genericReplacer 实现 replacer

// genericReplacer is the fully generic algorithm.
// It's used as a fallback when nothing faster can be used.
// 通用的一个替换算法
type genericReplacer struct {
	root trieNode //
	// tableSize is the size of a trie node's lookup table. It is the number
	// of unique key bytes.
	tableSize int
	// mapping maps from key bytes to a dense index for trieNode.table.
	mapping [256]byte
}

func makeGenericReplacer(oldnew []string) *genericReplacer {
	r := new(genericReplacer)
	// Find each byte used, then assign them each an index.
	// 找出所有的旧值中的字节，设置 r.mapping[key[j]] = 1
	for i := 0; i < len(oldnew); i += 2 {
		key := oldnew[i]
		for j := 0; j < len(key); j++ {
			r.mapping[key[j]] = 1
		}
	}

	// 统计下一共存在多少中字节
	for _, b := range r.mapping {
		r.tableSize += int(b)
	}

	// 映射下，不存在的设置为 byte(r.tableSize)
	var index byte
	for i, b := range r.mapping {
		if b == 0 {
			r.mapping[i] = byte(r.tableSize)
		} else {
			r.mapping[i] = index
			index++
		}
	}
	// Ensure root node uses a lookup table (for performance).
	// 确保根节点用查表法，以提高性能
	r.root.table = make([]*trieNode, r.tableSize)

	// 将key,val放入字典树，注意 priority=len(oldnew)-i ，即越数组前面的，值越大，优先级越高
	// key 对于旧值， val 对于新值
	for i := 0; i < len(oldnew); i += 2 {
		r.root.add(oldnew[i], oldnew[i+1], len(oldnew)-i, r)
	}
	return r
}

// appendSliceWriter 实现 replacer 写入到 []byte
type appendSliceWriter []byte

// Write writes to the buffer to satisfy io.Writer.
func (w *appendSliceWriter) Write(p []byte) (int, error) {
	*w = append(*w, p...)
	return len(p), nil
}

// WriteString writes to the buffer without string->[]byte->string allocations.
func (w *appendSliceWriter) WriteString(s string) (int, error) {
	*w = append(*w, s...)
	return len(s), nil
}

// appendSliceWriter 实现 replacer 写入到 io.Writer
type stringWriter struct {
	w io.Writer
}

func (w stringWriter) WriteString(s string) (int, error) {
	return w.w.Write([]byte(s))
}

func getStringWriter(w io.Writer) io.StringWriter {
	sw, ok := w.(io.StringWriter)
	if !ok {
		sw = stringWriter{w}
	}
	return sw
}

// genericReplacer 实现 replacer 的 Replace 方法
func (r *genericReplacer) Replace(s string) string {
	buf := make(appendSliceWriter, 0, len(s))
	r.WriteString(&buf, s) // 直接调用 WriteString 来实现
	return string(buf)
}

// genericReplacer 实现 replacer 的 WriteString 方法
func (r *genericReplacer) WriteString(w io.Writer, s string) (n int, err error) {
	sw := getStringWriter(w)
	var last, wn int        // last 为下一个需要写入的索引
	var prevMatchEmpty bool // 上一轮找到的是否为空串
	for i := 0; i <= len(s); {
		// Fast path: s[i] is not a prefix of any pattern.
		// 快速查找，看看是不是没有 s[i] 这个字符，如果没有就直接找下一个
		if i != len(s) && r.root.priority == 0 {
			index := int(r.mapping[s[i]])
			if index == r.tableSize || r.root.table[index] == nil {
				i++
				continue
			}
		}

		// Ignore the empty match iff the previous loop found the empty match.
		// 忽略空的匹配，当且仅当上一轮找到的是空匹配。iff 是当且仅当的意思，不是错别字。
		val, keylen, match := r.lookup(s[i:], prevMatchEmpty)
		prevMatchEmpty = match && keylen == 0
		if match { // 如果匹配到了
			// 写入 s 中不需要替换的字符
			wn, err = sw.WriteString(s[last:i])
			n += wn
			if err != nil {
				return
			}
			// 再写入替换后的字符
			wn, err = sw.WriteString(val)
			n += wn
			if err != nil {
				return
			}
			// 往后步进
			i += keylen
			last = i
			continue
		}
		i++
	}
	// 在遍历之前，可能一直 continue 完了， 这里将尾部的写入
	if last != len(s) {
		wn, err = sw.WriteString(s[last:])
		n += wn
	}
	return
}

// singleStringReplacer is the implementation that's used when there is only
// one string to replace (and that string has more than one byte).
// singleStringReplacer 是仅替换一个字符串（并且该字符串具有多个字节）时使用的实现。
type singleStringReplacer struct {
	finder *stringFinder // finder 使用的是 BM 算法
	// value is the new string that replaces that pattern when it's found.
	value string
}

// makeSingleStringReplacer 创建 singleStringReplacer
func makeSingleStringReplacer(pattern string, value string) *singleStringReplacer {
	return &singleStringReplacer{finder: makeStringFinder(pattern), value: value}
}

// singleStringReplacer 实现 replacer 的 Replace 方法
func (r *singleStringReplacer) Replace(s string) string {
	var buf []byte
	i, matched := 0, false
	for {
		match := r.finder.next(s[i:])
		if match == -1 {
			break
		}
		matched = true
		buf = append(buf, s[i:i+match]...)
		buf = append(buf, r.value...)
		i += match + len(r.finder.pattern)
	}
	if !matched {
		return s
	}
	buf = append(buf, s[i:]...)
	return string(buf)
}

// singleStringReplacer 实现 replacer 的 WriteString 方法
func (r *singleStringReplacer) WriteString(w io.Writer, s string) (n int, err error) {
	sw := getStringWriter(w)
	var i, wn int
	for {
		match := r.finder.next(s[i:])
		if match == -1 {
			break
		}
		wn, err = sw.WriteString(s[i : i+match])
		n += wn
		if err != nil {
			return
		}
		wn, err = sw.WriteString(r.value)
		n += wn
		if err != nil {
			return
		}
		i += match + len(r.finder.pattern)
	}
	wn, err = sw.WriteString(s[i:])
	n += wn
	return
}

// 以下是 byteReplacer 实现 replacer
// 一个字节一个自己的替换，直接遍历完就好了，实现很简单

// byteReplacer is the implementation that's used when all the "old"
// and "new" values are single ASCII bytes.
// The array contains replacement bytes indexed by old byte.
type byteReplacer [256]byte

func (r *byteReplacer) Replace(s string) string {
	var buf []byte // lazily allocated
	for i := 0; i < len(s); i++ {
		b := s[i]
		if r[b] != b {
			if buf == nil {
				buf = []byte(s)
			}
			buf[i] = r[b]
		}
	}
	if buf == nil {
		return s
	}
	return string(buf)
}

func (r *byteReplacer) WriteString(w io.Writer, s string) (n int, err error) {
	// TODO(bradfitz): use io.WriteString with slices of s, avoiding allocation.
	bufsize := 32 << 10
	if len(s) < bufsize {
		bufsize = len(s)
	}
	buf := make([]byte, bufsize)

	for len(s) > 0 {
		ncopy := copy(buf, s)
		s = s[ncopy:]
		for i, b := range buf[:ncopy] {
			buf[i] = r[b]
		}
		wn, err := w.Write(buf[:ncopy])
		n += wn
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// 以下是 byteStringReplacer 实现 replacer

// byteStringReplacer is the implementation that's used when all the
// "old" values are single ASCII bytes but the "new" values vary in size.
// 当所有“旧”值都是单个ASCII字节但“新”值的大小不同时，将使用 byteStringReplacer 实现。
type byteStringReplacer struct {
	// replacements contains replacement byte slices indexed by old byte.
	// A nil []byte means that the old byte should not be replaced.
	// replacements 包含由旧字节索引的替换字符串。 nil 表示不应替换旧字节。
	replacements [256][]byte
	// toReplace keeps a list of bytes to replace. Depending on length of toReplace
	// and length of target string it may be faster to use Count, or a plain loop.
	// We store single byte as a string, because Count takes a string.
	// toReplace 保留要替换的旧字节列表。 根据 toReplace 的长度和目标字符串的长度，使用 strings.Count 或纯循环可能会更快。
	// 我们将单个字节存储为字符串，因为 strings.Count 需要一个字符串。
	toReplace []string
}

// countCutOff controls the ratio of a string length to a number of replacements
// at which (*byteStringReplacer).Replace switches algorithms.
// For strings with higher ration of length to replacements than that value,
// we call Count, for each replacement from toReplace.
// For strings, with a lower ratio we use simple loop, because of Count overhead.
// countCutOff is an empirically determined overhead multiplier.
// TODO(tocarip) revisit once we have register-based abi/mid-stack inlining.
const countCutOff = 8

func (r *byteStringReplacer) Replace(s string) string {
	newSize := len(s)   // 字符串被替换后的长度
	anyChanges := false // 是否需要替换
	// Is it faster to use Count?
	// 如果 len(r.toReplace)*countCutOff <= len(s) 使用 Count ；否则使用纯遍历。下面计算 newSize 和 anyChanges
	if len(r.toReplace)*countCutOff <= len(s) {
		for _, x := range r.toReplace {
			if c := Count(s, x); c != 0 {
				// The -1 is because we are replacing 1 byte with len(replacements[b]) bytes.
				newSize += c * (len(r.replacements[x[0]]) - 1)
				anyChanges = true
			}

		}
	} else {
		for i := 0; i < len(s); i++ {
			b := s[i]
			if r.replacements[b] != nil {
				// See above for explanation of -1
				newSize += len(r.replacements[b]) - 1
				anyChanges = true
			}
		}
	}
	if !anyChanges { // 没有任何改变， 直接返回
		return s
	}
	// 开始替换
	buf := make([]byte, newSize)
	j := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if r.replacements[b] != nil {
			j += copy(buf[j:], r.replacements[b])
		} else {
			buf[j] = b
			j++
		}
	}
	return string(buf)
}

func (r *byteStringReplacer) WriteString(w io.Writer, s string) (n int, err error) {
	sw := getStringWriter(w)
	last := 0 // 上一次处理的字节，累计多个不需要替换的，一次性写入
	for i := 0; i < len(s); i++ {
		b := s[i]
		if r.replacements[b] == nil {
			continue
		}
		// i 是需要被替换的，所以 last != i 的时候，则 s[last,i] 是累计的
		if last != i {
			nw, err := sw.WriteString(s[last:i])
			n += nw
			// 这里写入失败了
			if err != nil {
				return n, err
			}
		}
		last = i + 1 // 重新设置 last
		nw, err := w.Write(r.replacements[b])
		n += nw
		// 这里写入失败了
		if err != nil {
			return n, err
		}
	}
	// 在遍历之前，可能一直 continue 完了， 这里将尾部的写入
	if last != len(s) {
		var nw int
		nw, err = sw.WriteString(s[last:])
		n += nw
	}
	return
}
