// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

// parsePort parses service as a decimal integer and returns the
// corresponding value as port. It is the caller's responsibility to
// parse service as a non-decimal integer when needsLookup is true.
//
// Some system resolvers will return a valid port number when given a number
// over 65536 (see https://golang.org/issues/11715). Alas, the parser
// can't bail early on numbers > 65536. Therefore reasonably large/small
// numbers are parsed in full and rejected if invalid.
// parsePort 将服务解析为十进制整数，并返回相应的值作为 port 。 当 needLookup 为 true 时，如果是非十进制整数，则由调用者负责将服务解析。
//
func parsePort(service string) (port int, needsLookup bool) {
	if service == "" {
		// Lock in the legacy behavior that an empty string
		// means port 0. See golang.org/issue/13610.
		// 空字符串表示端口 0
		return 0, false
	}
	const (
		max    = uint32(1<<32 - 1)
		cutoff = uint32(1 << 30)
	)
	// 正负数判断
	neg := false
	if service[0] == '+' {
		service = service[1:]
	} else if service[0] == '-' {
		neg = true
		service = service[1:]
	}
	// 解析端口
	var n uint32
	for _, d := range service {
		if '0' <= d && d <= '9' {
			d -= '0'
		} else {
			// 是非十进制整数，则由调用者负责将服务解析。
			return 0, true
		}
		if n >= cutoff {
			n = max
			break
		}
		n *= 10
		nn := n + uint32(d)
		if nn < n || nn > max {
			n = max
			break
		}
		n = nn
	}
	if !neg && n >= cutoff {
		port = int(cutoff - 1)
	} else if neg && n > cutoff {
		port = int(cutoff)
	} else {
		port = int(n)
	}
	if neg {
		port = -port
	}
	return port, false
}
