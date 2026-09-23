//go:build !notikv

package meta

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/oracle"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/tikvrpc"
)

// This needs a dedicated API V2 TiKV/CSE fixture: the probe advances the
// store-wide max_ts, modeling a read from an independently allocated TSO group.
// Never point JUICEFS_TEST_TIKV_API2_PD at a shared cluster.
func TestTiKVKeyspaceReadAfterWriteWithTSOSkew(t *testing.T) {
	endpoint := os.Getenv("JUICEFS_TEST_TIKV_API2_PD")
	if endpoint == "" {
		t.Skip("set JUICEFS_TEST_TIKV_API2_PD to a dedicated API V2 fixture")
	}
	re := require.New(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("jfs-visibility-%d", time.Now().UnixNano())
	client, err := newTikvClient(endpoint + "/" + prefix + "?keyspace=DEFAULT&gc-interval=0")
	re.NoError(err)
	t.Cleanup(func() { re.NoError(client.close()) })
	pc := client.(*prefixClient)
	c := pc.tkvClient.(*tikvClient)
	parent, child := []byte("parent"), []byte("child")
	var ahead uint64
	t.Cleanup(func() {
		// Let the injected timestamp pass before cleanup, including on the
		// unfixed implementation whose writes committed in the future.
		re.Eventually(func() bool {
			now, err := c.client.CurrentTimestamp(oracle.GlobalTxnScope)
			return err == nil && now > ahead
		}, 3*time.Second, 10*time.Millisecond)
		// Delete only this test's keys, without a scan or range deletion.
		err := client.txn(ctx, func(tx *kvTxn) error {
			tx.delete(parent)
			tx.delete(child)
			return nil
		}, 0)
		if err != nil {
			t.Errorf("cleanup metadata: %v", err)
		}
	})

	// Resolve the region before injecting skew, so discovery does not consume
	// the skew window. A direct RPC represents another group's valid read;
	// the local client's timestamp validation must not wait for its own TSO.
	probe := append(append([]byte(nil), pc.prefix...), []byte("other-group-read")...)
	bo := tikv.NewBackoffer(ctx, 1000)
	loc, err := c.client.GetRegionCache().LocateKey(bo, probe)
	re.NoError(err)
	ts, err := c.client.CurrentTimestamp(oracle.GlobalTxnScope)
	re.NoError(err)
	ahead = ts + oracle.ComposeTS(500, 0)
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
		tx.set(parent, []byte("directory"))
		return nil
	}, 0))
	// Point lookups use latest reads. This can succeed even when a later
	// transactional BatchGet cannot see the directory (db9-server #4752).
	re.NoError(client.simpleTxn(ctx, func(tx *kvTxn) error {
		if !bytes.Equal(tx.get(parent), []byte("directory")) {
			return fmt.Errorf("latest lookup lost committed directory")
		}
		return nil
	}, 0))
	re.NoError(client.txn(ctx, func(tx *kvTxn) error {
		snapshot := tx.kvtxn.(*prefixTxn).kvTxn.kvtxn.(*tikvTxn).KVTxn.StartTS()
		if snapshot >= ahead {
			return fmt.Errorf("fixture consumed skew window: snapshot=%d injected=%d", snapshot, ahead)
		}
		if !bytes.Equal(tx.gets(parent, child)[0], []byte("directory")) {
			return fmt.Errorf("committed parent missing: commit=%d next_snapshot=%d injected=%d",
				committed.CommitTS(), snapshot, ahead)
		}
		tx.set(child, []byte("file"))
		return nil
	}, 0))
}
