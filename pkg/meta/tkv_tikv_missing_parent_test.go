//go:build !notikv

package meta

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/oracle"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/txnkv/transaction"
)

// Requires a disposable API V2 TiKV fixture: the injected read advances the
// store-wide max_ts. Never set this endpoint to a shared or production cluster.
func TestTiKVMknodConfirmedMissingParent(t *testing.T) {
	endpoint := os.Getenv("JUICEFS_TEST_TIKV_API2_PD")
	if endpoint == "" {
		t.Skip("set JUICEFS_TEST_TIKV_API2_PD to a dedicated API V2 fixture")
	}
	re := require.New(t)
	ctx := context.Background()
	client, err := newTikvClient(endpoint + fmt.Sprintf("/mknod-%d?keyspace=DEFAULT&gc-interval=0", time.Now().UnixNano()))
	re.NoError(err)
	t.Cleanup(func() {
		if err := client.reset(nil); err != nil {
			t.Errorf("clean up test prefix: %v", err)
		}
		if err := client.close(); err != nil {
			t.Errorf("close test client: %v", err)
		}
	})
	pc := client.(*prefixClient)
	c := pc.tkvClient.(*tikvClient)
	m := &kvMeta{baseMeta: newBaseMeta("mknod-integration", testConfig()), client: client}
	m.en = m
	m.fmt = testFormat()
	parent := m.inodeKey(RootInode)
	child := Ino(2)
	probe := append(append([]byte(nil), pc.prefix...), []byte("other-tso-read")...)
	bo := tikv.NewBackoffer(ctx, 1000)
	loc, err := c.client.GetRegionCache().LocateKey(bo, probe)
	re.NoError(err)
	ts, err := c.client.CurrentTimestamp(oracle.GlobalTxnScope)
	re.NoError(err)
	ahead := ts + oracle.ComposeTS(150, 0)
	resp, err := c.client.SendReq(bo, tikvrpc.NewRequest(tikvrpc.CmdGet,
		&kvrpcpb.GetRequest{Key: probe, Version: ahead}), loc.Region, time.Second)
	re.NoError(err)
	regionErr, err := resp.GetRegionError()
	re.NoError(err)
	re.Nil(regionErr)
	re.Nil(resp.Resp.(*kvrpcpb.GetResponse).GetError())

	var committed *tikv.KVTxn
	re.NoError(client.txn(ctx, func(tx *kvTxn) error {
		committed = tx.kvtxn.(*prefixTxn).kvTxn.kvtxn.(*tikvTxn).KVTxn
		tx.set(parent, m.marshal(&Attr{Typ: TypeDirectory, Mode: 0755, Parent: RootInode, Nlink: 2}))
		return nil
	}, 0))
	re.True((transaction.TxnProbe{KVTxn: committed}).GetCommitter().IsOnePC(), "the fixture must actually use 1PC")
	re.NoError(client.txn(ctx, func(tx *kvTxn) error {
		snapshot := tx.kvtxn.(*prefixTxn).kvTxn.kvtxn.(*tikvTxn).KVTxn.StartTS()
		re.Less(snapshot, committed.CommitTS(), "fixture must still be inside the visibility gap")
		re.Nil(tx.gets(parent)[0], "transactional read must reproduce the missing parent")
		return nil
	}, 0))
	re.NoError(client.simpleTxn(ctx, func(tx *kvTxn) error {
		re.NotNil(tx.get(parent), "latest point read must confirm the parent exists")
		return nil
	}, 0))

	attr := Attr{Typ: TypeDirectory, Parent: RootInode, Nlink: 2}
	re.Equal(syscall.Errno(0), m.doMknod(Background(), RootInode, "child", TypeDirectory, 0755, 0, "", &child, &attr))
	re.NoError(client.simpleTxn(ctx, func(tx *kvTxn) error {
		re.Equal(m.packEntry(TypeDirectory, child), tx.get(m.entryKey(RootInode, "child")))
		var parentAttr Attr
		m.parseAttr(tx.get(parent), &parentAttr)
		re.Equal(uint32(3), parentAttr.Nlink)
		return nil
	}, 0))
}
