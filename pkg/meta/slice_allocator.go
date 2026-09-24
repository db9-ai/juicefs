package meta

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"strings"
)

// GlobalSliceAllocatorID names the deployment-wide sequence for high-bit slice
// IDs. Provision it once outside tenant snapshots; never recreate a missing
// sequence during mount or clone preparation.
const GlobalSliceAllocatorID = "a622e127cd50007f6c0a460b9ecc934b986d3ef2c916a2ee1704959bd67830a3"

// SliceAllocator reserves never-reused IDs outside cloneable volume metadata.
// Its keyspace must be excluded from tenant snapshot/restore operations. The
// caller owns Close; it may share one allocator among multiple metadata clients.
type SliceAllocator struct{ store *kvMeta }

// OpenSliceAllocator opens an existing control keyspace, without formatting it
// or creating any family records. Identity/state checks belong to the caller.
func OpenSliceAllocator(uri string) (*SliceAllocator, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid slice allocator URI")
	}
	if u.Scheme != "tikv" && u.Scheme != "memkv" {
		return nil, fmt.Errorf("unsupported slice allocator backend")
	}
	m, err := newKVMeta(u.Scheme, strings.TrimPrefix(uri, u.Scheme+"://"), DefaultConf())
	if err != nil {
		return nil, err
	}
	return &SliceAllocator{store: m.(*kvMeta)}, nil
}

// Close releases the control metadata client.
func (a *SliceAllocator) Close() error { return a.store.Shutdown() }

func sliceAllocatorKey(family string) ([]byte, error) {
	digest, err := hex.DecodeString(family)
	if err != nil || len(digest) != 32 {
		return nil, fmt.Errorf("invalid slice allocator family identity")
	}
	return append([]byte("fs9/slice-allocator/v1/"), digest...), nil
}

// Initialize is an explicit provisioning/offline-migration operation. next is
// the first unused ID, proven above every historical allocation. Replays may
// advance the high watermark, never reset it. Reserve never initializes records.
func (a *SliceAllocator) Initialize(ctx Context, family string, next uint64) error {
	key, err := sliceAllocatorKey(family)
	if err != nil {
		return err
	}
	if next == 0 || next > math.MaxInt64 {
		return fmt.Errorf("invalid slice allocator high watermark")
	}
	return a.store.txn(ctx, func(tx *kvTxn) error {
		old := tx.get(key)
		if len(old) != 0 && len(old) != 8 {
			return fmt.Errorf("corrupt slice allocator record")
		}
		if len(old) == 8 {
			current := binary.BigEndian.Uint64(old)
			if current == 0 || current > math.MaxInt64 {
				return fmt.Errorf("corrupt slice allocator high watermark")
			}
			if current >= next {
				return nil
			}
		}
		value := make([]byte, 8)
		binary.BigEndian.PutUint64(value, next)
		tx.set(key, value)
		return nil
	})
}

// Reserve atomically reserves [start, start+count). count=0 only verifies and
// reads the current watermark. An ambiguous commit is discarded; retrying may
// waste IDs but cannot reuse them. The caller must not use a result with error.
func (a *SliceAllocator) Reserve(ctx Context, family string, count uint64) (start uint64, err error) {
	key, err := sliceAllocatorKey(family)
	if err != nil {
		return 0, err
	}
	err = a.store.txn(ctx, func(tx *kvTxn) error {
		value := tx.get(key)
		if len(value) != 8 {
			return fmt.Errorf("slice allocator record missing or corrupt; explicit recovery required")
		}
		start = binary.BigEndian.Uint64(value)
		if start == 0 || start > math.MaxInt64 || count > uint64(math.MaxInt64)-start {
			return fmt.Errorf("slice allocator exhausted or corrupt")
		}
		if count != 0 {
			value = make([]byte, 8)
			binary.BigEndian.PutUint64(value, start+count)
			tx.set(key, value)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return start, nil
}
