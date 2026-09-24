package meta

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"syscall"
	"testing"
	"time"

	"github.com/google/btree"
	"github.com/stretchr/testify/require"
)

// Each test store is independent, including from memkv's process-wide setting file.
func cloneTestMeta(t *testing.T, allocator *SliceAllocator, namespace string) *kvMeta {
	t.Helper()
	conf := DefaultConf()
	conf.SliceAllocator = allocator
	m := &kvMeta{
		baseMeta: newBaseMeta("", conf),
		client: &prefixClient{
			tkvClient: &memKV{items: btree.New(2), temp: &kvItem{}},
			prefix:    []byte("isolated/"),
		},
		lockNamespace: namespace,
	}
	m.en = m
	t.Cleanup(func() { require.NoError(t, m.Shutdown()) })
	return m
}

func TestLegacyAllocationCompatibility(t *testing.T) {
	ctx := Background()
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	require.NoError(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, 1))
	legacy := cloneTestMeta(t, nil, "source")
	require.NoError(t, legacy.Init(&Format{Name: "legacy", MetaVersion: 1}, false))
	body, err := legacy.doLoad()
	require.NoError(t, err)
	var id uint64
	require.Zero(t, legacy.NewSlice(ctx, &id))
	require.Equal(t, uint64(1), id)
	counter, err := legacy.getCounter("nextChunk")
	require.NoError(t, err)

	configured := cloneTestMeta(t, allocator, "source")
	configured.client = legacy.client
	_, err = configured.Load(true)
	require.NoError(t, err)
	require.Zero(t, configured.NewSlice(ctx, &id))
	require.Equal(t, uint64(counter), id, "configured v1 must reserve from the private counter")
	after, err := configured.getCounter("nextChunk")
	require.NoError(t, err)
	require.Equal(t, counter+sliceIdBatch, after)
	next, err := allocator.Reserve(ctx, GlobalSliceAllocatorID, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1), next, "configured v1 must not reserve global IDs")
	stored, err := configured.doLoad()
	require.NoError(t, err)
	require.Equal(t, body, stored, "configured legacy mount changed format")
	require.Equal(t, legacy.flockKey(42), configured.flockKey(42))
	require.Zero(t, legacy.Flock(ctx, 42, 1, syscall.F_WRLCK, false))
	require.Equal(t, syscall.EAGAIN, configured.Flock(ctx, 42, 2, syscall.F_WRLCK, false))
	require.Zero(t, legacy.NewSlice(ctx, &id))
	require.Equal(t, uint64(2), id, "old writer's cached allocation must remain valid")
}

func TestLegacyAllocationIgnoresUnavailableAllocator(t *testing.T) {
	// An unusable client proves that even attempting external allocation would
	// fail: version 1 must not inspect this configured runtime dependency.
	m := cloneTestMeta(t, &SliceAllocator{}, "source")
	require.NoError(t, m.Init(&Format{Name: "legacy", MetaVersion: 1}, false))
	var id uint64
	require.Zero(t, m.NewSlice(Background(), &id))
	require.Equal(t, uint64(1), id)
	next, err := m.AdvanceNextChunk(0)
	require.NoError(t, err)
	require.Equal(t, int64(1+sliceIdBatch), next)
	next, err = m.AdvanceNextChunk(17)
	require.NoError(t, err)
	require.Equal(t, int64(18+sliceIdBatch), next)
}

func TestGlobalAllocationAcrossCloneRestarts(t *testing.T) {
	ctx := Background()
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	require.NoError(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, 1))
	seen := make(map[uint64]bool)
	for range 3 {
		m := cloneTestMeta(t, allocator, "target")
		format := &Format{Name: "member", MetaVersion: 3, SliceAllocator: GlobalSliceAllocatorID}
		require.NoError(t, m.Init(format, false))
		for range 8193 {
			var id uint64
			require.Zero(t, m.NewSlice(ctx, &id))
			require.NotZero(t, id&(uint64(1)<<63))
			require.False(t, seen[id], "clone or restarted writer reused %d", id)
			seen[id] = true
		}
		counter, err := m.getCounter("nextChunk")
		require.NoError(t, err)
		require.Equal(t, int64(1), counter)
	}
}

