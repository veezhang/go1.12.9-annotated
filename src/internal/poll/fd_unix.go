// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix darwin dragonfly freebsd js,wasm linux nacl netbsd openbsd solaris

package poll

import (
	"io"
	"runtime"
	"sync/atomic"
	"syscall"
)

// FD is a file descriptor. The net and os packages use this type as a
// field of a larger type representing a network connection or OS file.
// FD 是文件描述符。 net 和 os 包中大量的类型将此 FD 用作表示网络连接或 OS 文件的字段。
type FD struct {
	// Lock sysfd and serialize access to Read and Write methods.
	// 锁定 sysfd 并序列化对 Read 和 Write 函数的访问。
	fdmu fdMutex

	// System file descriptor. Immutable until Close.
	Sysfd int // 系统文件描述符

	// I/O poller.
	pd pollDesc // linux 对于的为 epoll fd

	// Writev cache.
	iovecs *[]syscall.Iovec // writev 系统调用的缓存

	// Semaphore signaled when file is closed.
	csema uint32 // 关闭的信号量

	// Non-zero if this file has been set to blocking mode.
	isBlocking uint32 // 是否是阻塞的

	// Whether this is a streaming descriptor, as opposed to a
	// packet-based descriptor like a UDP socket. Immutable.
	IsStream bool // 是否是数据流

	// Whether a zero byte read indicates EOF. This is false for a
	// message based socket connection.
	ZeroReadIsEOF bool // 读取到 0 是否表示是 EOF

	// Whether this is a file rather than a network socket.
	isFile bool // 是否是文件
}

// Init initializes the FD. The Sysfd field should already be set.
// This can be called multiple times on a single FD.
// The net argument is a network name from the net package (e.g., "tcp"),
// or "file".
// Set pollable to true if fd should be managed by runtime netpoll.
// Init 初始化 FD 。Sysfd 字段应该已经设置。 可以在单个 FD 上多次调用此命令。 net 参数是网络包中的网络名称（例如 "tcp"）或 "file"。
// 如果 pollable 为 true ，则将 fd 交给运行时 netpoll 管理。
func (fd *FD) Init(net string, pollable bool) error {
	// We don't actually care about the various network types.
	// 我们实际上并不关心各种网络类型。
	if net == "file" {
		fd.isFile = true
	}
	// 如果 pollable == false ，不需要放到 netpoll 中，直接返回
	if !pollable {
		fd.isBlocking = 1
		return nil
	}
	// init 初始化 pollDesc ， 这里会初始化 netpoll（只执行一次） ，然后加入到 netpoll
	err := fd.pd.init(fd)
	if err != nil {
		// If we could not initialize the runtime poller,
		// assume we are using blocking mode.
		// 如果我们无法初始化运行时轮询器，请假设我们正在使用阻止模式。
		fd.isBlocking = 1
	}
	return err
}

// Destroy closes the file descriptor. This is called when there are
// no remaining references.
// destroy 将关闭文件描述符。 没有引用时候调用此方法。
func (fd *FD) destroy() error {
	// Poller may want to unregister fd in readiness notification mechanism,
	// so this must be executed before CloseFunc.
	fd.pd.close()
	err := CloseFunc(fd.Sysfd)
	fd.Sysfd = -1
	runtime_Semrelease(&fd.csema)
	return err
}

// Close closes the FD. The underlying file descriptor is closed by the
// destroy method when there are no remaining references.
// Close 关闭 FD , 没有引用时候调用 destroy 函数关闭文件描述符
func (fd *FD) Close() error {
	if !fd.fdmu.increfAndClose() {
		return errClosing(fd.isFile)
	}

	// Unblock any I/O.  Once it all unblocks and returns,
	// so that it cannot be referring to fd.sysfd anymore,
	// the final decref will close fd.sysfd. This should happen
	// fairly quickly, since all the I/O is non-blocking, and any
	// attempts to block in the pollDesc will return errClosing(fd.isFile).
	// 调用 runtime_pollUnblock 函数取消阻塞在 pd 中的读写 goroutine 。
	// 当这些 goroutine 取消阻塞后，就不会再引用 fd.sysfd ，则 decref 将关闭 fd.sysfd 。
	fd.pd.evict()

	// The call to decref will call destroy if there are no other
	// references.
	// 如果没有引用，则调用 destroy
	err := fd.decref()

	// Wait until the descriptor is closed. If this was the only
	// reference, it is already closed. Only wait if the file has
	// not been set to blocking mode, as otherwise any current I/O
	// may be blocking, and that would block the Close.
	// No need for an atomic read of isBlocking, increfAndClose means
	// we have exclusive access to fd.
	// 如果是非阻塞的，等待 csema 信号量， destroy 中会释放
	if fd.isBlocking == 0 {
		runtime_Semacquire(&fd.csema)
	}

	return err
}

