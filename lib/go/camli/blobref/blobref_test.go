/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package blobref

import (
	"json"
	"testing"
	. "camli/test/asserts"
)

func TestAll(t *testing.T) {
	refStr := "sha1-0beec7b5ea3f0fdbc95d0dd47f3c5bc275da8a33"
	br := Parse(refStr)
	if br == nil {
		t.Fatalf("Failed to parse blobref")
	}
	if br.hashName != "sha1" {
		t.Errorf("Expected sha1 hashName")
	}
	if br.digest != "0beec7b5ea3f0fdbc95d0dd47f3c5bc275da8a33" {
		t.Errorf("Invalid digest")
	}
	Expect(t, br.IsSupported(), "sha1 should be supported")
	ExpectString(t, refStr, br.String(), "String() value")

	hash := br.Hash()
	hash.Write([]byte("foo"))
	if !br.HashMatches(hash) {
		t.Errorf("Expected hash of bytes 'foo' to match")
	}
	hash.Write([]byte("bogusextra"))
	if br.HashMatches(hash) {
		t.Errorf("Unexpected hash match with bogus extra bytes")
	}
}

func TestNotSupported(t *testing.T) {
	br := Parse("unknownfunc-0beec7b5ea3f0fdbc95d0dd47f3c5bc275da8a33")
	if br == nil {
		t.Fatalf("Failed to parse blobref")
	}
	if br.IsSupported() {
		t.Fatalf("Unexpected IsSupported() on unknownfunc")
	}
}

func TestSum32(t *testing.T) {
	refStr := "sha1-0000000000000000000000000000000000000012"
	br := Parse(refStr)
	if br == nil {
		t.Fatalf("Failed to parse blobref")
	}
	h32 := br.Sum32()
	if h32 != 18 {
		t.Errorf("got %d, want 18", h32)
	}
}

type Foo struct {
	B *BlobRef "foo"
}

func TestJsonUnmarshal(t *testing.T) {
	var f Foo
	if err := json.Unmarshal([]byte(`{"foo": "abc-def123", "other": 123}`), &f); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if f.B == nil {
		t.Fatal("blobref is nil")
	}
	if g, e := f.B.String(), "abc-def123"; g != e {
		t.Errorf("got %q, want %q", g, e)
	}
}

func TestJsonMarshal(t *testing.T) {
	f := &Foo{B: MustParse("def-1234abc")}
	bs, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if g, e := string(bs), `{"foo":"def-1234abc"}`; g != e {
		t.Errorf("got %q, want %q", g, e)
	}
}