func TestPrepareCloneFormatPreservesRestoredMetadata(t *testing.T) {
	ctx := Background()
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	require.NoError(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, 1))
	source := cloneTestMeta(t, nil, "source")
	require.NoError(t, source.Init(&Format{Name: "legacy", UUID: "original", MetaVersion: 1}, false))
	require.NoError(t, source.setValue([]byte("setting"), []byte(`{"Name":"legacy","UUID":"original","MetaVersion":1,"FutureSetting":{"keep":true}}`)))
	require.NoError(t, source.setValue(source.sliceKey(123, 4096), packCounter(2)))
	require.Zero(t, source.Flock(ctx, RootInode, 10, syscall.F_RDLCK, false))
	require.Zero(t, source.Flock(ctx, 42, 11, syscall.F_WRLCK, false))
	require.Zero(t, source.Setlk(ctx, 42, 12, false, syscall.F_WRLCK, 0, 99, 1))

	target := cloneTestMeta(t, allocator, "target")
	before := make(map[string][]byte)
	require.NoError(t, source.client.scan(nil, func(k, v []byte) bool {
		before[string(k)] = bytes.Clone(v)
		require.NoError(t, target.setValue(k, bytes.Clone(v)))
		return true
	}))
	_, err := target.Load(true) // A caller may have already inspected the restored format.
	require.NoError(t, err)
	require.NoError(t, target.PrepareCloneFormat(ctx))
	require.Equal(t, 3, target.getFormat().MetaVersion)
	require.NoError(t, target.PrepareCloneFormat(ctx), "preparation replay must be harmless")
	require.NoError(t, target.client.scan(nil, func(k, v []byte) bool {
		if string(k) == "setting" {
			var original, updated map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(before[string(k)], &original))
			require.NoError(t, json.Unmarshal(v, &updated))
			require.Equal(t, json.RawMessage("3"), updated["MetaVersion"])
			original["MetaVersion"] = json.RawMessage("3")
			original["SliceAllocator"], _ = json.Marshal(GlobalSliceAllocatorID)
			require.Equal(t, original, updated)
		} else {
			require.Equal(t, before[string(k)], v, "changed restored key %x", k)
		}
		delete(before, string(k))
		return true
	}))
	require.Empty(t, before, "preparation removed restored keys")
	require.Zero(t, target.Flock(ctx, RootInode, 20, syscall.F_WRLCK, false))
	require.Zero(t, target.Flock(ctx, 42, 21, syscall.F_WRLCK, false))
	require.Zero(t, target.Setlk(ctx, 42, 22, false, syscall.F_WRLCK, 0, 99, 1))
	replica := cloneTestMeta(t, allocator, "target")
	replica.client = target.client
	_, err = replica.Load(true)
	require.NoError(t, err)
	require.Equal(t, syscall.EAGAIN, replica.Flock(ctx, RootInode, 30, syscall.F_WRLCK, false))
	require.Equal(t, syscall.EAGAIN, replica.Flock(ctx, 42, 31, syscall.F_WRLCK, false))
	require.Equal(t, syscall.EAGAIN, replica.Setlk(ctx, 42, 32, false, syscall.F_WRLCK, 0, 99, 1))
	require.Equal(t, syscall.EAGAIN, source.Flock(ctx, RootInode, 40, syscall.F_WRLCK, false))
	locks, flocks, err := target.ListLocks(context.Background(), 42)
	require.NoError(t, err)
	require.Len(t, locks, 1)
	require.Len(t, flocks, 1)
	require.Equal(t, uint64(21), flocks[0].Owner)
}

func TestGlobalAllocatorFailsClosed(t *testing.T) {
	ctx := Background()
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	legacy := cloneTestMeta(t, allocator, "target")
	require.NoError(t, legacy.Init(&Format{Name: "legacy", MetaVersion: 1}, false))
	var id uint64
	require.Zero(t, legacy.NewSlice(ctx, &id), "v1 must ignore the missing global record")
	require.Equal(t, uint64(1), id)
	require.Error(t, legacy.PrepareCloneFormat(ctx), "preparation must not initialize control")
	require.Equal(t, 1, legacy.getFormat().MetaVersion)
	counter, err := legacy.getCounter("nextChunk")
	require.NoError(t, err)
	require.Equal(t, int64(1+sliceIdBatch), counter)
	require.NoError(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, 1))
	legacy.lockNamespace = ""
	require.Error(t, legacy.PrepareCloneFormat(ctx))
	clone := cloneTestMeta(t, nil, "target")
	require.NoError(t, clone.Init(&Format{Name: "clone", MetaVersion: 3, SliceAllocator: GlobalSliceAllocatorID}, false))
	require.Equal(t, syscall.EIO, clone.NewSlice(ctx, &id), "v3 must not use private counter")
	require.Error(t, clone.PrepareCloneFormat(ctx))
	require.Error(t, (&Format{MetaVersion: 3}).CheckVersion())
	require.Error(t, (&Format{MetaVersion: 3, SliceAllocator: "invalid"}).CheckVersion())
	require.NoError(t, (&Format{MetaVersion: 3, SliceAllocator: GlobalSliceAllocatorID}).CheckVersion())
}

