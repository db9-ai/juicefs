package meta

import (
	"bytes"
	"context"
	"syscall"
	"testing"

	"github.com/google/btree"
	"github.com/stretchr/testify/require"
)

// Model the observed mismatch: a transactional BatchGet misses an existing
// parent inode, while an independent point read still finds its committed value.
type missingParentTxn struct {
	kvtxn
	parent []byte
}

func (tx *missingParentTxn) gets(keys ...[]byte) [][]byte {
	values := tx.kvtxn.gets(keys...)
	for i, key := range keys {
		if bytes.Equal(key, tx.parent) {
			values[i] = nil
		}
	}
	return values
}

type mknodVisibilityClient struct {
	tkvClient
	backend          string
	parent           []byte
	missingSnapshots int
	attempts         int
	lookups          int
	lookupError      error
	commitError      error
	afterLookup      func()
}

func (c *mknodVisibilityClient) name() string { return c.backend }

func (c *mknodVisibilityClient) txn(ctx context.Context, f func(*kvTxn) error, retry int) error {
	c.attempts++
	err := c.tkvClient.txn(ctx, func(tx *kvTxn) error {
		if c.attempts <= c.missingSnapshots {
			tx.kvtxn = &missingParentTxn{kvtxn: tx.kvtxn, parent: c.parent}
		}
		return f(tx)
	}, retry)
	if err != nil {
		return err
	}
	// A commit error must not cause a replay even if the write took effect.
	return c.commitError
}

func (c *mknodVisibilityClient) simpleTxn(ctx context.Context, f func(*kvTxn) error, retry int) error {
	c.lookups++
	if c.lookupError != nil {
		return c.lookupError
	}
	err := c.tkvClient.simpleTxn(ctx, f, retry)
	if c.afterLookup != nil {
		c.afterLookup()
	}
	return err
}

func newMknodVisibilityFixture() (*kvMeta, *mknodVisibilityClient, *memKV) {
	store := &memKV{items: btree.New(2), temp: &kvItem{}}
	c := &mknodVisibilityClient{tkvClient: store, backend: "tikv"}
	m := &kvMeta{baseMeta: newBaseMeta("mknod-visibility", testConfig()), client: c}
	m.en = m
	m.fmt = testFormat()
	c.parent = m.inodeKey(RootInode)
	store.set(string(c.parent), m.marshal(&Attr{Typ: TypeDirectory, Mode: 0755, Parent: RootInode, Nlink: 2}))
	return m, c, store
}

func TestMknodConfirmedMissingParent(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.Errno(0), m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 2, c.attempts)
	require.Equal(t, 1, c.lookups)
	require.Equal(t, m.packEntry(TypeDirectory, inode), store.get(string(m.entryKey(RootInode, "child"))).value)
	var parent Attr
	m.parseAttr(store.get(string(c.parent)).value, &parent)
	require.Equal(t, uint32(3), parent.Nlink, "the child must be created only once")
}

func TestMknodMissingParentRetryBound(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	c.missingSnapshots = 100
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 3, c.attempts)
	require.Equal(t, 2, c.lookups)
	require.Nil(t, store.get(string(m.entryKey(RootInode, "child"))))
	require.Nil(t, store.get(string(m.inodeKey(inode))))
	var parent Attr
	m.parseAttr(store.get(string(c.parent)).value, &parent)
	require.Equal(t, uint32(2), parent.Nlink)
}

func TestMknodActuallyMissingParent(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	store.set(string(c.parent), nil)
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Equal(t, 1, c.lookups)
	require.Nil(t, store.get(string(m.inodeKey(inode))))
}

func TestMknodCommitErrorIsNotReplayed(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	c.commitError = syscall.ENOENT
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 2, c.attempts)
	require.Equal(t, 1, c.lookups)
	require.NotNil(t, store.get(string(m.inodeKey(inode))))
	var parent Attr
	m.parseAttr(store.get(string(c.parent)).value, &parent)
	require.Equal(t, uint32(3), parent.Nlink)
}

func TestMknodTrashParentIsNotRetried(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	store.set(string(c.parent), m.marshal(&Attr{Typ: TypeDirectory, Mode: 0755, Parent: TrashInode + 1, Nlink: 2}))
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Zero(t, c.lookups)
}

func TestMknodMissingParentLookupError(t *testing.T) {
	m, c, _ := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	c.lookupError = syscall.EIO
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.EIO, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Equal(t, 1, c.lookups)
}

