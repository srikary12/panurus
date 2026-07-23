/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package guard

import (
	"testing"

	"github.com/hyperledger-labs/fabric-smart-client/platform/common/utils/collections"
	"github.com/hyperledger-labs/fabric-smart-client/platform/common/utils/collections/iterators"
)

func sliceIterator(n int) iterators.Iterator[*int] {
	items := make([]*int, n)
	for i := range items {
		v := i
		items[i] = &v
	}

	return collections.NewSliceIterator(items)
}

func drain(it iterators.Iterator[*int]) (int, error) {
	count := 0
	for {
		v, err := it.Next()
		if err != nil {
			return count, err
		}
		if v == nil {
			return count, nil
		}
		count++
	}
}

func TestLimitIteratorWithinLimit(t *testing.T) {
	it := LimitIterator(sliceIterator(5), 10, "read")
	got, err := drain(it)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5 {
		t.Fatalf("expected 5 items, got %d", got)
	}
}

func TestLimitIteratorAtLimit(t *testing.T) {
	it := LimitIterator(sliceIterator(10), 10, "read")
	got, err := drain(it)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 10 {
		t.Fatalf("expected 10 items, got %d", got)
	}
}

func TestLimitIteratorOverLimit(t *testing.T) {
	it := LimitIterator(sliceIterator(11), 10, "read")
	_, err := drain(it)
	if err == nil {
		t.Fatalf("expected an error once the limit is exceeded")
	}
}

func TestLimitIteratorDisabled(t *testing.T) {
	// max <= 0 disables the cap: the wrapped iterator is returned unchanged.
	src := sliceIterator(3)
	if got := LimitIterator(src, 0, "read"); got != src {
		t.Fatalf("expected the source iterator to be returned unchanged when disabled")
	}
}
