/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newCompactionGuardMeta(t *testing.T, guard func(Context) (func(), error)) (*kvMeta, Ino) {
	t.Helper()
	allocator, err := OpenSliceAllocator("memkv://compaction-allocator")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, allocator.Close()) })
	family := GlobalSliceAllocatorID
	require.NoError(t, allocator.Initialize(Background(), family, 100))
	conf := DefaultConf()
	conf.NoBGJob, conf.MaxDeletes = true, 0
	conf.SliceAllocator, conf.CompactionGuard = allocator, guard
	client, err := newKVMeta("memkv", t.Name(), conf)
	require.NoError(t, err)
	m := client.(*kvMeta)
	require.NoError(t, m.Init(&Format{Name: "compaction", MetaVersion: 3, SliceAllocator: family}, true))
	_, err = m.Load(true)
	require.NoError(t, err)
	require.NoError(t, m.NewSession(false))
	t.Cleanup(func() { require.NoError(t, m.CloseSession()); require.NoError(t, m.Shutdown()) })
	var inode Ino
	require.Equal(t, syscall.Errno(0), m.Create(Background(), RootInode, "file", 0644, 0, 0, &inode, &Attr{}))
	require.Equal(t, syscall.Errno(0), m.Write(Background(), inode, 0, 0, Slice{Id: 1, Size: 4, Len: 4}, time.Now()))
	require.Equal(t, syscall.Errno(0), m.Write(Background(), inode, 0, 4, Slice{Id: 2, Size: 4, Len: 4}, time.Now()))
	return m, inode
}

func TestCompactionGuardRejectsBeforeUploadAndMetadataCommit(t *testing.T) {
	called := false
	m, inode := newCompactionGuardMeta(t, func(Context) (func(), error) {
		called = true
		return nil, errors.New("retired")
	})
	m.OnMsg(CompactChunk, func(...interface{}) error { t.Error("rejected compaction uploaded data"); return nil })
	m.compactChunk(inode, 0, false, false, 0)
	require.True(t, called)
	slices, st := m.doRead(Background(), inode, 0)
	require.Zero(t, st)
	require.Len(t, slices, 2)
}

func TestCompactionGuardCoversUploadAndMetadataCommit(t *testing.T) {
	held := false
	var m *kvMeta
	var inode Ino
	m, inode = newCompactionGuardMeta(t, func(Context) (func(), error) {
		require.False(t, held)
		held = true
		return func() {
			slices, st := m.doRead(Background(), inode, 0)
			require.Zero(t, st)
			require.Len(t, slices, 1, "the guard must remain held through metadata commit")
			held = false
		}, nil
	})
	m.OnMsg(CompactChunk, func(...interface{}) error {
		require.True(t, held, "the upload must hold the guard")
		return nil
	})
	m.compactChunk(inode, 0, false, true, 0)
	require.False(t, held)
}

func TestCompactionGuardReleasesAfterUploadFailure(t *testing.T) {
	released := false
	m, inode := newCompactionGuardMeta(t, func(Context) (func(), error) {
		return func() { released = true }, nil
	})
	m.OnMsg(CompactChunk, func(...interface{}) error { return errors.New("upload failed") })
	m.compactChunk(inode, 0, false, false, 0)
	require.True(t, released)
	slices, st := m.doRead(Background(), inode, 0)
	require.Zero(t, st)
	require.Len(t, slices, 2)
}

