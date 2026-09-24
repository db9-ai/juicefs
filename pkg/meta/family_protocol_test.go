package meta

import (
	"bytes"
	"fmt"
	"math"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFamilyV4AllocationAcrossCloneRestarts(t *testing.T) {
	ctx := Background()
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	family := fmt.Sprintf("%064x", 4)
	require.NoError(t, allocator.Initialize(ctx, family, 1))
	seen := make(map[uint64]bool)
	for range 3 {
		m := cloneTestMeta(t, allocator, "member")
		require.NoError(t, m.Init(&Format{Name: "family", MetaVersion: 4, SliceAllocator: family}, false))
		for range 8193 {
			var id uint64
			require.Zero(t, m.NewSlice(ctx, &id))
			require.Positive(t, id)
			require.Less(t, id, uint64(1)<<63)
			require.False(t, seen[id], "clone or restarted writer reused %d", id)
			seen[id] = true
		}
		counter, err := m.getCounter("nextChunk")
		require.NoError(t, err)
		require.Equal(t, int64(1), counter, "v4 must not advance its cloned private counter")
		_, err = m.AdvanceNextChunk(1 << 40)
		require.Error(t, err, "fixed offsets are invalid for v4")
	}
	next, err := allocator.Reserve(ctx, family, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1+9*4096), next, "each mount reserves complete 4096-ID batches")

	// A new root family has a separate object prefix and may start again at 1.
	otherFamily := fmt.Sprintf("%064x", 5)
	require.NoError(t, allocator.Initialize(ctx, otherFamily, 1))
	other := cloneTestMeta(t, allocator, "other")
	require.NoError(t, other.Init(&Format{Name: "other", MetaVersion: 4, SliceAllocator: otherFamily}, false))
	var id uint64
	require.Zero(t, other.NewSlice(ctx, &id))
	require.Equal(t, uint64(1), id)
	next, err = allocator.Reserve(ctx, family, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1+9*4096), next)
}

func TestFamilyV4FormatAndAllocatorFailures(t *testing.T) {
	ctx := Background()
	family := fmt.Sprintf("%064x", 4)
	require.Error(t, (&Format{MetaVersion: 4}).CheckVersion())
	require.Error(t, (&Format{MetaVersion: 4, SliceAllocator: "invalid"}).CheckVersion())
	require.NoError(t, (&Format{MetaVersion: 4, SliceAllocator: family}).CheckVersion())
	require.Error(t, (&Format{MetaVersion: 5}).CheckVersion())

	m := cloneTestMeta(t, nil, "target")
	require.NoError(t, m.Init(&Format{Name: "family", MetaVersion: 4, SliceAllocator: family}, false))
	var id uint64
	require.Equal(t, syscall.EIO, m.NewSlice(ctx, &id))
	_, err := m.AdvanceNextChunk(0)
	require.Error(t, err)
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	m.conf.SliceAllocator = allocator
	require.Equal(t, syscall.EIO, m.NewSlice(ctx, &id), "missing family record must not be initialized")
	require.NoError(t, allocator.Initialize(ctx, family, math.MaxInt64-4096))
	for range 4096 {
		require.Zero(t, m.NewSlice(ctx, &id))
	}
	require.Equal(t, uint64(math.MaxInt64-1), id)
	require.Equal(t, syscall.EIO, m.NewSlice(ctx, &id), "v4 must not overflow into the v3 domain")
}

