// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context defines the Context type, which carries deadlines,
// cancelation signals, and other request-scoped values across API boundaries
// and between processes.
// context 包定义了 Context 类型，它定义了截止时间、取消信号和请求域数据，可以贯穿 API 边界和进程之间 。
//
// Incoming requests to a server should create a Context, and outgoing
// calls to servers should accept a Context. The chain of function
// calls between them must propagate the Context, optionally replacing
// it with a derived Context created using WithCancel, WithDeadline,
// WithTimeout, or WithValue. When a Context is canceled, all
// Contexts derived from it are also canceled.
// 上次请求应该创建一个 Context ，下层请求应该接收一个 Context 。这个函数调用链必须传递 Context ，可以使用 WithCancel, WithDeadline,
// WithTimeout 或 WithValue 来替换。取消 Context 后，从该 Context 派生的所有 Context 也会被取消。
//
// The WithCancel, WithDeadline, and WithTimeout functions take a
// Context (the parent) and return a derived Context (the child) and a
// CancelFunc. Calling the CancelFunc cancels the child and its
// children, removes the parent's reference to the child, and stops
// any associated timers. Failing to call the CancelFunc leaks the
// child and its children until the parent is canceled or the timer
// fires. The go vet tool checks that CancelFuncs are used on all
// control-flow paths.
// WithCancel, WithDeadline 和 WithTimeout 函数接受 Context （父代）并返回派生的 Context （子代）和 CancelFunc 。调用 CancelFunc 会
// 取消该子代及其子代，删除父代对该子代的引用，并停止所有关联的计时器。未能调用 CancelFunc 会使子代及其子代泄漏，直到父代被取消或计时器触发。
// go vet 工具检查所有控制流上是否都使用了 CancelFuncs 。
//
// Programs that use Contexts should follow these rules to keep interfaces
// consistent across packages and enable static analysis tools to check context
// propagation:
// 使用 Contexts 的程序应遵循以下规则，以使各个包之间的接口保持一致，并启用静态分析工具来检查 Contexts 传播：
//
// Do not store Contexts inside a struct type; instead, pass a Context
// explicitly to each function that needs it. The Context should be the first
// parameter, typically named ctx:
// 不要将 Contexts 存储在结构类型中；而是将上下文明确传递给需要它的每个函数。 Context 应该是第一个参数，通常命名为 ctx ：
//
// 	func DoSomething(ctx context.Context, arg Arg) error {
// 		// ... use ctx ...
// 	}
//
// Do not pass a nil Context, even if a function permits it. Pass context.TODO
// if you are unsure about which Context to use.
// 即使函数允许，也不要传递 nil Context 。 如果不确定使用哪个上下文，请传递 context.TODO 。
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
// 仅将 context 值用于传递过程和 API 的请求范围的数据，而不用于将可选参数传递给函数。
//
// The same Context may be passed to functions running in different goroutines;
// Contexts are safe for simultaneous use by multiple goroutines.
// 可以将相同的 Context 传递给在不同 goroutine 中运行的函数。 Context 对于由多个goroutine同时使用是安全的。
//
// See https://blog.golang.org/context for example code for a server that uses
// Contexts.
package context

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// A Context carries a deadline, a cancelation signal, and other values across
// API boundaries.
//
// Context's methods may be called by multiple goroutines simultaneously.
// Context 定义了截止时间、取消信号和请求域数据，可以贯穿 API 边界和进程之间 。 Context 的函数可以同时被多个goroutine调用。
type Context interface {
	// Deadline returns the time when work done on behalf of this context
	// should be canceled. Deadline returns ok==false when no deadline is
	// set. Successive calls to Deadline return the same results.
	// Deadline 返回工作的截止时间。如果未设置 deadline ，则截止日期返回 ok == false。 连续调用 Deadline 会返回相同的结果。
	Deadline() (deadline time.Time, ok bool)

	// Done returns a channel that's closed when work done on behalf of this
	// context should be canceled. Done may return nil if this context can
	// never be canceled. Successive calls to Done return the same value.
	// Done 返回一个 channel ，，这个 channel 会在当前工作完成或者 context 被取消之后关闭，多次调用 Done 方法会返回同一个 Channel；
	//
	// WithCancel arranges for Done to be closed when cancel is called;
	// WithDeadline arranges for Done to be closed when the deadline
	// expires; WithTimeout arranges for Done to be closed when the timeout
	// elapses.
	//
	// Done is provided for use in select statements:
	//
	//  // Stream generates values with DoSomething and sends them to out
	//  // until DoSomething returns an error or ctx.Done is closed.
	//  func Stream(ctx context.Context, out chan<- Value) error {
	//  	for {
	//  		v, err := DoSomething(ctx)
	//  		if err != nil {
	//  			return err
	//  		}
	//  		select {
	//  		case <-ctx.Done():
	//  			return ctx.Err()
	//  		case out <- v:
	//  		}
	//  	}
	//  }
	//
	// See https://blog.golang.org/pipelines for more examples of how to use
	// a Done channel for cancelation.
	Done() <-chan struct{}

	// If Done is not yet closed, Err returns nil.
	// If Done is closed, Err returns a non-nil error explaining why:
	// Canceled if the context was canceled
	// or DeadlineExceeded if the context's deadline passed.
	// After Err returns a non-nil error, successive calls to Err return the same error.
	// 如果 Done 还没有关闭， Err 返回 nil
	// 如果 Done 关闭了， Err 返回一个非空的错误表明原因：
	// Canceled : 如果 context 被取消了
	// DeadlineExceeded : 如果过了截至时间
	// Err 返回非空值后，对 Err 的连续调用将返回相同的错误。
	Err() error

	// Value returns the value associated with this context for key, or nil
	// if no value is associated with key. Successive calls to Value with
	// the same key returns the same result.
	// Value 会从 Context 中返回键对应的值，如果没有值与之关联，则返回 nil 。多次使用相同的 key 调用 Value 会返回相同的结果。
	//
	// Use context values only for request-scoped data that transits
	// processes and API boundaries, not for passing optional parameters to
	// functions.
	// 仅将 context 值用于传递过程和 API 的请求范围的数据，而不用于将可选参数传递给函数。
	//
	// A key identifies a specific value in a Context. Functions that wish
	// to store values in Context typically allocate a key in a global
	// variable then use that key as the argument to context.WithValue and
	// Context.Value. A key can be any type that supports equality;
	// packages should define keys as an unexported type to avoid
	// collisions.
	// key 标识上下文中的特定值。 希望在 Context 中存储值的函数通常会在全局变量中分配一个 key ，然后将该 key 用作 context.WithValue 和 Context.Value 的参数。
	// key 可以是任何支持等于比较的类型。 包应将 key 定义为未导出的类型，以避免冲突。
	//
	// Packages that define a Context key should provide type-safe accessors
	// for the values stored using that key:
	// 定义 Context key 的包应为使用该 key 存储的值提供类型安全的访问器：(下面是一个例子)
	//
	// 	// Package user defines a User type that's stored in Contexts.
	// 	package user
	//
	// 	import "context"
	//
	// 	// User is the type of value stored in the Contexts.
	// 	type User struct {...}
	//
	// 	// key is an unexported type for keys defined in this package.
	// 	// This prevents collisions with keys defined in other packages.
	// 	type key int
	//
	// 	// userKey is the key for user.User values in Contexts. It is
	// 	// unexported; clients use user.NewContext and user.FromContext
	// 	// instead of using this key directly.
	// 	var userKey key
	//
	// 	// NewContext returns a new Context that carries value u.
	// 	func NewContext(ctx context.Context, u *User) context.Context {
	// 		return context.WithValue(ctx, userKey, u)
	// 	}
	//
	// 	// FromContext returns the User value stored in ctx, if any.
	// 	func FromContext(ctx context.Context) (*User, bool) {
	// 		u, ok := ctx.Value(userKey).(*User)
	// 		return u, ok
	// 	}
	Value(key interface{}) interface{}
}

