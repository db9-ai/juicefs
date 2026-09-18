//go:build !notikv

package meta

import (
	"context"
	"sync"
	"testing"

	"github.com/tikv/client-go/v2/testutils"
	"github.com/tikv/client-go/v2/tikv"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/tikvrpc/interceptor"
)

func TestTiKVTransactionPropagatesRPCObserver(t *testing.T) {
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
	c := &tikvClient{client: store}
	var mu sync.Mutex
	seen := make(map[tikvrpc.CmdType]int)
	ctx := interceptor.WithRPCInterceptor(context.Background(), func(next interceptor.RPCInterceptorFunc) interceptor.RPCInterceptorFunc {
		return func(target string, req *tikvrpc.Request) (*tikvrpc.Response, error) {
			mu.Lock()
			seen[req.Type]++
			mu.Unlock()
			return next(target, req)
		}
	})
	if err := c.txn(ctx, func(tx *kvTxn) error {
		if values := tx.gets([]byte("parent"), []byte("child")); values[0] != nil || values[1] != nil {
			t.Fatalf("unexpected existing values: %v", values)
		}
		tx.set([]byte("parent"), []byte("directory"))
		return nil
	}, 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[tikvrpc.CmdBatchGet] != 1 || seen[tikvrpc.CmdPrewrite] != 1 {
		t.Fatalf("observer missed read or commit: %v", seen)
	}
}
