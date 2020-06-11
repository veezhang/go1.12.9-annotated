// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package singleflight provides a duplicate function call suppression
// mechanism.
// 包 singleflight 提供了重复的函数调用抑制机制。单航模式
package singleflight

import "sync"

// call is an in-flight or completed singleflight.Do call
// call 是 singleflight.Do 的调用（在调用中或已经完成）
type call struct {
	wg sync.WaitGroup

	// These fields are written once before the WaitGroup is done
	// and are only read after the WaitGroup is done.
	// 以下字段在 WaitGroup 完成前写入一次；只能在 WaitGroup 完成后读取。
	val interface{} // 值
	err error       // 错误

	// These fields are read and written with the singleflight
	// mutex held before the WaitGroup is done, and are read but
	// not written after the WaitGroup is done.
	// 以下字段是在 WaitGroup 完成前，在持有 mutex 下进行读写；在 WaitGroup 完成后读取，但是不能写。
	dups  int             // 副本
	chans []chan<- Result // 函数结果，待通知返回值的 channel
}

// Group represents a class of work and forms a namespace in
// which units of work can be executed with duplicate suppression.
// Group 代表一类工作，并形成一个命名空间，在该命名空间中可以重复执行工作单元。
type Group struct {
	mu sync.Mutex       // protects m				// 保护 m 的锁
	m  map[string]*call // lazily initialized		// 延迟初始化，保存调用的 map
}

// Result holds the results of Do, so they can be passed
// on a channel.
// Result 保存 singleflight.Do 的结果，因此可以在通道上传递它们。
type Result struct {
	Val    interface{} // 值
	Err    error       // 错误
	Shared bool        // 返回的 Result 是否共享
}

// Do executes and returns the results of the given function, making
// sure that only one execution is in-flight for a given key at a
// time. If a duplicate comes in, the duplicate caller waits for the
// original to complete and receives the same results.
// The return value shared indicates whether v was given to multiple callers.
// Do 执行并返回给定函数 fn 的结果，确保一次仅对给定 key 进行一次执行。
// 如果出现重复，则重复的调用者将等待原始呼叫完成并收到相同的结果。
// 返回值中的 shared 表示是否将 v 分配给多个调用方。
// 阻塞调用
func (g *Group) Do(key string, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		// 延迟初始化
		g.m = make(map[string]*call)
	}
	// 已经有此 key 的调用了
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		// 等待调用返回，后返回结果
		c.wg.Wait()
		return c.val, c.err, true
	}
	// 第一次执行 key 的调用
	c := new(call)
	c.wg.Add(1) // wg 增加 1
	g.m[key] = c
	g.mu.Unlock()

	// 执行函数 fn ，
	g.doCall(c, key, fn)
	return c.val, c.err, c.dups > 0
}

// DoChan is like Do but returns a channel that will receive the
// results when they are ready. The second result is true if the function
// will eventually be called, false if it will not (because there is
// a pending request with this key).
// DoChan 和 Do 类似，但是返回一个 channel 来接收返回值。如果 fn 函数最终会调用，则返回 true ，否则返回 false 。
// 非阻塞调用， 需要自己来处理 channel
func (g *Group) DoChan(key string, fn func() (interface{}, error)) (<-chan Result, bool) {
	ch := make(chan Result, 1)
	g.mu.Lock()
	if g.m == nil {
		// 延迟初始化
		g.m = make(map[string]*call)
	}
	// 已经有此 key 的调用了
	if c, ok := g.m[key]; ok {
		c.dups++
		// 将 ch 添加到待通知的 channel 上
		c.chans = append(c.chans, ch)
		g.mu.Unlock()
		// 这里不会调用 c.wg.Wait() ，而是返回 ch ，不会阻塞此函数
		return ch, false
	}
	// 第一次执行 key 的调用
	c := &call{chans: []chan<- Result{ch}}
	c.wg.Add(1) // wg 增加 1
	g.m[key] = c
	g.mu.Unlock()

	// 这里开启 g 来执行，不阻塞此函数
	go g.doCall(c, key, fn)

	return ch, true
}

// doCall handles the single call for a key.
// doCall 执行 key 的调用
func (g *Group) doCall(c *call, key string, fn func() (interface{}, error)) {
	// 执行 fn
	c.val, c.err = fn()
	// 返回后， 执行 Done 唤醒等待的 G
	c.wg.Done()

	// 然后清理
	g.mu.Lock()
	// 删除 key
	delete(g.m, key)
	// 并将结果通知给所有等待中的 channel
	for _, ch := range c.chans {
		ch <- Result{c.val, c.err, c.dups > 0}
	}
	g.mu.Unlock()
}

// ForgetUnshared tells the singleflight to forget about a key if it is not
// shared with any other goroutines. Future calls to Do for a forgotten key
// will call the function rather than waiting for an earlier call to complete.
// Returns whether the key was forgotten or unknown--that is, whether no
// other goroutines are waiting for the result.
// ForgetUnshared 遗忘没有多个 goroutines 执行此 key 的调用的 key 。将来调用已经遗忘了的 key 是会调用传递的函数，而不是等待之前调用的返回。
// 返回是否 key 被遗忘了， 也就是：如果没有其它的 goroutine 等待返回结果，返回 true ， 否则返回 false 。
func (g *Group) ForgetUnshared(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.m[key]
	if !ok {
		// 如果没有此 key ，返回 true
		return true
	}
	if c.dups == 0 {
		// 如果此 key 没有多个 goroutines 共享，则删除它并返回 true
		delete(g.m, key)
		return true
	}
	// 返回 false
	return false
}