// Canceled is the error returned by Context.Err when the context is canceled.
// Canceled 定义取消时候返回的错误
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by Context.Err when the context's
// deadline passes.
// DeadlineExceeded 定义过期后返回的错误
var DeadlineExceeded error = deadlineExceededError{}

// deadlineExceededError 实现了 error 接口，并实现了 Timeout Temporary 函数
type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }

// An emptyCtx is never canceled, has no values, and has no deadline. It is not
// struct{}, since vars of this type must have distinct addresses.
// emptyCtx 是空的 Context ，返回的都是空值，是一个不可取消，没有设置截止时间，没有携带任何值的 Context 。他不是 struct{} ，因为此类型的变量必须有不同的地址。
type emptyCtx int

func (*emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (*emptyCtx) Done() <-chan struct{} {
	return nil
}

func (*emptyCtx) Err() error {
	return nil
}

func (*emptyCtx) Value(key interface{}) interface{} {
	return nil
}

func (e *emptyCtx) String() string {
	switch e {
	case background:
		return "context.Background"
	case todo:
		return "context.TODO"
	}
	return "unknown empty Context"
}

// Context 虽然是个接口，但是并不需要使用方实现，golang 内置的 context 包，已经帮我们实现了2个方法，一般在代码中，开始上下文的时候都是以这两个作为最顶层的 parent context，
// 然后再衍生出子 context 。这些 Context 对象形成一棵树：当一个 Context 对象被取消时，继承自它的所有 Context 都会被取消。
var (
	background = new(emptyCtx)
	todo       = new(emptyCtx)
)

// Background returns a non-nil, empty Context. It is never canceled, has no
// values, and has no deadline. It is typically used by the main function,
// initialization, and tests, and as the top-level Context for incoming
// requests.
// Background 函数返回空 Context，并且不可以取消，没有最后期限，没有共享数据。Background 仅仅会被用在 main、init 或 tests 函数中。
func Background() Context {
	return background
}

// TODO returns a non-nil, empty Context. Code should use context.TODO when
// it's unclear which Context to use or it is not yet available (because the
// surrounding function has not yet been extended to accept a Context
// parameter).
// TODO 返回一个非空的 Context 。当不清楚要使用哪个上下文或尚不可用时（因为周围的函数尚未扩展为接受Context参数），应使用 context.TODO 。
func TODO() Context {
	return todo
}

// A CancelFunc tells an operation to abandon its work.
// A CancelFunc does not wait for the work to stop.
// After the first call, subsequent calls to a CancelFunc do nothing.
// CancelFunc 通知放弃工作。不等待工作结束。第一次调用后再调用不做任何事。
type CancelFunc func()

// WithCancel returns a copy of parent with a new Done channel. The returned
// context's Done channel is closed when the returned cancel function is called
// or when the parent context's Done channel is closed, whichever happens first.
// WithCancel 返回具有新 Done channel 的父 Context 的副本。当调用返回的 cancel 函数或关闭父 Context 的 Done 通道时（以先发生的为准），关闭返回的 Context 的 Done 通道。
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
// 取消此 Context 将释放与其关联的资源，因此在此 Context 中运行的操作完成后，代码应立即调用 cancel 。
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	// 返回实现 Context 的 cancelCtx
	c := newCancelCtx(parent)
	// 当父 Context 取消的时候取消子 Context 。
	propagateCancel(parent, &c)
	return &c, func() { c.cancel(true, Canceled) }
}