// Shutdown wraps the shutdown network call.
// Shutdown shutdown 系统调用
func (fd *FD) Shutdown(how int) error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return syscall.Shutdown(fd.Sysfd, how)
}

// SetBlocking puts the file into blocking mode.
// SetBlocking 设置为阻塞模式
func (fd *FD) SetBlocking() error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	// Atomic store so that concurrent calls to SetBlocking
	// do not cause a race condition. isBlocking only ever goes
	// from 0 to 1 so there is no real race here.
	atomic.StoreUint32(&fd.isBlocking, 1)
	return syscall.SetNonblock(fd.Sysfd, false)
}

// Darwin and FreeBSD can't read or write 2GB+ files at a time,
// even on 64-bit systems.
// The same is true of socket implementations on many systems.
// See golang.org/issue/7812 and golang.org/issue/16266.
// Use 1GB instead of, say, 2GB-1, to keep subsequent reads aligned.
// 即使在64位系统上，Darwin和FreeBSD也无法一次读取或写入2GB以上的文件。
// 使用 1G 而不是 2GB-1, 来保持后续读取对齐。
const maxRW = 1 << 30

// Read implements io.Reader.
// Read 实现 io.Reader ，读取数据
func (fd *FD) Read(p []byte) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err
	}
	defer fd.readUnlock()
	// 读取 0 字节 ，立即放回
	if len(p) == 0 {
		// If the caller wanted a zero byte read, return immediately
		// without trying (but after acquiring the readLock).
		// Otherwise syscall.Read returns 0, nil which looks like
		// io.EOF.
		// TODO(bradfitz): make it wait for readability? (Issue 15735)
		return 0, nil
	}
	// 准备读取
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, err
	}
	// 数据流 ，最多读取 1G
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW]
	}
	// 循环读取，最多读到 len(p) 个字节 ，或者出错(超时/关闭)
	for {
		// 读取，最终调用 read 系统调用
		n, err := syscall.Read(fd.Sysfd, p)
		if err != nil { // 出错了
			n = 0
			// EAGAIN ，等待读就绪，然后继续
			if err == syscall.EAGAIN && fd.pd.pollable() {
				// 如果没有数据，就会在 waitRead 函数中 park 当前的 goroutine ，直到可读
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}

			// On MacOS we can see EINTR here if the user
			// pressed ^Z.  See issue #22838.
			// 在 MacOS 上，当用户按 ^Z 时会返回 EINTR ，这种情况也继续
			if runtime.GOOS == "darwin" && err == syscall.EINTR {
				continue
			}
		}
		err = fd.eofError(n, err)
		return n, err
	}
}

// Pread wraps the pread system call.
// Pread 包装 pread 系统调用，用于带偏移量地原子的从文件中读取数据，执行后，文件偏移指针不变。
func (fd *FD) Pread(p []byte, off int64) (int, error) {
	// Call incref, not readLock, because since pread specifies the
	// offset it is independent from other reads.
	// Similarly, using the poller doesn't make sense for pread.
	// 调用 incref 而不是 readLock ，因为 pread 指定偏移量，所以它独立于其他读取。同样，使用轮询器对于 pread 没有意义。
	if err := fd.incref(); err != nil {
		return 0, err
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW]
	}
	n, err := syscall.Pread(fd.Sysfd, p, off)
	if err != nil {
		n = 0
	}
	fd.decref()
	err = fd.eofError(n, err)
	return n, err
}

// ReadFrom wraps the recvfrom network call.
// ReadFrom 包装 recvfrom 网络调用
func (fd *FD) ReadFrom(p []byte) (int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil {
		return 0, nil, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, nil, err
	}
	for {
		n, sa, err := syscall.Recvfrom(fd.Sysfd, p, 0)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, sa, err
	}
}

// ReadMsg wraps the recvmsg network call.
// ReadFrom 包装 recvmsg 网络调用
func (fd *FD) ReadMsg(p []byte, oob []byte) (int, int, int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, nil, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, 0, 0, nil, err
	}
	for {
		n, oobn, flags, sa, err := syscall.Recvmsg(fd.Sysfd, p, oob, 0)
		if err != nil {
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, oobn, flags, sa, err
	}
}

