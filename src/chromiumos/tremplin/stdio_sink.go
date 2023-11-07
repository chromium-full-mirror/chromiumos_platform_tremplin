// Copyright 2018 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bytes"
	"io"
	"log"
	"sync"
)

const sinkLimit = 1 * 1024 * 1024 // 1 MiB limit on unread output.

// stdioSink will sink Write calls into a local buffer, and will provide only
// EOF for Read calls. This makes it suitable for running non-interactive
// programs and capturing their output. stdioSink implements io.ReadCloser and
// io.WriteCloser.
// Unlike bytes.Buffer, it is thread-safe and has a capped size after which
// writes will be silently ignored.
type stdioSink struct {
	x sync.Mutex
	b bytes.Buffer
}

func (s *stdioSink) Close() error { return nil }

func (s *stdioSink) Write(p []byte) (int, error) {
	s.x.Lock()
	defer s.x.Unlock()
	if s.b.Len() >= sinkLimit {
		log.Print("stdioSink is full, silently discarding extra data")
		return len(p), nil
	}
	var n int
	var err error
	if s.b.Len()+len(p) > sinkLimit {
		log.Print("stdioSink doesn't have enough spare capacity, silently truncating extra data")
		n, err = s.b.Write(p[:sinkLimit-s.b.Len()])
		if s.b.Len() == sinkLimit {
			// Pretend that we didn't truncate our buffer.
			n = len(p)
		}
	} else {
		n, err = s.b.Write(p)
	}
	return n, err
}

func (s *stdioSink) Read(p []byte) (n int, err error) { return 0, io.EOF }

func (s *stdioSink) String() string {
	s.x.Lock()
	defer s.x.Unlock()
	return s.b.String()
}

// ReadString returns new items since the last ReadString
// Non-blocking i.e. if nothing new will return "".
// Thread-safe i.e. concurrent Read will not return overlapping data.
func (s *stdioSink) ReadString() string {
	s.x.Lock()
	defer s.x.Unlock()
	data := string(s.b.Next(sinkLimit))
	s.b.Reset()

	return data
}