// newCancelCtx returns an initialized cancelCtx.
// newCancelCtx 返回已经初始化的 cancelCtx
func newCancelCtx(parent Context) cancelCtx {
	return cancelCtx{Context: parent}
}

// propagateCancel arranges for child to be canceled when parent is.
// propagateCancel 当父 Context 取消的时候取消子 Context 。
func propagateCancel(parent Context, child canceler) {
	if parent.Done() == nil {
		// 父 Context 永远不会取消
		return // parent is never canceled
	}
	if p, ok := parentCancelCtx(parent); ok {
		// 父 Context 支持取消
		p.mu.Lock()
		if p.err != nil {
			// parent has already been canceled
			// 父 Context 已经取消了，则取消子 Context
			child.cancel(false, p.err)
		} else {
			// 否则添加到父 Context children 中，当以后父 Context 取消时会取消 children 中所有的 Context
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			p.children[child] = struct{}{}
		}
		p.mu.Unlock()
	} else {
		// 父 Context 不支持取消，这里启动一个 G ，当父 Context 取消的时候取消子 Context
		go func() {
			select {
			case <-parent.Done():
				child.cancel(false, parent.Err())
			case <-child.Done():
			}
		}()
	}
}

// parentCancelCtx follows a chain of parent references until it finds a
// *cancelCtx. This function understands how each of the concrete types in this
// package represents its parent.
// parentCancelCtx 找父 Context ，直到找到 *cancelCtx
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	for {
		switch c := parent.(type) {
		case *cancelCtx: // *cancelCtx 类型本身就是
			return c, true
		case *timerCtx: // *timerCtx 类型其成员中包含 *cancelCtx
			return &c.cancelCtx, true
		case *valueCtx: // *valueCtx 类型，则继续找其父 Context
			parent = c.Context
		default: // 其他的情况没有 cancelCtx
			return nil, false
		}
	}
}

