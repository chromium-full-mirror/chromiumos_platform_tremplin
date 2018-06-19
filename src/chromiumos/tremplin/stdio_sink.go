// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"io"
)

const sinkLimit = 1024 * 1024 * 1024 // 1 MiB limit on output

// stdioSink will sink Write calls into a local buffer, and will provide only
// EOF for Read calls. This makes it suitable for running non-interactive
// programs and capturing their output. stdioSink implements io.ReadCloser and
// io.WriteCloser.
type stdioSink bytes.Buffer

func (s *stdioSink) Close() error { return nil }

func (s *stdioSink) Write(p []byte) (n int, err error) {
	buf := (*bytes.Buffer)(s)
	if buf.Len() >= sinkLimit {
		return 0, errors.New("stdioSink is full")
	}
	if buf.Len()+len(p) > sinkLimit {
		p = p[:sinkLimit-buf.Len()]
	}
	return buf.Write(p)
}

func (s *stdioSink) Read(p []byte) (n int, err error) { return 0, io.EOF }

func (s *stdioSink) String() string { return (*bytes.Buffer)(s).String() }
