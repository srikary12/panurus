/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package fabricx

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/LFDT-Panurus/panurus/integration/nwo/token/fabric"
	tokentopology "github.com/LFDT-Panurus/panurus/integration/nwo/token/topology"
	"github.com/LFDT-Panurus/panurus/integration/token/fungible/views/ppsetup"
	"github.com/LFDT-Panurus/panurus/token/services/logging"
	"github.com/hyperledger-labs/fabric-smart-client/integration/nwo/api"
	fabrictopology "github.com/hyperledger-labs/fabric-smart-client/integration/nwo/fabric/topology"
	"github.com/hyperledger-labs/fabric-smart-client/integration/nwo/fabricx"
	"github.com/hyperledger-labs/fabric-smart-client/pkg/utils/errors"
	"github.com/onsi/gomega"
)

const (
	// issuerClientID is the name of the FSC node whose view sets up the public parameters.
	issuerClientID = "issuer"
	// setupPublicParamsView is the view invoked on the issuer to set up the public parameters.
	setupPublicParamsView = "SetupPublicParams"
	// setupPublicParamsTimeout is the timeout passed to the SetupPublicParams view.
	setupPublicParamsTimeout = 2 * time.Minute
	// defaultClientRetries is how many times we look for a ready issuer client before giving up.
	defaultClientRetries = 60
	// defaultClientRetryDelay is how long we wait between two issuer client lookups.
	defaultClientRetryDelay = 1 * time.Second
	// defaultInstallDelay is how long the background installation waits before its first attempt,
	// giving the FSC nodes time to come up.
	defaultInstallDelay = 10 * time.Second
)

var logger = logging.MustGetLogger()

type ClientProvider interface {
	Client(string) api.GRPCClient
}

// Backend installs and updates the token public parameters of a fabricx network by
// invoking the SetupPublicParams view on the issuer FSC node.
//
// Neither InstallPublicParams nor UpdatePublicParams panics: both wait for the issuer
// client to become ready and report any failure as an error. InstallPublicParams does its
// work on a background goroutine, whose outcome is retrieved with WaitForPublicParams or
// PendingInstallError.
type Backend struct {
	ClientProvider ClientProvider

	// ClientRetries is how many times to look for a ready issuer client before giving up.
	// Zero means defaultClientRetries.
	ClientRetries int
	// ClientRetryDelay is how long to wait between two issuer client lookups.
	// Zero means defaultClientRetryDelay.
	ClientRetryDelay time.Duration
	// InstallDelay is how long InstallPublicParams waits before its first attempt.
	// Zero means defaultInstallDelay.
	InstallDelay time.Duration

	mutex    sync.Mutex
	installs map[string]*installTask
}

// installTask tracks the outcome of one background public-params installation. err is
// written before done is closed and only read after done is closed, so it needs no lock.
type installTask struct {
	done chan struct{}
	once sync.Once
	err  error
}

func newInstallTask() *installTask {
	return &installTask{done: make(chan struct{})}
}

// complete records the outcome of the installation and unblocks any waiter. Only the
// first call has an effect, so a panic recovered after an error was already recorded
// does not overwrite the original cause.
func (t *installTask) complete(err error) {
	t.once.Do(func() {
		t.err = err
		close(t.done)
	})
}

// result returns the recorded outcome. It must only be called once done is closed.
func (t *installTask) result() error {
	return t.err
}

func (b *Backend) PrepareNamespace(tms *tokentopology.TMS) {
	switch n := tms.BackendTopology.(type) {
	case *fabrictopology.Topology:
		orgs := fabric.GetOrgs(tms)
		gomega.Expect(orgs).ToNot(gomega.BeEmpty(), "missing orgs for tms [%s:%s:%s:%s:%s]", tms.Network, tms.Channel, tms.Namespace, tms.Driver, tms.Alias)

		addNamespace(n, tms, orgs...)
	case *fabricx.Topology:
		orgs := fabric.GetOrgs(tms)
		gomega.Expect(orgs).ToNot(gomega.BeEmpty(), "missing orgs for tms [%s:%s:%s:%s:%s]", tms.Network, tms.Channel, tms.Namespace, tms.Driver, tms.Alias)

		addNamespace(n.Topology, tms, orgs...)
	default:
		panic(fmt.Sprintf("unknown backend network type %T", n))
	}
}

// addNamespace deploys the token namespace with either the custom policy configured via
// fabric.WithNamespacePolicy, or the default unanimity policy over orgs when unset.
func addNamespace(n *fabrictopology.Topology, tms *tokentopology.TMS, orgs ...string) {
	policy := fabric.GetNamespacePolicy(tms)
	if len(policy) == 0 {
		n.AddNamespaceWithUnanimity(tms.Namespace, orgs...)

		return
	}

	var peers []string
	for _, org := range orgs {
		for _, peer := range n.Peers {
			if peer.Organization == org {
				peers = append(peers, peer.Name)
			}
		}
	}
	n.AddNamespace(tms.Namespace, policy, peers...)
}

// InstallPublicParams starts the installation of the public parameters of tms in the
// background and returns as soon as that work is scheduled. The installation waits for the
// issuer FSC node to become reachable, so it must not run synchronously during network
// bring-up.
//
// The returned error only reports a failure to schedule the work. The outcome of the
// installation itself is obtained with WaitForPublicParams or PendingInstallError; it is
// never raised as a panic on the background goroutine.
func (b *Backend) InstallPublicParams(tms *tokentopology.TMS, ppRaw []byte) error {
	if b.ClientProvider == nil {
		return errors.Errorf("no client provider available, cannot install public params on [%s]", tms.ID())
	}

	task := b.newInstallTask(tms)
	go func() {
		// nothing above this goroutine is watching for a panic, so turn one into a
		// recorded error instead of letting it take down the whole process
		defer func() {
			if r := recover(); r != nil {
				task.complete(errors.Errorf("panic while installing public params on [%s]: %v", tms.ID(), r))
			}
		}()

		task.complete(b.installPublicParams(tms, ppRaw))
	}()

	return nil
}

