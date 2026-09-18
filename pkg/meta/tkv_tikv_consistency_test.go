//go:build !notikv

package meta

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/tikv/client-go/v2/oracle"
	"github.com/tikv/client-go/v2/testutils"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/tikvrpc/interceptor"
)

// Freeze physical time while allocating strictly increasing logical timestamps.
// The regression must not disappear if the test runs slowly enough for real
// wall time to catch up with another keyspace group's clock.
type metadataTestOracle struct {
	oracle.Oracle
	next atomic.Uint64
}

func (o *metadataTestOracle) GetTimestamp(context.Context, *oracle.Option) (uint64, error) {
	return o.next.Add(1), nil
}

func TestTiKVCommittedParentVisibleToNextTransaction(t *testing.T) {
	client, cluster, pdClient, err := testutils.NewMockTiKV("", nil)
	if err != nil {
		t.Fatal(err)
	}
	testutils.BootstrapWithSingleStore(cluster)
	store, err := tikv.NewTestTiKVStore(client, pdClient, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	baseTS := oracle.GoTimeToTS(time.Now())
	tso := &metadataTestOracle{Oracle: store.GetOracle()}
	tso.next.Store(baseTS)
	store.SetOracle(tso)
	c := &tikvClient{client: store}
	var parentCommitTS uint64
	ctx := interceptor.WithRPCInterceptor(context.Background(), func(next interceptor.RPCInterceptorFunc) interceptor.RPCInterceptorFunc {
		return func(target string, req *tikvrpc.Request) (*tikvrpc.Response, error) {
			resp, err := next(target, req)
			if err != nil {
				return resp, err
			}
			if req.Type == tikvrpc.CmdCommit {
				parentCommitTS = req.Commit().CommitVersion
			}
			if req.Type != tikvrpc.CmdPrewrite || !req.Prewrite().TryOnePc {
				return resp, nil
			}
			prewrite := resp.Resp.(*kvrpcpb.PrewriteResponse)
			if prewrite.RegionError != nil || len(prewrite.Errors) != 0 {
				return resp, nil
			}
			// Model deployed CSE's store-wide max_ts: another TSO group has
			// read 10 ms ahead, so 1PC commits beyond this client's next TSO.
			// The mock store supplies real MVCC reads; only the server-side
			// selection of a 1PC commit timestamp is supplied here.
			parentCommitTS = baseTS + oracle.ComposeTS(10, 0)
			var keys [][]byte
			for _, mutation := range req.Prewrite().Mutations {
				keys = append(keys, mutation.Key)
			}
			commitResp, err := next(target, tikvrpc.NewRequest(tikvrpc.CmdCommit, &kvrpcpb.CommitRequest{
				Keys: keys, StartVersion: req.Prewrite().StartVersion, CommitVersion: parentCommitTS,
			}, req.Context))
			if err != nil {
				return nil, err
			}
			commit := commitResp.Resp.(*kvrpcpb.CommitResponse)
			if commit.RegionError != nil || commit.Error != nil {
				return nil, fmt.Errorf("mock 1PC commit failed: %v", commit)
			}
			prewrite.OnePcCommitTs = parentCommitTS
			return resp, nil
		}
	})
	if err := c.txn(ctx, func(tx *kvTxn) error {
		tx.set([]byte("parent-inode"), []byte("directory"))
		return nil
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.simpleTxn(ctx, func(tx *kvTxn) error {
		if string(tx.get([]byte("parent-inode"))) != "directory" {
			return fmt.Errorf("latest read missed committed parent")
		}
		return nil
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.txn(ctx, func(tx *kvTxn) error {
		values := tx.gets([]byte("parent-inode"), []byte("child-entry"))
		if string(values[0]) != "directory" {
			return fmt.Errorf("next transaction missed committed parent: commitTS=%d, nextTS=%d", parentCommitTS, tso.next.Load())
		}
		if values[1] != nil {
			return fmt.Errorf("unexpected existing child: %q", values[1])
		}
		return nil
	}, 0); err != nil {
		t.Fatal(err)
	}
}