func TestFamilyV4RestoredMetadataAndAuthorityCAS(t *testing.T) {
	ctx := Background()
	family := fmt.Sprintf("%064x", 4)
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	require.NoError(t, allocator.Initialize(ctx, family, 1))
	source := cloneTestMeta(t, allocator, "source")
	require.NoError(t, source.Init(&Format{Name: "family", UUID: "original", MetaVersion: 4, SliceAllocator: family}, false))
	body := []byte(fmt.Sprintf(`{"Name":"family","UUID":"original","MetaVersion":4,"SliceAllocator":%q,"FutureSetting":{"keep":true}}`, family))
	require.NoError(t, source.setValue([]byte("setting"), body))
	require.NoError(t, source.setValue(source.sliceKey(123, 4096), packCounter(2)))
	require.Zero(t, source.SetXattr(ctx, RootInode, "authority", []byte("source"), XattrCreate))
	require.Zero(t, source.Flock(ctx, RootInode, 10, syscall.F_RDLCK, false))
	require.Zero(t, source.Setlk(ctx, 42, 11, false, syscall.F_WRLCK, 0, 99, 1))
	target := cloneTestMeta(t, allocator, "target")
	before := make(map[string][]byte)
	require.NoError(t, source.client.scan(nil, func(k, v []byte) bool {
		before[string(k)] = bytes.Clone(v)
		require.NoError(t, target.setValue(k, bytes.Clone(v)))
		return true
	}))

	// The integration validates immutable metadata and the existing control
	// record. No preparation API, format write, session or target flock is needed.
	format, err := target.Load(true)
	require.NoError(t, err)
	require.Equal(t, 4, format.MetaVersion)
	require.Equal(t, family, format.SliceAllocator)
	next, err := allocator.Reserve(ctx, format.SliceAllocator, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1), next)
	require.Zero(t, target.CompareAndSwapXattr(ctx, RootInode, "authority", []byte("source"), []byte("target")))
	require.Equal(t, syscall.EAGAIN, target.CompareAndSwapXattr(ctx, RootInode, "authority", []byte("source"), []byte("other")))
	require.NoError(t, target.client.scan(nil, func(k, v []byte) bool {
		if bytes.Equal(k, target.xattrKey(RootInode, "authority")) {
			require.Equal(t, []byte("target"), v)
		} else {
			require.Equal(t, before[string(k)], v, "authority rebind changed key %x", k)
		}
		delete(before, string(k))
		return true
	}))
	require.Empty(t, before)
	var authority []byte
	require.Zero(t, source.GetXattr(ctx, RootInode, "authority", &authority))
	require.Equal(t, []byte("source"), authority)

	require.Zero(t, target.Flock(ctx, RootInode, 20, syscall.F_WRLCK, false))
	require.Zero(t, target.Setlk(ctx, 42, 21, false, syscall.F_WRLCK, 0, 99, 1))
	replica := cloneTestMeta(t, allocator, "target")
	replica.client = target.client
	_, err = replica.Load(true)
	require.NoError(t, err)
	require.Equal(t, syscall.EAGAIN, replica.Flock(ctx, RootInode, 30, syscall.F_WRLCK, false))
	require.Equal(t, syscall.EAGAIN, replica.Setlk(ctx, 42, 31, false, syscall.F_WRLCK, 0, 99, 1))
	require.Equal(t, syscall.EAGAIN, source.Flock(ctx, RootInode, 40, syscall.F_WRLCK, false))

	// The historical migration must never reinterpret a v4 family as v3.
	require.NoError(t, allocator.Initialize(ctx, GlobalSliceAllocatorID, 1))
	require.Error(t, target.PrepareCloneFormat(ctx))
	stored, err := target.doLoad()
	require.NoError(t, err)
	require.Equal(t, body, stored)
}

func TestCompareAndSwapXattrExactBytes(t *testing.T) {
	ctx := Background()
	m := cloneTestMeta(t, nil, "target")
	require.NoError(t, m.Init(&Format{Name: "xattr", MetaVersion: 1}, false))
	require.Equal(t, syscall.EAGAIN, m.CompareAndSwapXattr(ctx, RootInode, "authority", []byte{}, []byte("created")), "empty expected does not match absence")
	require.Zero(t, m.CompareAndSwapXattr(ctx, RootInode, "authority", nil, []byte{}))
	require.Equal(t, syscall.EAGAIN, m.CompareAndSwapXattr(ctx, RootInode, "authority", nil, []byte("created")), "nil expected does not match an empty existing attribute")
	require.Zero(t, m.CompareAndSwapXattr(ctx, RootInode, "authority", []byte{}, []byte(`{ "owner": "source" }`)))
	require.Equal(t, syscall.EAGAIN, m.CompareAndSwapXattr(ctx, RootInode, "authority", []byte(`{"owner":"source"}`), []byte("target")), "semantic JSON equality is not byte equality")
	require.Zero(t, m.CompareAndSwapXattr(ctx, RootInode, "authority", []byte(`{ "owner": "source" }`), []byte("target")))
	require.Equal(t, syscall.EINVAL, m.CompareAndSwapXattr(ctx, RootInode, "", nil, []byte("value")))
	m.conf.ReadOnly = true
	require.Equal(t, syscall.EROFS, m.CompareAndSwapXattr(ctx, RootInode, "authority", []byte("target"), []byte("other")))
}

func TestCompareAndSwapXattrConcurrentWriters(t *testing.T) {
	ctx := Background()
	m := cloneTestMeta(t, nil, "target")
	require.NoError(t, m.Init(&Format{Name: "xattr", MetaVersion: 1}, false))
	require.Zero(t, m.SetXattr(ctx, RootInode, "authority", []byte("source"), XattrCreate))
	replica := cloneTestMeta(t, nil, "target")
	replica.client = m.client
	start := make(chan struct{})
	results := make(chan syscall.Errno, 2)
	go func() {
		<-start
		results <- m.CompareAndSwapXattr(ctx, RootInode, "authority", []byte("source"), []byte("target-a"))
	}()
	go func() {
		<-start
		results <- replica.CompareAndSwapXattr(ctx, RootInode, "authority", []byte("source"), []byte("target-b"))
	}()
	close(start)
	require.ElementsMatch(t, []syscall.Errno{0, syscall.EAGAIN}, []syscall.Errno{<-results, <-results})
}