// installPublicParams performs the actual installation, reporting any failure as an error.
func (b *Backend) installPublicParams(tms *tokentopology.TMS, ppRaw []byte) error {
	time.Sleep(b.installDelay())

	logger.Infof("installing public params on [%s]...", tms.ID())
	if err := b.setupPublicParams(tms, ppRaw); err != nil {
		logger.Errorf("installing public params on [%s]...failed [%v]", tms.ID(), err)

		return err
	}
	logger.Infof("installing public params on [%s]...done", tms.ID())

	return nil
}

// UpdatePublicParams replaces the public parameters of tms by invoking the SetupPublicParams
// view on the issuer FSC node. It waits for the issuer client to become available, so calling
// it while the issuer node is still starting is a wait rather than a failure, and returns any
// failure as an error.
func (b *Backend) UpdatePublicParams(tms *tokentopology.TMS, ppRaw []byte) error {
	if b.ClientProvider == nil {
		return errors.Errorf("no client provider available, cannot update public params on [%s]", tms.ID())
	}

	logger.Infof("updating public params on [%s]...", tms.ID())
	if err := b.setupPublicParams(tms, ppRaw); err != nil {
		logger.Errorf("updating public params on [%s]...failed [%v]", tms.ID(), err)

		return err
	}
	logger.Infof("updating public params on [%s]...done", tms.ID())

	return nil
}

// WaitForPublicParams blocks until the background installation started by
// InstallPublicParams for tms has finished, and returns its outcome. It returns an error if
// the installation did not finish within timeout, and nil if no installation was ever
// started for tms. It can be called repeatedly and always reports the same outcome.
func (b *Backend) WaitForPublicParams(tms *tokentopology.TMS, timeout time.Duration) error {
	task := b.installTaskFor(tms)
	if task == nil {
		return nil
	}

	select {
	case <-task.done:
		return task.result()
	case <-time.After(timeout):
		return errors.Errorf("timeout waiting for the installation of the public params on [%s]", tms.ID())
	}
}

// PendingInstallError returns the failure recorded by the background installation of the
// public parameters of tms, or nil if that installation succeeded, is still running, or was
// never started. Unlike WaitForPublicParams it never blocks.
func (b *Backend) PendingInstallError(tms *tokentopology.TMS) error {
	task := b.installTaskFor(tms)
	if task == nil {
		return nil
	}

	select {
	case <-task.done:
		return task.result()
	default:
		return nil
	}
}

// newInstallTask registers a fresh task for tms, replacing any previous one.
func (b *Backend) newInstallTask(tms *tokentopology.TMS) *installTask {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if b.installs == nil {
		b.installs = map[string]*installTask{}
	}
	task := newInstallTask()
	b.installs[tms.ID()] = task

	return task
}

// installTaskFor returns the task registered for tms, or nil if there is none.
func (b *Backend) installTaskFor(tms *tokentopology.TMS) *installTask {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	return b.installs[tms.ID()]
}

// setupPublicParams waits for the issuer client to become available, then invokes the
// SetupPublicParams view on it. It returns an error if the client is still not available
// after the configured number of attempts, or if the view fails.
func (b *Backend) setupPublicParams(tms *tokentopology.TMS, ppRaw []byte) error {
	retries, delay := b.clientRetries(), b.clientRetryDelay()
	for range retries {
		issuer := b.ClientProvider.Client(issuerClientID)
		if issuer != nil {
			return callSetupPublicParamsView(issuer, tms, ppRaw)
		}

		logger.Infof("public params setup on [%s]...client [%s] not ready, wait a bit...", tms.ID(), issuerClientID)
		time.Sleep(delay)
	}

	return errors.Errorf("client [%s] not ready after %d attempts, cannot set up the public params on [%s]", issuerClientID, retries, tms.ID())
}

func (b *Backend) clientRetries() int {
	if b.ClientRetries > 0 {
		return b.ClientRetries
	}

	return defaultClientRetries
}

func (b *Backend) clientRetryDelay() time.Duration {
	if b.ClientRetryDelay > 0 {
		return b.ClientRetryDelay
	}

	return defaultClientRetryDelay
}

func (b *Backend) installDelay() time.Duration {
	if b.InstallDelay > 0 {
		return b.InstallDelay
	}

	return defaultInstallDelay
}

// callSetupPublicParamsView invokes the SetupPublicParams view on the given client.
func callSetupPublicParamsView(issuer api.GRPCClient, tms *tokentopology.TMS, ppRaw []byte) error {
	// marshalling here rather than via common.JSONMarshall, whose failure mode is an
	// assertion that needs a registered gomega fail handler
	input, err := json.Marshal(&ppsetup.SetupPublicParams{
		Network:         tms.Network,
		Channel:         tms.Channel,
		Namespace:       tms.Namespace,
		PublicParamsRaw: ppRaw,
		Timeout:         setupPublicParamsTimeout,
	})
	if err != nil {
		return errors.Wrapf(err, "failed marshalling the public params setup request for [%s]", tms.ID())
	}

	if _, err := issuer.CallView(setupPublicParamsView, input); err != nil {
		return errors.Wrapf(err, "failed setting up the public params on [%s:%s:%s:%s]", tms.Network, tms.Channel, tms.Namespace, tms.Driver)
	}

	return nil
}
