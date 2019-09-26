// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/internal/arenaskl"
	"github.com/cockroachdb/pebble/internal/record"
	"github.com/cockroachdb/pebble/vfs"
	"golang.org/x/exp/rand"
)

type testCommitEnv struct {
	logSeqNum     uint64
	visibleSeqNum uint64
	writePos      int64
	writeCount    uint64
	applyBuf      struct {
		sync.Mutex
		buf []uint64
	}
}

func (e *testCommitEnv) env() commitEnv {
	return commitEnv{
		logSeqNum:     &e.logSeqNum,
		visibleSeqNum: &e.visibleSeqNum,
		apply:         e.apply,
		write:         e.write,
	}
}

func (e *testCommitEnv) apply(b *Batch, mem *memTable) error {
	e.applyBuf.Lock()
	e.applyBuf.buf = append(e.applyBuf.buf, b.SeqNum())
	e.applyBuf.Unlock()
	return nil
}

func (e *testCommitEnv) write(b *Batch, _ *sync.WaitGroup, _ *error) (*memTable, error) {
	n := int64(len(b.storage.data))
	atomic.AddInt64(&e.writePos, n)
	atomic.AddUint64(&e.writeCount, 1)
	return nil, nil
}

func TestCommitQueue(t *testing.T) {
	var q commitQueue
	var batches [16]Batch
	for i := range batches {
		q.enqueue(&batches[i])
	}
	if b := q.dequeue(); b != nil {
		t.Fatalf("unexpectedly dequeued batch: %p", b)
	}
	atomic.StoreUint32(&batches[1].applied, 1)
	if b := q.dequeue(); b != nil {
		t.Fatalf("unexpectedly dequeued batch: %p", b)
	}
	for i := range batches {
		atomic.StoreUint32(&batches[i].applied, 1)
		if b := q.dequeue(); b != &batches[i] {
			t.Fatalf("%d: expected batch %p, but found %p", i, &batches[i], b)
		}
	}
	if b := q.dequeue(); b != nil {
		t.Fatalf("unexpectedly dequeued batch: %p", b)
	}
}

func TestCommitPipeline(t *testing.T) {
	var e testCommitEnv
	p := newCommitPipeline(e.env())

	n := 10000
	if raceEnabled {
		// Under race builds we have to limit the concurrency or we hit the
		// following error:
		//
		//   race: limit on 8128 simultaneously alive goroutines is exceeded, dying
		n = 1000
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			var b Batch
			_ = b.Set([]byte(fmt.Sprint(i)), nil, nil)
			_ = p.Commit(&b, false)
		}(i)
	}
	wg.Wait()

	if s := atomic.LoadUint64(&e.writeCount); uint64(n) != s {
		t.Fatalf("expected %d written batches, but found %d", n, s)
	}
	if n != len(e.applyBuf.buf) {
		t.Fatalf("expected %d written batches, but found %d",
			n, len(e.applyBuf.buf))
	}
	if s := atomic.LoadUint64(&e.logSeqNum); uint64(n) != s {
		t.Fatalf("expected %d, but found %d", n, s)
	}
	if s := atomic.LoadUint64(&e.visibleSeqNum); uint64(n) != s {
		t.Fatalf("expected %d, but found %d", n, s)
	}
}

func TestCommitPipelineAllocateSeqNum(t *testing.T) {
	var e testCommitEnv
	p := newCommitPipeline(e.env())

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	var prepareCount uint64
	var applyCount uint64
	for i := 1; i <= n; i++ {
		go func(i int) {
			defer wg.Done()
			p.AllocateSeqNum(i, func() {
				atomic.AddUint64(&prepareCount, uint64(1))
			}, func(seqNum uint64) {
				atomic.AddUint64(&applyCount, uint64(1))
			})
		}(i)
	}
	wg.Wait()

	if s := atomic.LoadUint64(&prepareCount); n != s {
		t.Fatalf("expected %d prepares, but found %d", n, s)
	}
	if s := atomic.LoadUint64(&applyCount); n != s {
		t.Fatalf("expected %d applies, but found %d", n, s)
	}
	// AllocateSeqNum always returns a non-zero sequence number causing the
	// values we see to be offset from 1.
	const total = 1 + 1 + 2 + 3 + 4 + 5 + 6 + 7 + 8 + 9 + 10
	if s := atomic.LoadUint64(&e.logSeqNum); total != s {
		t.Fatalf("expected %d, but found %d", total, s)
	}
	if s := atomic.LoadUint64(&e.visibleSeqNum); total != s {
		t.Fatalf("expected %d, but found %d", total, s)
	}
}