// Simulate a storage commit whose acknowledgment is lost. The callback really
// commits, so both transaction retries and caller retries must skip that block.
type lostAllocatorCommit struct {
	tkvClient
	failure error
}

func (c *lostAllocatorCommit) txn(ctx context.Context, f func(*kvTxn) error, retry int) error {
	if err := c.tkvClient.txn(ctx, f, retry); err != nil {
		return err
	}
	err := c.failure
	c.failure = nil
	return err
}

func TestGlobalAllocatorAmbiguousCommitAndBounds(t *testing.T) {
	ctx := Background()
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	require.NoError(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, 1))
	client := &lostAllocatorCommit{tkvClient: allocator.store.client, failure: errors.New("commit response lost")}
	allocator.store.client = client
	start, err := allocator.Reserve(ctx, GlobalSliceAllocatorID, 4096)
	require.Error(t, err)
	require.Zero(t, start, "ambiguous range must never escape to caller")
	start, err = allocator.Reserve(ctx, GlobalSliceAllocatorID, 4096)
	require.NoError(t, err)
	require.Equal(t, uint64(4097), start)
	client.failure = errors.New("write conflict after commit")
	start, err = allocator.Reserve(ctx, GlobalSliceAllocatorID, 4096)
	require.NoError(t, err)
	require.Equal(t, uint64(12289), start, "transaction retry reused ambiguous block")

	require.NoError(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, math.MaxInt64-4096))
	m := cloneTestMeta(t, allocator, "target")
	require.NoError(t, m.Init(&Format{Name: "clone", MetaVersion: 3, SliceAllocator: GlobalSliceAllocatorID}, false))
	var id uint64
	for range 4096 {
		require.Zero(t, m.NewSlice(ctx, &id))
	}
	require.Equal(t, uint64(math.MaxUint64-1), id)
	require.Equal(t, syscall.EIO, m.NewSlice(ctx, &id))
	start, err = allocator.Reserve(ctx, GlobalSliceAllocatorID, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(math.MaxInt64), start)
	require.Error(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, 0))
	require.Error(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, uint64(1)<<63))
	key, err := sliceAllocatorKey(GlobalSliceAllocatorID)
	require.NoError(t, err)
	corrupt := make([]byte, 8)
	binary.BigEndian.PutUint64(corrupt, uint64(1)<<63)
	require.NoError(t, allocator.store.setValue(key, corrupt))
	_, err = allocator.Reserve(ctx, GlobalSliceAllocatorID, 0)
	require.Error(t, err)
}

func TestHighBitSliceDumpLoad(t *testing.T) {
	ctx := Background()
	source := cloneTestMeta(t, nil, "source")
	require.NoError(t, source.Init(&Format{Name: "clone", MetaVersion: 3, SliceAllocator: GlobalSliceAllocatorID}, false))
	var inode Ino
	require.Zero(t, source.Mknod(ctx, RootInode, "file", TypeFile, 0644, 0, 0, "", &inode, nil))
	high := Slice{Id: uint64(1)<<63 | 17, Size: 4096, Len: 4096}
	low := Slice{Id: 41, Size: 4096, Len: 4096}
	require.Zero(t, source.Write(ctx, inode, 0, 0, high, time.Now()))
	require.Zero(t, source.Write(ctx, inode, 0, 4096, low, time.Now()))
	var dump bytes.Buffer
	require.NoError(t, source.DumpMeta(&dump, RootInode, 1, false, true, false))
	target := cloneTestMeta(t, nil, "target")
	require.NoError(t, target.LoadMeta(&dump))
	format, err := target.Load(true)
	require.NoError(t, err)
	require.Equal(t, 3, format.MetaVersion)
	var slices []Slice
	require.Zero(t, target.Read(ctx, inode, 0, &slices))
	require.Equal(t, []Slice{high, low}, slices)
	counter, err := target.getCounter("nextChunk")
	require.NoError(t, err)
	require.Equal(t, int64(42), counter, "high-bit slices must not enter signed legacy counter")
}
