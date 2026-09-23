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

package chunk

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/object"
	"github.com/stretchr/testify/require"
)

type controlledPutStorage struct {
	object.ObjectStorage
	put func(context.Context, string, io.Reader, ...object.AttrGetter) error
}

func (s *controlledPutStorage) Put(ctx context.Context, key string, in io.Reader, getters ...object.AttrGetter) error {
	return s.put(ctx, key, in, getters...)
}

func TestSliceFinishDrainsUploadsAfterError(t *testing.T) {
	mem, err := object.CreateStorage("mem", "", "", "", "")
	require.NoError(t, err)
	entered := make(chan struct{})
	release := make(chan struct{})
	failed := make(chan struct{})
	blob := &controlledPutStorage{ObjectStorage: mem}
	blob.put = func(ctx context.Context, key string, in io.Reader, getters ...object.AttrGetter) error {
		if strings.Contains(key, "_0_") {
			<-entered
			close(failed)
			return errors.New("first block failed")
		}
		close(entered)
		<-release
		return mem.Put(ctx, key, in, getters...)
	}
	conf := defaultConf
	conf.CacheDir, conf.MaxUpload = "memory", 2
	store := NewCachedStore(blob, conf, nil).(*cachedStore)
	store.conf.MaxRetries = 0 // Exercise the terminal error without retry delays.
	defer store.Close()
	writer := store.NewWriter(41, 0)
	_, err = writer.WriteAt(make([]byte, 2*conf.BlockSize), 0)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- writer.Finish(2 * conf.BlockSize) }()
	<-failed
	select {
	case err := <-done:
		close(release)
		t.Fatalf("Finish returned while another PUT was running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.ErrorContains(t, <-done, "first block failed")
	writer.Abort()
	_, err = mem.Head(context.Background(), "chunks/0/0/41_1_1048576")
	require.Error(t, err, "Abort must remove the completed second block")
}

func TestSliceAbortWaitsForStartedUpload(t *testing.T) {
	mem, err := object.CreateStorage("mem", "", "", "", "")
	require.NoError(t, err)
	blob := &blockingPutStorage{ObjectStorage: mem, entered: make(chan struct{}), release: make(chan struct{})}
	conf := defaultConf
	conf.CacheDir = "memory"
	store := NewCachedStore(blob, conf, nil).(*cachedStore)
	defer store.Close()
	writer := store.NewWriter(42, 0)
	_, err = writer.WriteAt(make([]byte, conf.BlockSize), 0)
	require.NoError(t, err)
	require.NoError(t, writer.FlushTo(conf.BlockSize))
	<-blob.entered
	done := make(chan struct{})
	go func() { writer.Abort(); close(done) }()
	select {
	case <-done:
		close(blob.release)
		t.Fatal("Abort returned before the physical PUT finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(blob.release)
	<-done
	_, err = mem.Head(context.Background(), "chunks/0/0/42_0_1048576")
	require.Error(t, err, "the PUT must finish before Abort deletes its object")
}

func TestCachedStorePutTimeoutDoesNotDetachUpload(t *testing.T) {
	mem, err := object.CreateStorage("mem", "", "", "", "")
	require.NoError(t, err)
	expired := make(chan struct{})
	release := make(chan struct{})
	blob := &controlledPutStorage{ObjectStorage: mem}
	blob.put = func(ctx context.Context, key string, in io.Reader, getters ...object.AttrGetter) error {
		<-ctx.Done()
		close(expired)
		<-release // Model a provider which finishes its request after cancellation.
		return mem.Put(context.Background(), key, in, getters...)
	}
	conf := defaultConf
	conf.CacheDir, conf.PutTimeout = "memory", 10*time.Millisecond
	store := NewCachedStore(blob, conf, nil).(*cachedStore)
	defer store.Close()
	page := NewPage([]byte("data"))
	defer page.Release()
	done := make(chan error, 1)
	go func() { done <- store.put(context.Background(), "chunks/late", page) }()
	<-expired
	select {
	case err := <-done:
		close(release)
		t.Fatalf("timeout detached the physical PUT: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-done)
	_, err = mem.Head(context.Background(), "chunks/late")
	require.NoError(t, err)
}