type syncDelayFile struct {
	vfs.File
	waiting chan struct{}
	done    chan struct{}
}

func (f *syncDelayFile) Sync() error {
	select {
	case f.waiting <- struct{}{}:
	default:
	}
	<-f.done
	return nil
}

func TestCommitPipelineWALClose(t *testing.T) {
	// This test stresses the edge case of N goroutines blocked in the
	// commitPipeline waiting for the log to sync when we concurrently decide to
	// rotate and close the log.

	mem := vfs.NewMem()
	f, err := mem.Create("test-wal")
	if err != nil {
		t.Fatal(err)
	}
	// syncDelayFile will send to the waiting channel when Sync is called, and
	// then block on the done channel befor returning from that call.
	sf := &syncDelayFile{
		File:    f,
		waiting: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}

	// A basic commitEnv which writes to a WAL.
	wal := record.NewLogWriter(sf, 0 /* logNum */)
	testEnv := commitEnv{
		logSeqNum:     new(uint64),
		visibleSeqNum: new(uint64),
		apply: func(b *Batch, mem *memTable) error {
			return nil
		},
		write: func(b *Batch, syncWG *sync.WaitGroup, syncErr *error) (*memTable, error) {
			_, err := wal.SyncRecord(b.storage.data, syncWG, syncErr)
			return nil, err
		},
	}
	p := newCommitPipeline(testEnv)

	// Launch N (commitConcurrency) goroutines which each create a batch and
	// commit it with sync==true. Because of the syncDelayFile, none of these
	// operations can complete until syncDelayFile.done is closed.
	errCh := make(chan error, cap(p.sem))
	for i := 0; i < cap(errCh); i++ {
		go func(i int) {
			b := &Batch{}
			if err := b.LogData([]byte("foo"), nil); err != nil {
				errCh <- err
				return
			}
			errCh <- p.Commit(b, true /* sync */)
		}(i)
	}

	<-sf.waiting
	// At this point, we know at least one operation is blocked waiting for the
	// Sync to finish, but most likely all are blocked.

	// Fire off another goroutine which will wait a short period of time and then
	// released the Sync. There isn't a clean way to do this as the wal.Close()
	// below will block indefinitely until the Sync is released.
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(sf.done)
	}()

	// Close the WAL. A "queue is full" panic means that something is broken.
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(errCh); i++ {
		err := <-errCh
		if err != nil {
			t.Fatal(err)
		}
	}
}

func BenchmarkCommitPipeline(b *testing.B) {
	for _, parallelism := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
		b.Run(fmt.Sprintf("parallel=%d", parallelism), func(b *testing.B) {
			b.SetParallelism(parallelism)
			mem := newMemTable(nil)
			wal := record.NewLogWriter(ioutil.Discard, 0 /* logNum */)

			nullCommitEnv := commitEnv{
				logSeqNum:     new(uint64),
				visibleSeqNum: new(uint64),
				apply: func(b *Batch, mem *memTable) error {
					err := mem.apply(b, b.SeqNum())
					if err != nil {
						return err
					}
					mem.unref()
					return nil
				},
				write: func(b *Batch, syncWG *sync.WaitGroup, syncErr *error) (*memTable, error) {
					for {
						err := mem.prepare(b)
						if err == arenaskl.ErrArenaFull {
							mem = newMemTable(nil)
							continue
						}
						if err != nil {
							return nil, err
						}
						break
					}

					_, err := wal.SyncRecord(b.storage.data, syncWG, syncErr)
					return mem, err
				},
			}
			p := newCommitPipeline(nullCommitEnv)

			const keySize = 8
			b.SetBytes(2 * keySize)
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				rng := rand.New(rand.NewSource(uint64(time.Now().UnixNano())))
				buf := make([]byte, keySize)

				for pb.Next() {
					batch := newBatch(nil)
					binary.BigEndian.PutUint64(buf, rng.Uint64())
					batch.Set(buf, buf, nil)
					if err := p.Commit(batch, true /* sync */); err != nil {
						b.Fatal(err)
					}
					batch.release()
				}
			})
		})
	}
}
