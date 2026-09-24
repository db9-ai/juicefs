/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
)

func TestFixObjectSize(t *testing.T) {
	t.Run("Should make sure the size is in range", func(t *testing.T) {
		cases := []struct {
			input, expected uint64
		}{
			{30 << 10, 64 << 10},
			{0, 64 << 10},
			{2 << 40, 16 << 20},
			{16 << 21, 16 << 20},
		}
		for _, c := range cases {
			if size := fixObjectSize(c.input); size != c.expected {
				t.Fatalf("Expected %d, got %d", c.expected, size)
			}
		}
	})
	t.Run("Should use powers of two", func(t *testing.T) {
		cases := []struct {
			input, expected uint64
		}{
			{150 << 10, 128 << 10},
			{99 << 10, 64 << 10},
			{1077 << 10, 1024 << 10},
		}
		for _, c := range cases {
			if size := fixObjectSize(c.input); size != c.expected {
				t.Fatalf("Expected %d, got %d", c.expected, size)
			}
		}
	})
}

func TestFormat(t *testing.T) {
	rdb := resetTestMeta()
	if err := Main([]string{"", "format", "--bucket", t.TempDir(), testMeta, testVolume}); err != nil {
		t.Fatalf("format error: %s", err)
	}
	body, err := rdb.Get(context.Background(), "setting").Bytes()
	if err != nil {
		t.Fatalf("get setting: %s", err)
	}
	f := meta.Format{}
	if err = json.Unmarshal(body, &f); err != nil {
		t.Fatalf("json unmarshal: %s", err)
	}
	if f.Name != testVolume {
		t.Fatalf("volume name %s != expected %s", f.Name, testVolume)
	}

	if err = Main([]string{"", "format", testMeta, testVolume, "--capacity", "1", "--inodes", "1000"}); err != nil {
		t.Fatalf("format error: %s", err)
	}
	if body, err = rdb.Get(context.Background(), "setting").Bytes(); err != nil {
		t.Fatalf("get setting: %s", err)
	}
	if err = json.Unmarshal(body, &f); err != nil {
		t.Fatalf("json unmarshal: %s", err)
	}
	if f.Capacity != 1<<30 || f.Inodes != 1000 {
		t.Fatalf("unexpected volume: %+v", f)
	}
}

func TestFormatStandaloneAllocation(t *testing.T) {
	uri := "sqlite3://" + t.TempDir() + "/metadata.db"
	if err := Main([]string{"", "format", "--bucket", t.TempDir(), uri, "standalone"}); err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewClientWithError(uri, meta.DefaultConf())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	f, err := m.Load(true)
	if err != nil {
		t.Fatal(err)
	}
	if f.MetaVersion != 1 || f.SliceAllocator != "" {
		t.Fatalf("standalone format requires external allocator: %+v", f)
	}
	var id uint64
	if st := m.NewSlice(meta.Background(), &id); st != 0 || id == 0 {
		t.Fatalf("standalone allocation: id=%d errno=%v", id, st)
	}
}
