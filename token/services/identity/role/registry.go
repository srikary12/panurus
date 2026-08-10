/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package role

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/LFDT-Panurus/panurus/token/driver"
	idriver "github.com/LFDT-Panurus/panurus/token/services/identity/driver"
	"github.com/LFDT-Panurus/panurus/token/services/logging"
	"github.com/hyperledger-labs/fabric-smart-client/pkg/utils/errors"
)

//go:generate counterfeiter -o mock/wf.go -fake-name WalletFactory . WalletFactory
type WalletFactory interface {
	NewWallet(ctx context.Context, id idriver.WalletID, role idriver.IdentityRoleType, is IdentitySupport, info idriver.IdentityInfo) (driver.Wallet, error)
}

// WalletIDStatus classifies the outcome of resolving an identity to the wallet id
// bound to it. It exists so callers branch on an explicit, named state instead of
// re-deriving intent from the ambiguous "(string, error)" shape — where ("", nil),
// ("", err) and ("id", nil) each mean something different and the difference is easy
// to get wrong (see issue #2063).
type WalletIDStatus int

const (
	// WalletIDUnknown is the zero value and never returned by GetWalletID. It is the
	// guard against a WalletIDResolution constructed without going through GetWalletID
	// (a mock, or a future constructor that forgets to set Status): because callers act
	// on a fallthrough only when authoritative() is true — Bound or Unbound — this zero
	// value is treated as a lookup failure, not as a safe "no binding" that would create
	// a duplicate wallet.
	WalletIDUnknown WalletIDStatus = iota
	// WalletIDBound means storage holds a wallet id for the identity. WalletID is set.
	WalletIDBound
	// WalletIDUnbound means storage answered authoritatively that the identity has no
	// wallet binding. This is a definitive, successful miss: it is safe to fall through
	// to the next resolution step and, ultimately, to create a wallet.
	WalletIDUnbound
	// WalletIDFailed means the storage lookup itself failed (timeout, connection reset,
	// ...), so whether a binding exists is UNKNOWN. Err carries the cause. Callers MUST
	// NOT treat this as WalletIDUnbound: doing so lets a transient blip masquerade as an
	// unregistered identity and triggers the creation of a duplicate wallet.
	WalletIDFailed
)

// WalletIDResolution is the explicit result of resolving an identity to its bound
// wallet id. It is the single shared value every wallet-lookup fallback branches on,
// so the "not found" vs "could not check" distinction is decided once (in GetWalletID)
// rather than re-inferred at each call site.
type WalletIDResolution struct {
	// Status is the outcome of the lookup. Always inspect it via Bound/Unbound/Failed
	// before reading the other fields, and branch exhaustively: any status that is
	// neither Bound nor Unbound (Failed, or the WalletIDUnknown zero value) is not
	// authoritative and must abort the lookup rather than fall through to creation.
	Status WalletIDStatus
	// WalletID is the bound wallet id; meaningful only when Status is WalletIDBound.
	WalletID idriver.WalletID
	// Err is the underlying storage failure; set only when Status is WalletIDFailed.
	Err error
}

// Bound reports whether the identity has a wallet id bound to it.
func (r WalletIDResolution) Bound() bool { return r.Status == WalletIDBound }

// Unbound reports whether storage answered authoritatively that the identity has no
// wallet binding. This is the ONLY non-Bound state that a caller may act on by falling
// through to the next resolution step and, ultimately, wallet creation. Every other
// state — Failed, or the zero value produced by a WalletIDResolution built without
// going through GetWalletID — leaves the binding unknown and must abort the lookup.
func (r WalletIDResolution) Unbound() bool { return r.Status == WalletIDUnbound }

// Failed reports whether the storage lookup failed, leaving the binding unknown.
// A failed resolution must abort the enclosing lookup, never fall through to creation.
func (r WalletIDResolution) Failed() bool { return r.Status == WalletIDFailed }

