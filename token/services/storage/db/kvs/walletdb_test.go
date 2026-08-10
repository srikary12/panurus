/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package kvs

import (
	"testing"

	"github.com/LFDT-Panurus/panurus/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWalletStoreGetConfID asserts that GetConfID round-trips the confID passed to
// StoreIdentity for a given identity, regardless of which role it was bound under, and returns
// an empty string with no error for an identity that was never bound. This is the KVS read side
// that SignerRouter relies on to pin a signer to exactly one KeyManager without probing every
// KeyManager registered under the identity's type.
func TestWalletStoreGetConfID(t *testing.T) {
	backend, err := NewInMemory()
	require.NoError(t, err)
	tmsID := token.TMSID{Network: "apple", Channel: "pears", Namespace: "strawberries"}
	db := NewWalletStore(backend, tmsID)
	ctx := t.Context()

	// miss: never bound
	got, err := db.GetConfID(ctx, []byte("erin"))
	require.NoError(t, err)
	assert.Empty(t, got)

	const confID = "wallet-test-conf-id"

	// bound under role 0
	require.NoError(t, db.StoreIdentity(ctx, []byte("erin"), "eID", "erin_wallet", 0, nil, confID))
	got, err = db.GetConfID(ctx, []byte("erin"))
	require.NoError(t, err)
	assert.Equal(t, confID, got)

	// bound again under a different role: still resolves to the same confID
	require.NoError(t, db.StoreIdentity(ctx, []byte("erin"), "eID", "erin_wallet_2", 1, nil, confID))
	got, err = db.GetConfID(ctx, []byte("erin"))
	require.NoError(t, err)
	assert.Equal(t, confID, got)

	// a different identity never bound in this TMS still misses cleanly
	got, err = db.GetConfID(ctx, []byte("frank"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestWalletStoreGetWalletID asserts the not-found contract that the role Registry relies on to
// tell a transient storage error apart from "this identity has no binding": an unbound identity
// must resolve to ("", nil), and a bound identity must round-trip its wallet id. If GetWalletID
// returned an error for a missing key (as kvs.Get does), the registry would abort every lookup
// for a genuinely-unregistered identity instead of creating its wallet.
func TestWalletStoreGetWalletID(t *testing.T) {
	backend, err := NewInMemory()
	require.NoError(t, err)
	// NewInMemory shares a global in-memory backing store across the package's tests, so use a
	// tmsID and identities unique to this test to stay isolated from any other stored bindings.
	tmsID := token.TMSID{Network: "getwalletid", Channel: "getwalletid", Namespace: "getwalletid"}
	db := NewWalletStore(backend, tmsID)
	ctx := t.Context()

	// miss: never bound -> ("", nil), NOT an error
	got, err := db.GetWalletID(ctx, []byte("gwid-grace"), 0)
	require.NoError(t, err)
	assert.Empty(t, got)

	// bound under role 0 -> round-trips the wallet id
	require.NoError(t, db.StoreIdentity(ctx, []byte("gwid-grace"), "eID", "grace_wallet", 0, nil, "conf-1"))
	got, err = db.GetWalletID(ctx, []byte("gwid-grace"), 0)
	require.NoError(t, err)
	assert.Equal(t, "grace_wallet", got)

	// the same identity under a different role is still an independent miss
	got, err = db.GetWalletID(ctx, []byte("gwid-grace"), 1)
	require.NoError(t, err)
	assert.Empty(t, got)
}
