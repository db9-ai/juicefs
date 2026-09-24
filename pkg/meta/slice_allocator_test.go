package meta

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

func TestSharedSliceAllocator(t *testing.T) {
	a, err := OpenSliceAllocator("memkv://allocator")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	family := fmt.Sprintf("%064x", 123)
	if _, err := a.Reserve(Background(), family, 4096); err == nil {
		t.Fatal("missing record silently created")
	}
	if err := a.Initialize(Background(), family, 1); err != nil {
		t.Fatal(err)
	}
	starts := make(chan uint64, 32)
	errors := make(chan error, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start, err := a.Reserve(Background(), family, 4096)
			starts <- start
			errors <- err
		}()
	}
	wg.Wait()
	close(starts)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint64]bool{}
	for start := range starts {
		if (start-1)%4096 != 0 || seen[start] {
			t.Fatalf("overlapping allocation: %d", start)
		}
		seen[start] = true
	}
	// Lost response: forget a committed block, then retry. Neither replaying
	// provisioning nor creating a new volume client may rewind this watermark.
	lost, err := a.Reserve(Background(), family, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Initialize(Background(), family, 1); err != nil {
		t.Fatal(err)
	}
	next, err := a.Reserve(Background(), family, 4096)
	if err != nil || next != lost+4096 {
		t.Fatalf("lost-response retry reused IDs: %d %v", next, err)
	}
	if err := a.Initialize(Background(), family, math.MaxInt64-4095); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reserve(Background(), family, 4096); err == nil {
		t.Fatal("overflow wrapped IDs")
	}
}

func TestSharedSliceAllocatorFormatGuard(t *testing.T) {
	if err := (&Format{MetaVersion: 2}).CheckVersion(); err == nil {
		t.Fatal("v2 without family identity accepted")
	}
	if err := (&Format{MetaVersion: 1, SliceAllocator: fmt.Sprintf("%064x", 1)}).CheckVersion(); err == nil {
		t.Fatal("legacy version hides allocator requirement")
	}
	conf := DefaultConf()
	conf.ReadOnly = true
	m, err := newKVMeta("memkv", "readonly", conf)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	var id uint64
	if st := m.NewSlice(Background(), &id); st == 0 {
		t.Fatal("read-only metadata allocated a slice")
	}
}