func TestCloseSessionCancelsAndJoinsCompactionGuard(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	m, inode := newCompactionGuardMeta(t, func(ctx Context) (func(), error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil, ctx.Err()
	})
	compacted := make(chan struct{})
	go func() { m.compactChunk(inode, 0, false, false, 0); close(compacted) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- m.CloseSession() }()
	<-canceled
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("session closed before compaction left its guard: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-closed)
	<-compacted
	// A read-triggered task queued before shutdown must not reenter the guard.
	m.compactChunk(inode, 0, false, false, 0)
}

func TestCloseSessionJoinsAdmittedCompaction(t *testing.T) {
	admitted := make(chan Context, 1)
	released := make(chan struct{})
	m, inode := newCompactionGuardMeta(t, func(ctx Context) (func(), error) {
		admitted <- ctx
		return func() { close(released) }, nil
	})
	uploading := make(chan struct{})
	finishUpload := make(chan struct{})
	m.OnMsg(CompactChunk, func(...interface{}) error {
		close(uploading)
		<-finishUpload
		return nil
	})
	compacted := make(chan struct{})
	go func() { m.compactChunk(inode, 0, false, false, 0); close(compacted) }()
	<-uploading
	ctx := <-admitted
	closed := make(chan error, 1)
	go func() { closed <- m.CloseSession() }()
	<-ctx.Done()
	select {
	case err := <-closed:
		close(finishUpload)
		t.Fatalf("session closed while an admitted upload was running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishUpload)
	require.NoError(t, <-closed)
	select {
	case <-released:
	default:
		t.Fatal("session closed before releasing the compaction guard")
	}
	<-compacted
	slices, st := m.doRead(Background(), inode, 0)
	require.Zero(t, st)
	require.Len(t, slices, 1, "session shutdown must wait for the admitted metadata commit")
}

func TestForcedCompactionReleasesGuardBeforeNextPass(t *testing.T) {
	held := false
	m, inode := newCompactionGuardMeta(t, func(Context) (func(), error) {
		require.False(t, held, "recursive compaction must not retain the previous admission")
		held = true
		return func() { held = false }, nil
	})
	passes := 0
	m.OnMsg(CompactChunk, func(...interface{}) error {
		passes++
		if passes == 1 {
			// Concurrent writes survive this pass and require a second pass.
			require.Zero(t, m.Write(Background(), inode, 0, 8, Slice{Id: 3, Size: 4, Len: 4}, time.Now()))
		}
		return nil
	})
	m.compactChunk(inode, 0, false, true, 0)
	require.Equal(t, 2, passes)
	require.False(t, held)
}

// Block the real allocator transaction boundary, not the compaction callback:
// cancellation must reach a cache refill before any object upload starts.
type blockedCompactionAllocator struct {
	tkvClient
	entered chan context.Context
	release chan struct{}
}

func (c *blockedCompactionAllocator) txn(ctx context.Context, f func(*kvTxn) error, retry int) error {
	c.entered <- ctx
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return errors.New("allocator fixture released")
	}
}

func TestCloseSessionCancelsCompactionAllocatorRefill(t *testing.T) {
	guardCtx := make(chan Context, 1)
	m, inode := newCompactionGuardMeta(t, func(ctx Context) (func(), error) {
		guardCtx <- ctx
		return func() {}, nil
	})
	blocked := &blockedCompactionAllocator{
		tkvClient: m.conf.SliceAllocator.store.client,
		entered:   make(chan context.Context, 1), release: make(chan struct{}),
	}
	m.conf.SliceAllocator.store.client = blocked
	m.OnMsg(CompactChunk, func(...interface{}) error {
		t.Error("canceled allocator refill reached the upload callback")
		return nil
	})
	compacted := make(chan struct{})
	go func() { m.compactChunk(inode, 0, false, false, 0); close(compacted) }()
	allocatorCtx := <-blocked.entered
	sessionCtx := <-guardCtx
	closed := make(chan error, 1)
	go func() { closed <- m.CloseSession() }()
	<-sessionCtx.Done()
	select {
	case <-allocatorCtx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Error("session cancellation did not reach the compaction allocator")
	}
	// Always release the fixture, so the negative case reports its assertion
	// instead of hanging test cleanup while CloseSession waits for compaction.
	close(blocked.release)
	<-compacted
	require.NoError(t, <-closed)
}

type blockedCompactionRead struct {
	tkvClient
	entered chan context.Context
	release chan struct{}
}

func (c *blockedCompactionRead) simpleTxn(ctx context.Context, f func(*kvTxn) error, retry int) error {
	c.entered <- ctx
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return errors.New("fixture released")
	}
}
func TestCloseSessionCancelsCompactionMetadataRead(t *testing.T) {
	conf := DefaultConf()
	conf.NoBGJob, conf.MaxDeletes = true, 0
	conf.CompactionGuard = func(Context) (func(), error) { return func() {}, nil }
	client, err := newKVMeta("memkv", t.Name(), conf)
	if err != nil {
		t.Fatal(err)
	}
	m := client.(*kvMeta)
	defer m.Shutdown()
	if err := m.Init(&Format{Name: "compaction-cancel", MetaVersion: 1}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(true); err != nil {
		t.Fatal(err)
	}
	blocked := &blockedCompactionRead{tkvClient: m.client, entered: make(chan context.Context, 1), release: make(chan struct{})}
	m.client = blocked
	compacted := make(chan struct{})
	go func() { m.compactChunk(RootInode, 0, false, false, 0); close(compacted) }()
	readCtx := <-blocked.entered
	closed := make(chan error, 1)
	go func() { closed <- m.CloseSession() }()
	select {
	case <-readCtx.Done():
	case <-time.After(300 * time.Millisecond):
		t.Error("CloseSession cancellation did not reach compaction metadata read")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("CloseSession blocked on uncancellable compaction metadata read")
		close(blocked.release)
		if err := <-closed; err != nil {
			t.Error(err)
		}
	}
	<-compacted
}

func TestLegacyCloseSessionDoesNotCancelOrJoinCompaction(t *testing.T) {
	m := cloneTestMeta(t, nil, "legacy")
	require.NoError(t, m.Init(&Format{Name: "legacy", MetaVersion: 1}, false))
	blocked := &blockedCompactionRead{tkvClient: m.client, entered: make(chan context.Context, 1), release: make(chan struct{})}
	m.client = blocked
	compacted := make(chan struct{})
	go func() { m.compactChunk(RootInode, 0, false, false, 0); close(compacted) }()
	ctx := <-blocked.entered
	closed := make(chan error, 1)
	go func() { closed <- m.CloseSession() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		close(blocked.release)
		<-compacted
		<-closed
		t.Fatal("legacy close joined a background compaction")
	}
	require.NoError(t, ctx.Err(), "legacy background compaction must not be canceled")
	close(blocked.release)
	<-compacted
}
