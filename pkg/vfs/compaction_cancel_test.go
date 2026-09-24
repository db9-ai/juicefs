package vfs

import (
	"io"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
)

func TestCloseSessionCancelsMemoryThrottledCompaction(t *testing.T) {
	conf := meta.DefaultConf()
	conf.NoBGJob, conf.MaxDeletes = true, 0
	conf.CompactionGuard = func(meta.Context) (func(), error) { return func() {}, nil }
	m, err := meta.NewClientWithError("memkv://compaction-cancel", conf)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	if err := m.Init(&meta.Format{Name: "compaction-cancel", MetaVersion: 1}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(true); err != nil {
		t.Fatal(err)
	}
	if err := m.NewSession(false); err != nil {
		t.Fatal(err)
	}
	var inode meta.Ino
	if st := m.Create(meta.Background(), meta.RootInode, "file", 0644, 0, 0, &inode, &meta.Attr{}); st != 0 {
		t.Fatal(st)
	}
	if st := m.Write(meta.Background(), inode, 0, 0, meta.Slice{Id: 1, Size: 4, Len: 4}, time.Now()); st != 0 {
		t.Fatal(st)
	}
	if st := m.Write(meta.Background(), inode, 0, 4, meta.Slice{Id: 2, Size: 4, Len: 4}, time.Now()); st != 0 {
		t.Fatal(st)
	}
	blob, err := object.CreateStorage("mem", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cc := chunk.Config{CacheDir: "memory", BlockSize: 1 << 20, Compress: "none", MaxUpload: 1, MaxDownload: 1, BufferSize: 32 << 20, PutTimeout: time.Second, GetTimeout: time.Second}
	store := chunk.NewCachedStore(blob, cc, nil)
	defer store.(io.Closer).Close()
	// A second volume's live reader/cache pages count toward this global total.
	pressure := utils.Alloc(64 << 20)
	entered := make(chan struct{})
	m.OnMsg(meta.CompactChunk, func(args ...interface{}) error {
		close(entered)
		return CompactContext(args[3].(meta.Context), cc, store, args[0].([]meta.Slice), args[1].(uint64), args[2].(uint8))
	})
	compactDone := make(chan struct{})
	go func() { m.Compact(meta.Background(), inode, 1, func() {}, func() {}); close(compactDone) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- m.CloseSession() }()
	blocked := false
	select {
	case err := <-closed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(300 * time.Millisecond):
		blocked = true
	}
	utils.Free(pressure)
	if blocked {
		if err := <-closed; err != nil {
			t.Error(err)
		}
	}
	<-compactDone
	if blocked {
		t.Fatal("CloseSession cannot cancel a compaction waiting for other volumes' memory; only releasing unrelated memory unblocks it")
	}
}