// authoritative reports whether the resolution definitively answers whether a wallet is
// bound — i.e. it came back from GetWalletID as Bound or Unbound. Any other status
// (Failed, or the zero-value WalletIDUnknown of a resolution built outside GetWalletID)
// is non-authoritative: the binding is unknown and the caller MUST abort rather than
// fall through to wallet creation. This is the guard the WalletIDUnknown zero value was
// introduced to provide.
func (r WalletIDResolution) authoritative() bool { return r.Bound() || r.Unbound() }

// abortError returns the error a non-authoritative resolution must abort a wallet lookup
// with. It preserves the storage cause for a WalletIDFailed and synthesizes one for a
// zero-value / unknown resolution (whose Err is nil), so an unknown status can never
// collapse into a nil error — via errors.WithMessagef(nil, ...) returning nil — and be
// silently mistaken for a successful lookup. It must only be called once Bound and
// Unbound have been ruled out (i.e. authoritative() is false).
func (r WalletIDResolution) abortError(id driver.WalletLookupID) error {
	cause := r.Err
	if cause == nil {
		cause = errors.Errorf("non-authoritative wallet id resolution status [%d]", r.Status)
	}

	return errors.WithMessagef(cause, "failed to lookup wallet [%s]", id)
}

// Registry manages wallets whose long-term identities have a given role.
//
// Concurrency and invariants:
//   - The Wallets map MUST only be accessed while holding WalletMu. Use
//     WalletMu.RLock()/RUnlock() for short read-only access and WalletMu.Lock()/Unlock()
//     for modifications. Methods in this file follow the pattern of taking short
//     RLocks for map reads and never holding locks while calling out to external
//     services (identity provider, storage, wallet factory) to avoid blocking and
//     potential deadlocks.
type Registry struct {
	Logger  logging.Logger
	Role    idriver.Role
	Storage idriver.WalletStoreService

	WalletFactory WalletFactory
	WalletMu      sync.RWMutex
	Wallets       map[string]driver.Wallet
}

// NewRegistry returns a new registry for the passed parameters.
// A registry is bound to a given role, and it is persistent.
func NewRegistry(logger logging.Logger, role idriver.Role, storage idriver.WalletStoreService, walletFactory WalletFactory) *Registry {
	return &Registry{
		Logger:        logger,
		Role:          role,
		Storage:       storage,
		WalletFactory: walletFactory,
		Wallets:       map[string]driver.Wallet{},
	}
}

func (r *Registry) RegisterIdentity(ctx context.Context, config driver.IdentityConfiguration) error {
	r.Logger.DebugfContext(ctx, "register identity [%s:%s]", config.ID, config.URL)

	return r.Role.RegisterIdentity(ctx, config)
}

