package meta

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"testing"

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
	family := fmt.Sprintf("%064x", 3)
	require.NoError(t, allocator.Initialize(ctx, family, 1))
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
	next, err := allocator.Reserve(ctx, family, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1), next, "configured v1 must not reserve family IDs")
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

func TestFamilyAllocatorAmbiguousCommitAndCorruption(t *testing.T) {
	ctx := Background()
	allocator := &SliceAllocator{store: cloneTestMeta(t, nil, "control")}
	family := fmt.Sprintf("%064x", 3)
	require.NoError(t, allocator.Initialize(ctx, family, 1))
	client := &lostAllocatorCommit{tkvClient: allocator.store.client, failure: errors.New("commit response lost")}
	allocator.store.client = client
	start, err := allocator.Reserve(ctx, family, 4096)
	require.Error(t, err)
	require.Zero(t, start, "ambiguous range must never escape to caller")
	start, err = allocator.Reserve(ctx, family, 4096)
	require.NoError(t, err)
	require.Equal(t, uint64(4097), start)
	client.failure = errors.New("write conflict after commit")
	start, err = allocator.Reserve(ctx, family, 4096)
	require.NoError(t, err)
	require.Equal(t, uint64(12289), start, "transaction retry reused ambiguous block")

	require.Error(t, allocator.Initialize(ctx, family, 0))
	require.Error(t, allocator.Initialize(ctx, family, uint64(1)<<63))
	key, err := sliceAllocatorKey(family)
	require.NoError(t, err)
	corrupt := make([]byte, 8)
	binary.BigEndian.PutUint64(corrupt, uint64(1)<<63)
	require.NoError(t, allocator.store.setValue(key, corrupt))
	_, err = allocator.Reserve(ctx, family, 0)
	require.Error(t, err)
}