func TestMknodMissingParentCancellation(t *testing.T) {
	m, c, _ := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.afterLookup = cancel
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.EINTR, m.doMknod(WrapContext(ctx), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Equal(t, 1, c.lookups)
}

func TestMknodPinnedSnapshotIsNotRetried(t *testing.T) {
	m, c, _ := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	ctx := Background().WithValue(txSessionKey{}, uint64(123))
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(ctx, RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Zero(t, c.lookups)
}

func TestMknodExplicitRetryBudgetIsNotOverridden(t *testing.T) {
	m, c, _ := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	ctx := Background().WithValue(txMaxRetryKey{}, 1)
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(ctx, RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Zero(t, c.lookups)
}

func TestMknodOtherBackendIsNotRetried(t *testing.T) {
	m, c, _ := newMknodVisibilityFixture()
	c.backend = "memkv"
	c.missingSnapshots = 1
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Zero(t, c.lookups)
}

func TestMknodVisibleParentNeedsNoConfirmation(t *testing.T) {
	m, c, _ := newMknodVisibilityFixture()
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.Errno(0), m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 1, c.attempts)
	require.Zero(t, c.lookups)
}

func TestMknodParentDeletedAfterConfirmation(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	c.afterLookup = func() {
		c.afterLookup = nil
		store.set(string(c.parent), nil)
	}
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 2, c.attempts)
	require.Equal(t, 2, c.lookups)
	require.Nil(t, store.get(string(m.entryKey(RootInode, "child"))))
	require.Nil(t, store.get(string(m.inodeKey(inode))))
}

func TestMknodDoesNotFollowReplacementParent(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	parent, replacement := Ino(2), Ino(3)
	c.parent = m.inodeKey(parent)
	store.set(string(c.parent), m.marshal(&Attr{Typ: TypeDirectory, Mode: 0755, Parent: RootInode, Nlink: 2}))
	store.set(string(m.entryKey(RootInode, "parent")), m.packEntry(TypeDirectory, parent))
	c.missingSnapshots = 1
	c.afterLookup = func() {
		c.afterLookup = nil
		store.set(string(c.parent), nil)
		store.set(string(m.inodeKey(replacement)), m.marshal(&Attr{Typ: TypeDirectory, Mode: 0755, Parent: RootInode, Nlink: 2}))
		store.set(string(m.entryKey(RootInode, "parent")), m.packEntry(TypeDirectory, replacement))
	}
	inode := Ino(4)
	attr := Attr{Typ: TypeDirectory, Parent: parent, Nlink: 2}

	require.Equal(t, syscall.ENOENT, m.doMknod(Background(), parent, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 2, c.attempts)
	require.Equal(t, 2, c.lookups)
	require.Equal(t, m.packEntry(TypeDirectory, replacement), store.get(string(m.entryKey(RootInode, "parent"))).value)
	require.Nil(t, store.get(string(m.entryKey(parent, "child"))))
	require.Nil(t, store.get(string(m.entryKey(replacement, "child"))))
	require.Nil(t, store.get(string(m.inodeKey(inode))))
}

func TestMknodRechecksPermissionsAfterConfirmation(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	store.set(string(c.parent), m.marshal(&Attr{Typ: TypeDirectory, Mode: 0777, Parent: RootInode, Nlink: 2}))
	c.missingSnapshots = 1
	c.afterLookup = func() {
		store.set(string(c.parent), m.marshal(&Attr{Typ: TypeDirectory, Mode: 0555, Parent: RootInode, Nlink: 2}))
	}
	ctx := NewContext(1, 1000, []uint32{1000})
	defer ctx.Cancel()
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Uid: 1000, Gid: 1000, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.EACCES, m.doMknod(ctx, RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 2, c.attempts)
	require.Equal(t, 1, c.lookups)
	require.Nil(t, store.get(string(m.inodeKey(inode))))
}

func TestMknodCompetingCreateAfterConfirmation(t *testing.T) {
	m, c, store := newMknodVisibilityFixture()
	c.missingSnapshots = 1
	c.afterLookup = func() {
		// Force the competing operation into the gap, without a timing race.
		// The parent lock must already be released before the confirmation read.
		lock := &m.txlocks[uint(RootInode)%nlocks]
		require.True(t, lock.TryLock(), "confirmation must not hold the parent transaction lock")
		lock.Unlock()
		peer := &kvMeta{baseMeta: m.baseMeta, client: store}
		peerInode := Ino(3)
		peerAttr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}
		require.Equal(t, syscall.Errno(0), peer.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &peerInode, &peerAttr))
	}
	inode := Ino(2)
	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}

	require.Equal(t, syscall.EEXIST, m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &inode, &attr))
	require.Equal(t, 2, c.attempts)
	require.Equal(t, 1, c.lookups)
	require.Equal(t, Ino(3), inode)
	require.Equal(t, m.packEntry(TypeDirectory, 3), store.get(string(m.entryKey(RootInode, "child"))).value)
	require.Nil(t, store.get(string(m.inodeKey(2))))
	var parent Attr
	m.parseAttr(store.get(string(c.parent)).value, &parent)
	require.Equal(t, uint32(3), parent.Nlink, "only the competing create may increment nlink")
}