// Lookup searches the wallet corresponding to the passed id.
// If a wallet is found, Lookup returns the wallet and its identifier.
// If no wallet is found, Lookup returns the identity info and a potential wallet identifier for the passed id, if anything is found
//
// The lookup strategy is multi-step:
// 1. Ask the role provider to MapToIdentity (identity, walletID). If that errors, fall back to toViewIdentity/GetWalletID.
// 2. Check the in-memory cache (r.Wallets) for wallet entries. Map reads are protected by WalletMu.RLock for a short duration.
// 3. If cache misses, try to resolve identity -> wallet id using storage/role and finally call role.GetIdentityInfo for any discovered wallet identifiers.
//
// Note: Lookup only takes short RLocks for map reads and does not hold the lock while calling external services.
func (r *Registry) Lookup(ctx context.Context, id driver.WalletLookupID) (driver.Wallet, idriver.IdentityInfo, idriver.WalletID, error) {
	r.Logger.DebugfContext(ctx, "lookup wallet by [%T]", id)
	var walletIdentifiers []string

	ident, walletID, err := r.Role.MapToIdentity(ctx, id)
	if err != nil {
		r.Logger.Errorf("failed to map wallet [%T] to identity [%s], use a fallback strategy", id, err)
		fail := true
		// give it a second change
		passedIdentity, ok := toViewIdentity(id)
		if ok {
			r.Logger.DebugfContext(ctx, "lookup failed, check if there is a wallet for identity [%s]", passedIdentity)
			// is this identity registered
			res := r.GetWalletID(ctx, passedIdentity)
			if !res.authoritative() {
				// A storage failure — or a resolution that never went through GetWalletID —
				// leaves the binding unknown; it must not be treated as "not registered", or
				// a transient blip would fall through to wallet creation and duplicate state.
				return nil, nil, "", res.abortError(id)
			}
			if res.Bound() {
				r.Logger.DebugfContext(ctx, "lookup failed, there is a wallet for identity [%s]: [%s]", passedIdentity, res.WalletID)
				// we got a hit
				walletID = res.WalletID
				ident = passedIdentity
				fail = false
			}
		}
		if fail {
			return nil, nil, "", errors.WithMessagef(err, "failed to lookup wallet [%s]", id)
		}
	}
	r.Logger.DebugfContext(ctx, "looked-up identifier [%s:%s]", ident, logging.Prefix(walletID))
	wID := walletID
	// Short RLock while reading from the map cache. Do not hold while calling external services.
	r.WalletMu.RLock()
	walletEntry, ok := r.Wallets[wID]
	r.WalletMu.RUnlock()
	if ok {
		return walletEntry, nil, wID, nil
	}
	walletIdentifiers = append(walletIdentifiers, wID)

	// give it a second chance
	passedIdentity, ok := toViewIdentity(id)
	if ok {
		r.Logger.DebugfContext(ctx, "no wallet found, check if there is a wallet for identity [%s]", passedIdentity)
		// is this identity registered
		res := r.GetWalletID(ctx, passedIdentity)
		if !res.authoritative() {
			// A storage failure — or a resolution that never went through GetWalletID —
			// leaves the binding unknown; it must not be treated as "not registered", or
			// a transient blip would fall through to wallet creation and duplicate state.
			return nil, nil, "", res.abortError(id)
		}
		if res.Bound() {
			r.Logger.DebugfContext(ctx, "no wallet found, there is a wallet for identity [%s]: [%s]", passedIdentity, res.WalletID)
			// we got a hit
			r.WalletMu.RLock()
			walletEntry, ok = r.Wallets[res.WalletID]
			r.WalletMu.RUnlock()
			if ok {
				return walletEntry, nil, res.WalletID, nil
			}
			r.Logger.DebugfContext(ctx, "no wallet found, there is a wallet for identity [%s]: [%s] but it has not been recreated yet", passedIdentity, res.WalletID)
		}
		walletIdentifiers = append(walletIdentifiers, res.WalletID)
	}

	r.Logger.DebugfContext(ctx, "no wallet found for [%s] at [%s]", passedIdentity, logging.Prefix(wID))
	if len(ident) != 0 {
		res := r.GetWalletID(ctx, ident)
		r.Logger.DebugfContext(ctx, "wallet for identity [%s] -> [%s:%d]", ident, res.WalletID, res.Status)
		if !res.authoritative() {
			// A storage failure — or a resolution that never went through GetWalletID —
			// leaves the binding unknown; it must not be treated as "not registered", or
			// a transient blip would fall through to wallet creation and duplicate state.
			return nil, nil, "", res.abortError(id)
		}
		if res.Bound() {
			r.WalletMu.RLock()
			w, ok := r.Wallets[res.WalletID]
			r.WalletMu.RUnlock()
			if ok {
				r.Logger.DebugfContext(ctx, "found wallet [%s:%s:%s:%s]", ident, walletID, w.ID(), res.WalletID)

				return w, nil, res.WalletID, nil
			}
		}
		walletIdentifiers = append(walletIdentifiers, res.WalletID)
	}

	for _, walletIdentifier := range walletIdentifiers {
		if len(walletIdentifier) == 0 {
			continue
		}
		// give it a second chance
		var idInfo idriver.IdentityInfo
		idInfo, err = r.Role.GetIdentityInfo(ctx, walletIdentifier)
		if err == nil {
			r.Logger.DebugfContext(ctx, "identity info found at [%s]", logging.Prefix(walletIdentifier))

			return nil, idInfo, walletIdentifier, nil
		} else {
			r.Logger.DebugfContext(ctx, "identity info not found at [%s]", logging.Prefix(walletIdentifier))
		}
	}

	return nil, nil, "", errors.Errorf(
		"failed to get wallet info for [%s]",
		logging.Prefix(walletID),
	)
}