// removeChild removes a context from its parent.
// removeChild 从其父 Context 中移除
func removeChild(parent Context, child canceler) {
	p, ok := parentCancelCtx(parent)
	if !ok {
		// 如果父 Context 不支持取消，则不会再其 children 中，直接返回
		return
	}
	// 这里可能需要从其 children 中删除
	p.mu.Lock()
	if p.children != nil {
		delete(p.children, child)
	}
	p.mu.Unlock()
}

// A canceler is a context type that can be canceled directly. The
// implementations are *cancelCtx and *timerCtx.
// canceler 是能够被直接取消的 Context 。实现此接口的有 *cancelCtx 和 *timerCtx 。
type canceler interface {
	cancel(removeFromParent bool, err error)
	Done() <-chan struct{}
}

// closedchan is a reusable closed channel.
// closedchan 是复用的关闭的 channel
var closedchan = make(chan struct{})

func init() {
	// 初始化时候就关闭 closedchan
	close(closedchan)
}

// A cancelCtx can be canceled. When canceled, it also cancels any children
// that implement canceler.
// cancelCtx 可以被取消 的 Content 。 取消后，它也会取消所有实现 canceler 的子级。
type cancelCtx struct {
	Context

	mu       sync.Mutex            // protects following fields
	done     chan struct{}         // created lazily, closed by first cancel call // 延迟创建，第一个 cancel 调用的时候关闭
	children map[canceler]struct{} // set to nil by the first cancel call // 记录实现了 cancel 函数的子 Context ，第一个 cancel 函数调用的时候将取消子 Context ，然后将此字段设置为 nil
	err      error                 // set to non-nil by the first cancel call // 第一个 cancel 函数调用的时候设置这个值
}

// 返回 channel
func (c *cancelCtx) Done() <-chan struct{} {
	c.mu.Lock()
	if c.done == nil {
		// 延迟创建 done
		c.done = make(chan struct{})
	}
	d := c.done
	c.mu.Unlock()
	return d
}

// 返回 err
func (c *cancelCtx) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

func (c *cancelCtx) String() string {
	return fmt.Sprintf("%v.WithCancel", c.Context)
}

// cancel closes c.done, cancels each of c's children, and, if
// removeFromParent is true, removes c from its parent's children.
// cancel 关闭 c.done ，取消 c 的每个子 Context ，如果 removeFromParent 为 true，则从其父 Context 的 children 中删除 c 。
func (c *cancelCtx) cancel(removeFromParent bool, err error) {
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		// c 已经取消过了
		return // already canceled
	}
	// 第一次 cancel 调用的时候，设置 err ，后续的调用上面已经返回了
	c.err = err
	if c.done == nil {
		// 如果没有设置 done ，则将其设置为已经取消了的 closedchan ，复用下
		c.done = closedchan
	} else {
		close(c.done)
	}
	// 取消子 Context
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err)
	}
	c.children = nil
	c.mu.Unlock()

	// 如果 removeFromParent == true ，则从父 Context 中删除 c
	if removeFromParent {
		removeChild(c.Context, c)
	}
}

// WithDeadline returns a copy of the parent context with the deadline adjusted
// to be no later than d. If the parent's deadline is already earlier than d,
// WithDeadline(parent, d) is semantically equivalent to parent. The returned
// context's Done channel is closed when the deadline expires, when the returned
// cancel function is called, or when the parent context's Done channel is
// closed, whichever happens first.
// WithDeadline 返回父 Context 的副本，并将截止时间调整为不迟于 d 。如果父 Context 的截止日期早于 d ，则 WithDeadline(parent，d) 在语义上等效于父级。
// 当截止时间到期，调用返回的 cancel 函数或关闭父 Context 的 Done channel 时（以先发生者为准），将关闭返回的 Context 的 Done channel。
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
// 取消此 Context 将释放与其关联的资源，因此在此 Context 中运行的操作完成后，代码应立即调用 cancel 。
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	// 父 Context 早于截至时间 d ，则父 Context 取消的时候，取消此 Context
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		// The current deadline is already sooner than the new one.
		return WithCancel(parent)
	}
	// 新建 timerCtx
	c := &timerCtx{
		cancelCtx: newCancelCtx(parent),
		deadline:  d,
	}
	// 当父 Context 取消的时候取消子 Context 。
	propagateCancel(parent, c)
	dur := time.Until(d)
	// 如果已经过了截止时间， 则调用 cancel 函数，并返回 c 和 取消函数
	if dur <= 0 {
		c.cancel(true, DeadlineExceeded) // deadline has already passed
		return c, func() { c.cancel(false, Canceled) }
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// c.err == nil 表示白没有取消过，则启动定时器，当截至时间到了后，调用 cancel 函数
	if c.err == nil {
		c.timer = time.AfterFunc(dur, func() {
			c.cancel(true, DeadlineExceeded)
		})
	}
	// 并返回 c 和 取消函数
	return c, func() { c.cancel(true, Canceled) }
}

