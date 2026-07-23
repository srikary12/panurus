/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package guard

import (
	"github.com/hyperledger-labs/fabric-smart-client/pkg/utils/errors"
	"github.com/hyperledger-labs/fabric-smart-client/platform/common/utils/collections/iterators"
)

// LimitIterator wraps a streaming iterator so that reading more than max items
// fails instead of materialising an unbounded result set. It returns an error
// on the read that would exceed max (rather than silently truncating), so a
// caller that legitimately needs the full set learns it must paginate.
//
// max <= 0 disables the cap, and a nil iterator is returned unchanged, so the
// wrapper is safe to apply unconditionally. op names the read for the error.
func LimitIterator[A any](it iterators.Iterator[*A], max int, op string) iterators.Iterator[*A] {
	if it == nil || max <= 0 {
		return it
	}

	return &limitedIterator[A]{Iterator: it, max: max, op: op}
}

type limitedIterator[A any] struct {
	iterators.Iterator[*A]
	max   int
	count int
	op    string
}

func (l *limitedIterator[A]) Next() (*A, error) {
	next, err := l.Iterator.Next()
	if err != nil {
		return nil, err
	}
	if next == nil {
		// exhausted
		return nil, nil
	}
	l.count++
	if l.count > l.max {
		return nil, errors.Errorf("%s exceeded maximum of %d rows", l.op, l.max)
	}

	return next, nil
}