// Write implements io.Writer.
// Write 实现 io.Writer ，读取数据
func (fd *FD) Write(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	// 准备写入
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err
	}
	var nn int // nn 为已经写入多少字节
	for {
		max := len(p) // max 为需要写入多少字节， max-nn 则为剩余需要写入多少字节，这个值不能超过 1G (maxRW)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW
		}
		// 读取，最终调用 write 系统调用
		n, err := syscall.Write(fd.Sysfd, p[nn:max])
		if n > 0 {
			nn += n
		}
		// 读取完成
		if nn == len(p) {
			return nn, err
		}
		// EAGAIN ，等待写就绪，然后继续
		if err == syscall.EAGAIN && fd.pd.pollable() {
			// 如果不可写，就会在 waitWrite 函数中 park 当前的 goroutine ，直到可写
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		// 出错了
		if err != nil {
			return nn, err
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF
		}
	}
}

// Pwrite wraps the pwrite system call.
// Pwrite 包装 pwrite 系统调用，用于带偏移量地原子的从文件中读取数据，执行后，文件偏移指针不变。多个线程同时写,可能存在覆盖问题。
func (fd *FD) Pwrite(p []byte, off int64) (int, error) {
	// Call incref, not writeLock, because since pwrite specifies the
	// offset it is independent from other writes.
	// Similarly, using the poller doesn't make sense for pwrite.
	// 调用 incref 而不是 writeLock ，因为 pread 指定偏移量，所以它独立于其他读取。同样，使用轮询器对于 pread 没有意义。
	// 以下实现跟 Write 基本一致
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	var nn int
	for {
		max := len(p)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW
		}
		n, err := syscall.Pwrite(fd.Sysfd, p[nn:max], off+int64(nn))
		if n > 0 {
			nn += n
		}
		if nn == len(p) {
			return nn, err
		}
		if err != nil {
			return nn, err
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF
		}
	}
}

// WriteTo wraps the sendto network call.
// WriteTo 包装 sendto 网络调用
func (fd *FD) WriteTo(p []byte, sa syscall.Sockaddr) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err
	}
	for {
		err := syscall.Sendto(fd.Sysfd, p, 0, sa)
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
}

// WriteMsg wraps the sendmsg network call.
// WriteMsg 包装 sendmsg 网络调用
func (fd *FD) WriteMsg(p []byte, oob []byte, sa syscall.Sockaddr) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, 0, err
	}
	for {
		n, err := syscall.SendmsgN(fd.Sysfd, p, oob, sa, 0)
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return n, 0, err
		}
		return n, len(oob), err
	}
}

// Accept wraps the accept network call.
// ReadFrom 包装 accept 网络调用
func (fd *FD) Accept() (int, syscall.Sockaddr, string, error) {
	if err := fd.readLock(); err != nil {
		return -1, nil, "", err
	}
	defer fd.readUnlock()

	// 准备读取
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return -1, nil, "", err
	}
	// 循环 accept
	for {
		// accept: 包装 accept 系统调用，返回的文件描述符，已经是 SOCK_NONBLOCK 和 SOCK_CLOEXEC
		s, rsa, errcall, err := accept(fd.Sysfd)
		if err == nil {
			return s, rsa, "", err
		}
		switch err {
		// EAGAIN ，等待读就绪，然后继续
		case syscall.EAGAIN:
			if fd.pd.pollable() {
				// 如果没有数据，就会在 waitRead 函数中 park 当前的 goroutine ，直到可读
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		case syscall.ECONNABORTED:
			// This means that a socket on the listen
			// queue was closed before we Accept()ed it;
			// it's a silly error, so try again.
			//
			// ECONNABORTED 这意味着在我们 Accept 它之前，监听队列上的套接字已关闭。 重试。
			continue
		}
		return -1, nil, errcall, err
	}
}

// Seek wraps syscall.Seek.
// Seek 包装 seek 系统调用
func (fd *FD) Seek(offset int64, whence int) (int64, error) {
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	return syscall.Seek(fd.Sysfd, offset, whence)
}

// ReadDirent wraps syscall.ReadDirent.
// We treat this like an ordinary system call rather than a call
// that tries to fill the buffer.
// ReadFrom 包装 syscall.ReadDirent ，最终调用 getdents64 系统调用
// 读取目录结构，获取到的 buf 需要解析 ，解析过程可以参考 syscall.ParseDirent
func (fd *FD) ReadDirent(buf []byte) (int, error) {
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	for {
		n, err := syscall.ReadDirent(fd.Sysfd, buf)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		// Do not call eofError; caller does not expect to see io.EOF.
		return n, err
	}
}