// A timerCtx carries a timer and a deadline. It embeds a cancelCtx to
// implement Done and Err. It implements cancel by stopping its timer then
// delegating to cancelCtx.cancel.
// timerCtx 带有计时器和截至时间。 它嵌入了 cancelCtx 以实现 Done 和 Err 。 它通过停止计时器然后委派到 cancelCtx.cancel 来实现取消。
type timerCtx struct {
	cancelCtx
	timer *time.Timer // Under cancelCtx.mu. // 在 cancelCtx.mu 锁的保护下

	deadline time.Time
}

// Deadline 返回截止时间
func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

func (c *timerCtx) String() string {
	return fmt.Sprintf("%v.WithDeadline(%s [%s])", c.cancelCtx.Context, c.deadline, time.Until(c.deadline))
}

// cancel 是 timerCtx 的取消函数
func (c *timerCtx) cancel(removeFromParent bool, err error) {
	// 直接调用 cancelCtx 的取消函数，这一步不会从父 Context 中删除
	c.cancelCtx.cancel(false, err)
	// 是否从父 Context 中删除
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}
	// 关闭 timer 并清空
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

// WithTimeout returns WithDeadline(parent, time.Now().Add(timeout)).
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete:
//
// 	func slowOperationWithTimeout(ctx context.Context) (Result, error) {
// 		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
// 		defer cancel()  // releases resources if slowOperation completes before timeout elapses
// 		return slowOperation(ctx)
// 	}
// WithTimeout 调用 WithDeadline 来实现
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}

// WithValue returns a copy of parent in which the value associated with key is
// val.
// WithValue 返回父 Context 的副本，其中与 key 关联的值为 val 。
//
// Use context Values only for request-scoped data that transits processes and
// WithValue 返回父 Context 的副本，其中与 key 关联的值为 val 。
// APIs, not for passing optional parameters to functions.
//
// The provided key must be comparable and should not be of type
// string or any other built-in type to avoid collisions between
// packages using context. Users of WithValue should define their own
// types for keys. To avoid allocating when assigning to an
// interface{}, context keys often have concrete type
// struct{}. Alternatively, exported context key variables' static
// type should be a pointer or interface.
// 提供的 key 必须是可比较的，并且不能为字符串类型或任何其他内置类型，以避免使用 Context 在程序包之间发生冲突。
// WithValue 的用户应定义自己的 key 类型。 为了避免在分配给 interface{} 时进行分配， Context 的 key 通常具有具体的类型 struct{} 。
// 另外，导出的 Context 的 key 变量的静态类型应该是指针或接口。
func WithValue(parent Context, key, val interface{}) Context {
	if key == nil {
		panic("nil key")
	}
	// key 必是须可比较类型
	if !reflect.TypeOf(key).Comparable() {
		panic("key is not comparable")
	}
	// 返回 valueCtx
	return &valueCtx{parent, key, val}
}

// A valueCtx carries a key-value pair. It implements Value for that key and
// delegates all other calls to the embedded Context.
// valueCtx 带有一个键值对。 它为该键实现Value，并将所有其他调用委派给嵌入式Context。
type valueCtx struct {
	Context
	key, val interface{}
}

func (c *valueCtx) String() string {
	return fmt.Sprintf("%v.WithValue(%#v, %#v)", c.Context, c.key, c.val)
}

// Value 是 valueCtx 对接口 Context Value 函数的实现 ，如果自己有 key 则返回，否则级联父 Context
func (c *valueCtx) Value(key interface{}) interface{} {
	if c.key == key {
		return c.val
	}
	return c.Context.Value(key)
}
