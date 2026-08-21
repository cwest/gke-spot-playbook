// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package clientgo

import (
	"strings"
	"testing"
)

func TestDecodeComputeClassMetadataNotAMap(t *testing.T) {
	yaml := []byte("metadata: 123\nname: batch-cpu")
	_, _, err := decodeComputeClass(yaml)
	if err == nil {
		t.Fatal("expected error when metadata is not a map")
	}
	if !strings.Contains(err.Error(), "is not a map") {
		t.Errorf("error = %q, want it to contain 'is not a map'", err)
	}
}

func TestDecodeComputeClassNameNotAString(t *testing.T) {
	yaml := []byte("metadata:\n  name: 123")
	_, _, err := decodeComputeClass(yaml)
	if err == nil {
		t.Fatal("expected error when name is not a string")
	}
	if !strings.Contains(err.Error(), "is not a string") {
		t.Errorf("error = %q, want it to contain 'is not a string'", err)
	}
}

func TestDecodeComputeClassNameEmpty(t *testing.T) {
	yaml := []byte("metadata:\n  name: \"\"")
	_, _, err := decodeComputeClass(yaml)
	if err == nil {
		t.Fatal("expected error when name is empty")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to mention empty", err)
	}
}