// RegisterWallet binds the passed wallet to the passed id
func (r *Registry) RegisterWallet(ctx context.Context, id string, w driver.Wallet) error {
	r.Logger.DebugfContext(ctx, "register wallet [%s]", id)
	// Protect writes to the Wallets map
	r.WalletMu.Lock()
	defer r.WalletMu.Unlock()
	r.Wallets[id] = w

	return nil
}

// BindIdentity binds the passed identity to the passed wallet identifier.
// Additional metadata can be bound to the identity. confID is the unique identifier
// of the IdentityConfiguration that originated the identity being bound
// (see driver.IdentityConfiguration.UniqueID).
func (r *Registry) BindIdentity(ctx context.Context, identity driver.Identity, eID string, wID idriver.WalletID, meta any, confID string) error {
	r.Logger.DebugfContext(ctx, "put recipient identity [%s]->[%s]", identity, wID)
	metaEncoded, err := json.Marshal(meta)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal metadata")
	}

	return r.Storage.StoreIdentity(ctx, identity, eID, wID, int(r.Role.ID()), metaEncoded, confID)
}

// ContainsIdentity returns true if the passed identity belongs to the passed wallet,
// false otherwise
func (r *Registry) ContainsIdentity(ctx context.Context, identity driver.Identity, wID string) bool {
	return r.Storage.IdentityExists(ctx, identity, wID, int(r.Role.ID()))
}

// WalletIDs returns the list of wallet identifiers
func (r *Registry) WalletIDs(ctx context.Context) ([]string, error) {
	walletIDs, err := r.Role.IdentityIDs()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get wallet identifiers from identity provider")
	}
	duplicates := map[string]bool{}
	for _, id := range walletIDs {
		duplicates[id] = true
	}

	ids, err := r.Storage.GetWalletIDs(ctx, int(r.Role.ID()))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get roles iterator")
	}
	for _, wID := range ids {
		_, found := duplicates[wID]
		if !found {
			walletIDs = append(walletIDs, wID)
			duplicates[wID] = true
		}
	}

	return walletIDs, nil
}

// GetIdentityMetadata loads metadata bound to the passed identity into the passed meta argument
func (r *Registry) GetIdentityMetadata(ctx context.Context, identity driver.Identity, wID string, meta any) error {
	r.Logger.DebugfContext(ctx, "get recipient identity metadata [%s]->[%s]", identity, wID)
	raw, err := r.Storage.LoadMeta(ctx, identity, wID, int(r.Role.ID()))
	if err != nil {
		return errors.WithMessagef(err, "failed to retrieve identity's metadata [%s]", identity)
	}

	return json.Unmarshal(raw, &meta)
}

// GetWalletID resolves the wallet identifier bound to the passed identity.
//
// It is the single point that translates the storage layer's (WalletID, error)
// convention into an explicit WalletIDResolution, so no caller has to re-derive the
// meaning of ("", nil) vs ("", err). The storage contract reports an unbound identity
// as ("", nil); a non-nil error is a genuine storage failure (timeout, connection
// reset, ...) whose result is therefore WalletIDFailed, never WalletIDUnbound. Keeping
// the two apart here is what prevents a transient blip from looking like an
// unregistered identity and triggering the creation of a duplicate wallet (issue #2063).
func (r *Registry) GetWalletID(ctx context.Context, identity driver.Identity) WalletIDResolution {
	wID, err := r.Storage.GetWalletID(ctx, identity, int(r.Role.ID()))
	if err != nil {
		return WalletIDResolution{
			Status: WalletIDFailed,
			Err:    errors.Wrapf(err, "failed to get wallet id for identity [%s]", identity),
		}
	}
	if len(wID) == 0 {
		r.Logger.DebugfContext(ctx, "no wallet bound to identity [%s]", identity)

		return WalletIDResolution{Status: WalletIDUnbound}
	}
	r.Logger.DebugfContext(ctx, "wallet [%s] is bound to identity [%s]", wID, identity)

	return WalletIDResolution{Status: WalletIDBound, WalletID: wID}
}

