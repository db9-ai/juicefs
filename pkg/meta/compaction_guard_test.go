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
	"errors"
	"fmt"
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
	family := fmt.Sprintf("%064x", 789)
	require.NoError(t, allocator.Initialize(Background(), family, 100))
	conf := DefaultConf()
	conf.NoBGJob, conf.MaxDeletes = true, 0
	conf.SliceAllocator, conf.CompactionGuard = allocator, guard
	client, err := newKVMeta("memkv", t.Name(), conf)
	require.NoError(t, err)
	m := client.(*kvMeta)
	require.NoError(t, m.Init(&Format{Name: "compaction", MetaVersion: 2, SliceAllocator: family}, true))
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
