/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sherdlock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/LFDT-Panurus/panurus/token"
	"github.com/LFDT-Panurus/panurus/token/services/selector/sherdlock"
	"github.com/LFDT-Panurus/panurus/token/services/selector/sherdlock/mocks"
	token2 "github.com/LFDT-Panurus/panurus/token/token"
	"github.com/hyperledger-labs/fabric-smart-client/pkg/utils/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tokenID(txID string, idx uint64) *token2.ID {
	return &token2.ID{TxId: txID, Index: idx}
}

// newFakeLocker returns a FakeLocker configured to succeed on TryLock and Lock.
func newFakeLocker() *mocks.FakeLocker {
	l := &mocks.FakeLocker{}
	l.LockReturns(nil)
	l.UnlockByTxIDReturns(nil)
	l.CleanupReturns(nil)

	return l
}

func TestNewBoundedLocker_ZeroLimit_ReturnsInner(t *testing.T) {
	inner := newFakeLocker()
	got := sherdlock.NewBoundedLocker(inner, 0)
	assert.Equal(t, inner, got, "zero limit should return inner locker unchanged")
}

func TestNewBoundedLocker_NegativeLimit_ReturnsInner(t *testing.T) {
	inner := newFakeLocker()
	got := sherdlock.NewBoundedLocker(inner, -1)
	assert.Equal(t, inner, got, "negative limit should return inner locker unchanged")
}

func TestBoundedLocker_UnderLimit_Succeeds(t *testing.T) {
	inner := newFakeLocker()
	bl := sherdlock.NewBoundedLocker(inner, 3)

	ctx := t.Context()
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 0), "consumer1", "wallet1"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 2), "consumer1", "wallet1"))

	assert.Equal(t, 3, inner.LockCallCount())
}

func TestBoundedLocker_AtLimit_ReturnsRateLimited(t *testing.T) {
	inner := newFakeLocker()
	bl := sherdlock.NewBoundedLocker(inner, 2)

	ctx := t.Context()
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 0), "consumer1", "wallet1"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1"))

	err := bl.Lock(ctx, tokenID("tx1", 2), "consumer1", "wallet1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, token.SelectorRateLimited), "expected SelectorRateLimited, got: %v", err)
	// inner must not have been called for the 3rd lock
	assert.Equal(t, 2, inner.LockCallCount())
}

func TestBoundedLocker_LimitIsPerTx(t *testing.T) {
	inner := newFakeLocker()
	bl := sherdlock.NewBoundedLocker(inner, 2)

	ctx := t.Context()
	// Two different consumer transactions share the limit independently.
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 0), "consumer1", "wallet1"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx2", 0), "consumer2", "wallet2"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx2", 1), "consumer2", "wallet2"))
	assert.Equal(t, 4, inner.LockCallCount())
}

func TestBoundedLocker_UnlockByTxID_ResetsCounter(t *testing.T) {
	inner := newFakeLocker()
	bl := sherdlock.NewBoundedLocker(inner, 2)

	ctx := t.Context()
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 0), "consumer1", "wallet1"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1"))

	// limit reached
	err := bl.Lock(ctx, tokenID("tx1", 2), "consumer1", "wallet1")
	require.True(t, errors.Is(err, token.SelectorRateLimited))

	// unlock clears the counter for consumer1
	require.NoError(t, bl.UnlockByTxID(ctx, "consumer1"))
	assert.Equal(t, 1, inner.UnlockByTxIDCallCount())

	// can acquire locks again
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 0), "consumer1", "wallet1"))
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1"))
}

func TestBoundedLocker_Cleanup_ResetsAllCounters(t *testing.T) {
	inner := newFakeLocker()
	bl := sherdlock.NewBoundedLocker(inner, 1)

	ctx := t.Context()
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 0), "consumer1", "wallet1"))

	err := bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1")
	require.True(t, errors.Is(err, token.SelectorRateLimited))

	require.NoError(t, bl.Cleanup(ctx, time.Minute))
	assert.Equal(t, 1, inner.CleanupCallCount())

	// counter cleared — can lock again
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1"))
}

func TestBoundedLocker_InnerLockError_DoesNotIncrementCounter(t *testing.T) {
	inner := newFakeLocker()
	inner.LockReturnsOnCall(0, errors.New("db error"))
	bl := sherdlock.NewBoundedLocker(inner, 2)

	ctx := t.Context()
	err := bl.Lock(ctx, tokenID("tx1", 0), "consumer1", "wallet1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, token.SelectorRateLimited))

	// second attempt must still succeed (counter was not incremented)
	require.NoError(t, bl.Lock(ctx, tokenID("tx1", 1), "consumer1", "wallet1"))
}

func TestBoundedLocker_Concurrent(t *testing.T) {
	const limit = 5
	inner := newFakeLocker()
	bl := sherdlock.NewBoundedLocker(inner, limit)

	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idx := uint64(i) //nolint:gosec // i is in [0,19], conversion is safe
			errs[i] = bl.Lock(ctx, tokenID("tx1", idx), "consumer1", "wallet1")
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, e := range errs {
		if e == nil {
			successes++
		}
	}
	assert.Equal(t, limit, successes, "exactly limit locks should succeed")
}
