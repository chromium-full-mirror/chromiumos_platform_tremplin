// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func sortLines(s string) string {
	lines := strings.Split(s, "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func repeatedRandomDo(repetitions int, wg *sync.WaitGroup, f func(int)) {
	wg.Add(repetitions)
	for n := 0; n < repetitions; n++ {
		go func(m int) {
			r := rand.Intn(100)
			time.Sleep(time.Duration(r) * time.Microsecond)
			f(m)
			wg.Done()
		}(n)
	}
}

func TestMaxSizeIsLimited(t *testing.T) {
	sink := &stdioSink{}
	data := make([]byte, sinkLimit-1)

	written, err := sink.Write(data)
	if written != len(data) || err != nil {
		t.Fatalf("Failed to write everything, wrote %d bytes and got error %v", written, err)
	}

	written, err = sink.Write(data)
	if written != len(data) || err != nil {
		t.Fatalf("Truncated write didn't report success, reportedly wrote %d bytes and got error %v", written, err)
	}

	written, err = sink.Write(data)
	if written != len(data) || err != nil {
		t.Fatalf("Write on full buffer didn't report success, reportedly wrote %d bytes and got error %v", written, err)
	}
	if s := sink.ReadString(); len(s) != sinkLimit {
		t.Fatalf("Read data was the wrong size, expected %d and got %d", sinkLimit, len(s))
	}

	written, err = sink.Write([]byte("Test"))
	if written != 4 || err != nil {
		t.Fatalf("Write on previously full buffer didn't report success after draining, wrote %d bytes and got error %v", written, err)
	}
	if s := sink.ReadString(); s != "Test" {
		t.Fatalf("Didn't read the right data after draining buffer, expected Test and got %s", s)
	}

	// Check that we're not continually allocating new underlying capacity. The size comparison at the end isn't exact because
	// the underlying container doesn't grow byte-by-byte.
	for n := 0; n < 10; n++ {
		sink.Write(data)
		sink.ReadString()
	}
	if sink.b.Cap() > sinkLimit*2 {
		t.Fatalf("Sink is continually growing in allocated size. Expected between %d and %d, got %d", sinkLimit, sinkLimit*2, sink.b.Cap())
	}
}

// Do lots of reads and writes concurrently, then check that the reads read everything which was
// written without any data duplication, loss, or other corruption.
func TestConcurrentEndToEnd(t *testing.T) {
	const reps = 2000
	sink := &stdioSink{}
	var wg sync.WaitGroup
	var sb strings.Builder
	for n := 0; n < reps; n++ {
		sb.WriteString(fmt.Sprintf("Test%d\n", n))
	}
	expected := sortLines(sb.String())

	// Lots of concurrent writes
	write := func(n int) { sink.Write([]byte(fmt.Sprintf("Test%d\n", n))) }
	repeatedRandomDo(reps, &wg, write)

	// Lots of concurrent reads
	s := [reps]string{}
	read := func(n int) { s[n] = sink.ReadString() }
	repeatedRandomDo(reps, &wg, read)

	wg.Wait()
	got := sortLines(strings.Join(s[:], "") + sink.ReadString())

	if expected != got {
		t.Fatalf("expected: %s, got: %s", expected, got)
	}
}
