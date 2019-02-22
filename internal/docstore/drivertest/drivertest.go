// Copyright 2019 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package drivertest provides a conformance test for implementations of
// driver.
package drivertest // import "gocloud.dev/internal/docstore/drivertest"

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	ds "gocloud.dev/internal/docstore"
	"gocloud.dev/internal/docstore/driver"
)

// Harness descibes the functionality test harnesses must provide to run
// conformance tests.
type Harness interface {
	// MakeCollection makes a driver.Collection for testing.
	MakeCollection(context.Context) (driver.Collection, error)

	// Close closes resources used by the harness.
	Close()
}

// HarnessMaker describes functions that construct a harness for running tests.
// It is called exactly once per test; Harness.Close() will be called when the test is complete.
type HarnessMaker func(ctx context.Context, t *testing.T) (Harness, error)

// RunConformanceTests runs conformance tests for provider implementations of docstore.
func RunConformanceTests(t *testing.T, newHarness HarnessMaker) {
	t.Run("Create", func(t *testing.T) { withCollection(t, newHarness, testCreate) })
	t.Run("Put", func(t *testing.T) { withCollection(t, newHarness, testPut) })
	t.Run("Replace", func(t *testing.T) { withCollection(t, newHarness, testReplace) })
	t.Run("Get", func(t *testing.T) { withCollection(t, newHarness, testGet) })
	t.Run("Delete", func(t *testing.T) { withCollection(t, newHarness, testDelete) })
	t.Run("Update", func(t *testing.T) { withCollection(t, newHarness, testUpdate) })
}

const KeyField = "_id"

func withCollection(t *testing.T, newHarness HarnessMaker, f func(*testing.T, *ds.Collection)) {
	ctx := context.Background()
	h, err := newHarness(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	dc, err := h.MakeCollection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coll := ds.NewCollection(dc)
	f(t, coll)
}

type docmap = map[string]interface{}

var nonexistentDoc = docmap{KeyField: "doesNotExist"}

func testCreate(t *testing.T, coll *ds.Collection) {
	ctx := context.Background()
	named := docmap{KeyField: "testCreate1", "b": true}
	unnamed := docmap{"b": false}
	// Attempt to clean up
	defer func() {
		_, _ = coll.Actions().Delete(named).Delete(unnamed).Do(ctx)
	}()

	createThenGet := func(doc docmap) {
		t.Helper()
		if err := coll.Create(ctx, doc); err != nil {
			t.Fatal(err)
		}
		got := docmap{KeyField: doc[KeyField]}
		if err := coll.Get(ctx, got); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(got, doc); diff != "" {
			t.Fatalf(diff)
		}
	}

	createThenGet(named)
	createThenGet(unnamed)

	// Can't create an existing doc.
	if err := coll.Create(ctx, named); err == nil {
		t.Error("got nil, want error")
	}
}

func testPut(t *testing.T, coll *ds.Collection) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	named := docmap{KeyField: "testPut1", "b": true}
	// Create a new doc.
	must(coll.Put(ctx, named))
	got := docmap{KeyField: named[KeyField]}
	must(coll.Get(ctx, got))
	if diff := cmp.Diff(got, named); diff != "" {
		t.Fatalf(diff)
	}

	// Replace an existing doc.
	named["b"] = false
	must(coll.Put(ctx, named))
	must(coll.Get(ctx, got))
	if diff := cmp.Diff(got, named); diff != "" {
		t.Fatalf(diff)
	}
}

func testReplace(t *testing.T, coll *ds.Collection) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	doc1 := docmap{KeyField: "testReplace", "s": "a"}
	must(coll.Put(ctx, doc1))
	doc1["s"] = "b"
	must(coll.Replace(ctx, doc1))
	got := docmap{KeyField: doc1[KeyField]}
	must(coll.Get(ctx, got))
	if diff := cmp.Diff(got, doc1); diff != "" {
		t.Fatalf(diff)
	}
	// Can't replace a nonexistent doc.
	if err := coll.Replace(ctx, nonexistentDoc); err == nil {
		t.Fatal("got nil, want error")
	}
}

func testGet(t *testing.T, coll *ds.Collection) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	doc := docmap{
		KeyField: "testGet1",
		"s":      "a string",
		"i":      int64(95),
		"f":      32.3,
	}
	must(coll.Put(ctx, doc))
	// If only the key fields are present, the full document is populated.
	got := docmap{KeyField: doc[KeyField]}
	must(coll.Get(ctx, got))
	if diff := cmp.Diff(got, doc); diff != "" {
		t.Error(diff)
	}
	// TODO(jba): test with field paths
}

func testDelete(t *testing.T, coll *ds.Collection) {
	ctx := context.Background()
	doc := docmap{KeyField: "testDelete"}
	if _, err := coll.Actions().Put(doc).Delete(doc).Do(ctx); err != nil {
		t.Fatal(err)
	}
	// The document should no longer exist.
	if err := coll.Get(ctx, doc); err == nil {
		t.Error("want error, got nil")
	}
	// Delete doesn't fail if the doc doesn't exist.
	if err := coll.Delete(ctx, nonexistentDoc); err != nil {
		t.Fatal(err)
	}
}

func testUpdate(t *testing.T, coll *ds.Collection) {
	ctx := context.Background()
	doc := docmap{KeyField: "testUpdate", "a": "A", "b": "B"}
	if err := coll.Put(ctx, doc); err != nil {
		t.Fatal(err)
	}

	got := docmap{KeyField: doc[KeyField]}
	_, err := coll.Actions().Update(doc, ds.Mods{
		"a": "X",
		"b": nil,
		"c": "C",
	}).Get(got).Do(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := docmap{
		KeyField: doc[KeyField],
		"a":      "X",
		"c":      "C",
	}
	if !cmp.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Can't update a nonexistent doc
	if err := coll.Update(ctx, nonexistentDoc, ds.Mods{}); err == nil {
		t.Error("got nil, want error")
	}
}