// Fchdir wraps syscall.Fchdir.
// ReadFrom 包装 Fchdir 系统调用
func (fd *FD) Fchdir() error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return syscall.Fchdir(fd.Sysfd)
}

// Fstat wraps syscall.Fstat
// ReadFrom 包装 Fstat 系统调用
func (fd *FD) Fstat(s *syscall.Stat_t) error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return syscall.Fstat(fd.Sysfd, s)
}

// tryDupCloexec indicates whether F_DUPFD_CLOEXEC should be used.
// If the kernel doesn't support it, this is set to 0.
// tryDupCloexec 表示是否应使用 F_DUPFD_CLOEXEC 。 如果内核不支持，则将其设置为 0 。 默认 1 ， DupCloseOnExec 中出错后会改成 1 。
var tryDupCloexec = int32(1)

// DupCloseOnExec dups fd and marks it close-on-exec.
// DupCloseOnExec 创建 fd 副本 ， 并且设置 CLOEXEC 标志
func DupCloseOnExec(fd int) (int, string, error) {
	// 如果支持 F_DUPFD_CLOEXEC ， 使用 fcntl 系统调用
	if atomic.LoadInt32(&tryDupCloexec) == 1 {
		r0, e1 := fcntl(fd, syscall.F_DUPFD_CLOEXEC, 0)
		if e1 == nil {
			return r0, "", nil
		}
		switch e1.(syscall.Errno) {
		case syscall.EINVAL, syscall.ENOSYS:
			// Old kernel, or js/wasm (which returns
			// ENOSYS). Fall back to the portable way from
			// now on.
			// 不支持 F_DUPFD_CLOEXEC  ，这里没有返回 ， 会调用 dupCloseOnExecOld
			atomic.StoreInt32(&tryDupCloexec, 0)
		default:
			return -1, "fcntl", e1
		}
	}
	// 不支持 F_DUPFD_CLOEXEC ， 使用
	return dupCloseOnExecOld(fd)
}

// dupCloseOnExecUnixOld is the traditional way to dup an fd and
// set its O_CLOEXEC bit, using two system calls.
// dupCloseOnExecUnixOld 是使用两个系统调用来复制 fd 并将其 O_CLOEXEC 位置 1 的传统方法。
func dupCloseOnExecOld(fd int) (int, string, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	// 调用系统函数 dup
	newfd, err := syscall.Dup(fd)
	if err != nil {
		return -1, "dup", err
	}
	syscall.CloseOnExec(newfd)
	return newfd, "", nil
}

// Dup duplicates the file descriptor.
// Dup 创建 fd 副本 ，已经设置了 O_CLOEXEC 标志
func (fd *FD) Dup() (int, string, error) {
	if err := fd.incref(); err != nil {
		return -1, "", err
	}
	defer fd.decref()
	return DupCloseOnExec(fd.Sysfd)
}

// On Unix variants only, expose the IO event for the net code.
// 仅在 Unix 系统上，导出网络代码的IO事件。

// WaitWrite waits until data can be read from fd.
// WaitWrite 等待读
func (fd *FD) WaitWrite() error {
	return fd.pd.waitWrite(fd.isFile)
}

// WriteOnce is for testing only. It makes a single write call.
// WriteOnce 写一次
func (fd *FD) WriteOnce(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	return syscall.Write(fd.Sysfd, p)
}

// RawControl invokes the user-defined function f for a non-IO
// operation.
// RawControl 为非 IO 操作调用用户定义的函数 f 。
func (fd *FD) RawControl(f func(uintptr)) error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	f(uintptr(fd.Sysfd))
	return nil
}

// RawRead invokes the user-defined function f for a read operation.
// RawRead 调用用户定义的函数 f 进行读取操作。
func (fd *FD) RawRead(f func(uintptr) bool) error {
	if err := fd.readLock(); err != nil {
		return err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return err
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil
		}
		if err := fd.pd.waitRead(fd.isFile); err != nil {
			return err
		}
	}
}

// RawWrite invokes the user-defined function f for a write operation.
// RawWrite 调用用户定义的函数 f 进行写入操作。
func (fd *FD) RawWrite(f func(uintptr) bool) error {
	if err := fd.writeLock(); err != nil {
		return err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return err
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil
		}
		if err := fd.pd.waitWrite(fd.isFile); err != nil {
			return err
		}
	}
}
