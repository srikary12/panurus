/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sherdlock

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LFDT-Panurus/panurus/token"
	"github.com/LFDT-Panurus/panurus/token/services/utils/types/transaction"
	token2 "github.com/LFDT-Panurus/panurus/token/token"
	"github.com/hyperledger-labs/fabric-smart-client/pkg/utils/errors"
)

// boundedLocker wraps any Locker and enforces a per-transaction lock-count
// ceiling. When the limit is reached, Lock returns an error wrapping
// token.SelectorRateLimited so the selector aborts fast.
type boundedLocker struct {
	Locker
	maxLocksPerTx int
	counts        sync.Map // transaction.ID -> *atomic.Int64
}

// NewBoundedLocker returns a Locker that rejects Lock calls for a given
// consumerTxID once it already holds maxLocksPerTx locks.
// When maxLocksPerTx <= 0 the inner locker is returned unchanged (no wrapping).
func NewBoundedLocker(inner Locker, maxLocksPerTx int) Locker {
	if maxLocksPerTx <= 0 {
		return inner
	}

	return &boundedLocker{Locker: inner, maxLocksPerTx: maxLocksPerTx}
}

func (b *boundedLocker) Lock(ctx context.Context, tokenID *token2.ID, consumerTxID transaction.ID, walletID string) error {
	c := b.counter(consumerTxID)
	limit := int64(b.maxLocksPerTx)
	// Atomically reserve a slot before touching the store. If the CAS fails
	// another goroutine incremented between our Load and Add; retry until the
	// slot is ours or we are over the limit.
	for {
		current := c.Load()
		if current >= limit {
			return errors.Wrapf(token.SelectorRateLimited,
				"lock limit exceeded: transaction %s already holds %d locks (max: %d)",
				consumerTxID, current, b.maxLocksPerTx)
		}
		if c.CompareAndSwap(current, current+1) {
			break
		}
	}
	if err := b.Locker.Lock(ctx, tokenID, consumerTxID, walletID); err != nil {
		// Roll back the reservation on failure.
		c.Add(-1)

		return err
	}

	return nil
}

func (b *boundedLocker) UnlockByTxID(ctx context.Context, consumerTxID transaction.ID) error {
	b.counts.Delete(consumerTxID)

	return b.Locker.UnlockByTxID(ctx, consumerTxID)
}

func (b *boundedLocker) Cleanup(ctx context.Context, leaseExpiry time.Duration) error {
	// The Cleanup path removes expired locks from the store; we clear all
	// in-memory counters because we cannot know which txIDs were cleaned.
	b.counts.Clear()

	return b.Locker.Cleanup(ctx, leaseExpiry)
}

func (b *boundedLocker) counter(txID transaction.ID) *atomic.Int64 {
	v, _ := b.counts.LoadOrStore(txID, new(atomic.Int64))

	return v.(*atomic.Int64) //nolint:forcetypeassert
}