func (r *Registry) WalletByID(ctx context.Context, role idriver.IdentityRoleType, id driver.WalletLookupID) (driver.Wallet, error) {
	r.Logger.DebugfContext(ctx, "role [%d] lookup wallet by [%T]", role, id)
	defer r.Logger.DebugfContext(ctx, "role [%d] lookup wallet by [%T] done", role, id)

	r.Logger.DebugfContext(ctx, "is it in cache?")

	// First, do a fast-path check of the cache without taking a long lock.
	v, ok := id.(string)
	if ok {
		r.Logger.DebugfContext(ctx, "role [%d] lookup wallet by string [%s]", role, v)
		r.WalletMu.RLock()
		w := r.Wallets[v]
		r.WalletMu.RUnlock()
		if w != nil {
			r.Logger.DebugfContext(ctx, "role [%d] lookup wallet by string [%s], found.", role, v)

			return w, nil
		}
	}

	// Not in cache: do the lookup to get identity info and wallet id (no locks held across external calls)
	// Lookup itself takes short RLocks for map reads. We call Lookup without holding
	// the global mutex to avoid blocking other operations while doing external lookups.
	w, idInfo, wID, err := r.Lookup(ctx, id)
	if err != nil {
		r.Logger.DebugfContext(ctx, "failed with error [%+v]", err)

		return nil, errors.WithMessagef(err, "failed to lookup identity for owner wallet [%T]", id)
	}
	if w != nil {
		r.Logger.DebugfContext(ctx, "yes [%s:%s]", w.ID(), wID)

		return w, nil
	}
	r.Logger.DebugfContext(ctx, "no")

	// Register the newly created wallet but check if another goroutine already created it.
	r.WalletMu.Lock()
	defer r.WalletMu.Unlock()
	if existing, ok := r.Wallets[wID]; ok {
		// Another goroutine created and registered the wallet in the meantime; prefer it.
		return existing, nil
	}
	// Create the wallet without holding the registry lock (avoid holding locks while calling external code).
	r.Logger.DebugfContext(ctx, "create wallet [%s]", wID)
	newWallet, err := r.WalletFactory.NewWallet(ctx, wID, role, r, idInfo)
	if err != nil {
		return nil, err
	}
	r.Logger.DebugfContext(ctx, "register wallet [%s:%s] with label [%s]", newWallet.ID(), wID, wID)
	r.Wallets[wID] = newWallet

	return newWallet, nil
}

// closer is implemented by wallets that hold resources needing an explicit
// release, such as a background provisioning goroutine. It is declared here,
// on the consumer side, rather than widening driver.Wallet: only anonymous
// owner wallets have anything to release, and widening the port would force a
// no-op Close on every wallet type of every driver.
type closer interface {
	Close()
}

// Done releases all the resources allocated by this service. Wallets created by
// this registry that hold releasable resources are closed and the wallet cache is
// dropped, so their background goroutines terminate instead of living for the
// lifetime of the process.
func (r *Registry) Done() error {
	r.WalletMu.Lock()
	wallets := make([]driver.Wallet, 0, len(r.Wallets))
	for _, w := range r.Wallets {
		wallets = append(wallets, w)
	}
	r.Wallets = map[string]driver.Wallet{}
	r.WalletMu.Unlock()

	// Close outside the lock: never hold the registry mutex while calling out.
	for _, w := range wallets {
		if c, ok := w.(closer); ok {
			c.Close()
		}
	}

	return r.Role.Done()
}

func toViewIdentity(id driver.WalletLookupID) (driver.Identity, bool) {
	switch v := id.(type) {
	case driver.Identity:
		return v, true
	case []byte:
		return v, true
	default:
		return nil, false
	}
}
