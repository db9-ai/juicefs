package meta

import (
	"bytes"
	"context"
	"sync"
	"syscall"
	"testing"

	"github.com/google/btree"
)

func TestRawCloneLocksUsePhysicalIdentity(t *testing.T) {
	sourceClient := &memKV{items: btree.New(2), temp: &kvItem{}}
	targetClient := &memKV{items: btree.New(2), temp: &kvItem{}}
	newMeta := func(client tkvClient, scope string) *kvMeta {
		m := &kvMeta{baseMeta: newBaseMeta("", DefaultConf()), client: client, lockNamespace: scope}
		m.en = m
		m.fmt = &Format{MetaVersion: 2}
		return m
	}
	source := newMeta(sourceClient, "cluster/1/source")
	target := newMeta(targetClient, "cluster/1/target")
	replica := newMeta(targetClient, "cluster/1/target")
	ctx := Background()
	// Snapshot both a root read fence and an append's exclusive inode lock.
	for inode, kind := range map[Ino]uint32{RootInode: syscall.F_RDLCK, 42: syscall.F_WRLCK} {
		if st := source.Flock(ctx, inode, 100, kind, false); st != 0 {
			t.Fatal(st)
		}
	}
	if st := source.Setlk(ctx, 42, 101, false, syscall.F_WRLCK, 0, 99, 1); st != 0 {
		t.Fatal(st)
	}
	if err := source.client.scan(nil, func(k, v []byte) bool {
		if err := target.client.txn(ctx, func(tx *kvTxn) error { tx.set(k, append([]byte(nil), v...)); return nil }, 0); err != nil {
			t.Fatal(err)
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	// Source locks remain live. Neither shared nor exclusive source state may
	// become an owner in the target's lock domain.
	for _, inode := range []Ino{RootInode, 42} {
		if st := target.Flock(ctx, inode, 200, syscall.F_WRLCK, false); st != 0 {
			t.Fatalf("inherited flock blocks target: %v", st)
		}
		if st := replica.Flock(ctx, inode, 300, syscall.F_WRLCK, false); st != syscall.EAGAIN {
			t.Fatalf("target replicas do not contend: %v", st)
		}
		if st := source.Flock(ctx, inode, 400, syscall.F_WRLCK, false); st != syscall.EAGAIN {
			t.Fatalf("target changed source lock: %v", st)
		}
	}
	if st := target.Setlk(ctx, 42, 201, false, syscall.F_WRLCK, 0, 99, 1); st != 0 {
		t.Fatalf("inherited plock blocks target: %v", st)
	}
	if st := replica.Setlk(ctx, 42, 301, false, syscall.F_WRLCK, 0, 99, 1); st != syscall.EAGAIN {
		t.Fatalf("target plocks do not contend: %v", st)
	}
	// Inspection sees only target owners; snapshot rows remain untouched.
	p, f, err := target.ListLocks(context.Background(), 42)
	if err != nil || len(p) != 1 || len(f) != 1 || f[0].Owner != 200 {
		t.Fatalf("target lock inspection: %v %v %v", p, f, err)
	}
	foreign, err := target.get(source.flockKey(42))
	if err != nil || len(foreign) == 0 {
		t.Fatalf("source snapshot lock was deleted: %v", err)
	}
}

func TestScopedLockSessionCleanup(t *testing.T) {
	c := &memKV{items: btree.New(2), temp: &kvItem{}}
	m := &kvMeta{baseMeta: newBaseMeta("", DefaultConf()), client: c, lockNamespace: "target"}
	m.en = m
	m.fmt = &Format{MetaVersion: 2}
	m.sid = 7
	if err := m.doNewSession([]byte(`{}`), false); err != nil {
		t.Fatal(err)
	}
	if st := m.Flock(Background(), 42, 100, syscall.F_WRLCK, false); st != 0 {
		t.Fatal(st)
	}
	if st := m.Setlk(Background(), 42, 101, false, syscall.F_WRLCK, 0, 99, 1); st != 0 {
		t.Fatal(st)
	}
	// A snapshot can carry a source lock with a colliding recorded session ID.
	foreign := m.fmtKey("F2/source/", Ino(42))
	value := marshalFlock(map[lockOwner]byte{{7, 100}: 'W'})
	if err := m.setValue(foreign, value); err != nil {
		t.Fatal(err)
	}
	session, err := m.GetSession(7, true)
	if err != nil || len(session.Flocks) != 1 || session.Flocks[0].Inode != 42 || len(session.Plocks) != 1 || session.Plocks[0].Inode != 42 {
		t.Fatalf("session details: %+v %v", session, err)
	}
	if err := m.doCleanStaleSession(7); err != nil {
		t.Fatal(err)
	}
	p, f, err := m.ListLocks(context.Background(), 42)
	if err != nil || len(p) != 0 || len(f) != 0 {
		t.Fatalf("local locks not cleaned: %v %v %v", p, f, err)
	}
	remaining, err := m.get(foreign)
	if err != nil || !bytes.Equal(remaining, value) {
		t.Fatalf("cleanup changed inherited scope: %v", err)
	}
}

func TestLegacyLockKeysRemainCompatible(t *testing.T) {
	m := &kvMeta{baseMeta: newBaseMeta("", DefaultConf()), lockNamespace: "physical"}
	m.fmt = &Format{MetaVersion: 1}
	if !bytes.Equal(m.flockKey(42), m.fmtKey("F", Ino(42))) || !bytes.Equal(m.plockKey(42), m.fmtKey("P", Ino(42))) {
		t.Fatal("legacy lock encoding changed")
	}
}

// Refresh replaces the format pointer while ordinary lock RPCs are admitted.
func TestScopedLocksDuringFormatRefresh(t *testing.T) {
	c := &memKV{items: btree.New(2), temp: &kvItem{}}
	c.set("setting", []byte(`{"Name":"refresh","MetaVersion":2}`))
	m := &kvMeta{baseMeta: newBaseMeta("", DefaultConf()), client: c, lockNamespace: "physical"}
	m.en = m
	if _, err := m.Load(false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			if _, err := m.Load(false); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		if st := m.Flock(Background(), RootInode, 77, syscall.F_RDLCK, false); st != 0 {
			t.Error(st)
			break
		}
	}
	wg.Wait()
}
