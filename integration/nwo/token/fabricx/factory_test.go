/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package fabricx

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	tokentopology "github.com/LFDT-Panurus/panurus/integration/nwo/token/topology"
	"github.com/LFDT-Panurus/panurus/integration/token/fungible/views/ppsetup"
	"github.com/hyperledger-labs/fabric-smart-client/integration/nwo/api"
	"github.com/hyperledger-labs/fabric-smart-client/pkg/utils/errors"
	viewclient "github.com/hyperledger-labs/fabric-smart-client/platform/view/services/view/grpc/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installCrashEnv marks the child process of
// TestInstallPublicParamsBackgroundFailureDoesNotCrashProcess.
const installCrashEnv = "FTS_TEST_FABRICX_INSTALL_PP_CHILD"

// fakeClient is an api.GRPCClient whose CallView behaviour is driven by the test.
type fakeClient struct {
	callView func(fid string, in []byte) (any, error)
	calls    atomic.Int32
	lastFID  atomic.Value
	lastIn   atomic.Value
}

func (c *fakeClient) CallView(fid string, in []byte) (any, error) {
	c.calls.Add(1)
	c.lastFID.Store(fid)
	c.lastIn.Store(in)

	return c.callView(fid, in)
}

func (c *fakeClient) CallViewWithContext(_ context.Context, fid string, in []byte) (any, error) {
	return c.CallView(fid, in)
}

func (c *fakeClient) StreamCallView(string, []byte) (*viewclient.Stream, error) {
	return nil, errors.New("not implemented")
}

// fakeClientProvider returns nil for the first nilTimes lookups, and client afterwards.
type fakeClientProvider struct {
	client   api.GRPCClient
	nilTimes int
	lookups  atomic.Int32
}

//nolint:ireturn // the return type is fixed by the ClientProvider interface under test
func (p *fakeClientProvider) Client(string) api.GRPCClient {
	if int(p.lookups.Add(1)) <= p.nilTimes {
		return nil
	}

	return p.client
}

func newTMS() *tokentopology.TMS {
	return &tokentopology.TMS{
		Network:   "testnet",
		Channel:   "testchannel",
		Namespace: "tns",
		Driver:    "fabtoken",
	}
}

// newBackend returns a Backend with test-sized retry budgets, so that a failing lookup does
// not take the production minute.
func newBackend(provider ClientProvider) *Backend {
	return &Backend{
		ClientProvider:   provider,
		ClientRetries:    3,
		ClientRetryDelay: time.Millisecond,
		InstallDelay:     time.Millisecond,
	}
}

func okClient() *fakeClient {
	return &fakeClient{callView: func(string, []byte) (any, error) { return nil, nil }}
}

func failingClient(msg string) *fakeClient {
	return &fakeClient{callView: func(string, []byte) (any, error) { return nil, errors.New(msg) }}
}

func TestUpdatePublicParamsClientNeverReady(t *testing.T) {
	backend := newBackend(&fakeClientProvider{client: nil, nilTimes: 1000})
	tms := newTMS()

	var err error
	require.NotPanics(t, func() { err = backend.UpdatePublicParams(tms, []byte("pp")) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client [issuer] not ready after 3 attempts")
	assert.Contains(t, err.Error(), tms.ID())
}

func TestUpdatePublicParamsNoClientProvider(t *testing.T) {
	backend := &Backend{}

	var err error
	require.NotPanics(t, func() { err = backend.UpdatePublicParams(newTMS(), []byte("pp")) })
	require.ErrorContains(t, err, "no client provider available")
}

func TestUpdatePublicParamsClientBecomesReady(t *testing.T) {
	client := okClient()
	provider := &fakeClientProvider{client: client, nilTimes: 2}
	backend := newBackend(provider)

	require.NoError(t, backend.UpdatePublicParams(newTMS(), []byte("pp")))
	assert.Equal(t, int32(1), client.calls.Load())
	assert.Equal(t, int32(3), provider.lookups.Load())
}

func TestUpdatePublicParamsCallViewError(t *testing.T) {
	backend := newBackend(&fakeClientProvider{client: failingClient("connection refused")})
	tms := newTMS()

	var err error
	require.NotPanics(t, func() { err = backend.UpdatePublicParams(tms, []byte("pp")) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.Contains(t, err.Error(), "failed setting up the public params on [testnet:testchannel:tns:fabtoken]")
}

func TestUpdatePublicParamsSendsExpectedPayload(t *testing.T) {
	client := okClient()
	backend := newBackend(&fakeClientProvider{client: client})
	tms := newTMS()
	ppRaw := []byte("public-parameters")

	require.NoError(t, backend.UpdatePublicParams(tms, ppRaw))

	assert.Equal(t, "SetupPublicParams", client.lastFID.Load())
	in, ok := client.lastIn.Load().([]byte)
	require.True(t, ok, "no payload recorded")
	var sent ppsetup.SetupPublicParams
	require.NoError(t, json.Unmarshal(in, &sent))
	assert.Equal(t, tms.Network, sent.Network)
	assert.Equal(t, tms.Channel, sent.Channel)
	assert.Equal(t, tms.Namespace, sent.Namespace)
	assert.Equal(t, ppRaw, sent.PublicParamsRaw)
	assert.Equal(t, 2*time.Minute, sent.Timeout)
}

func TestInstallPublicParamsSuccess(t *testing.T) {
	client := okClient()
	backend := newBackend(&fakeClientProvider{client: client})
	tms := newTMS()

	require.NoError(t, backend.InstallPublicParams(tms, []byte("pp")))
	require.NoError(t, backend.WaitForPublicParams(tms, 10*time.Second))
	assert.Equal(t, int32(1), client.calls.Load())
	assert.NoError(t, backend.PendingInstallError(tms))
}

func TestInstallPublicParamsNoClientProvider(t *testing.T) {
	backend := &Backend{}

	require.ErrorContains(t, backend.InstallPublicParams(newTMS(), []byte("pp")), "no client provider available")
}

func TestInstallPublicParamsCallViewErrorIsReported(t *testing.T) {
	backend := newBackend(&fakeClientProvider{client: failingClient("connection refused")})
	tms := newTMS()

	// scheduling the installation succeeds, the failure surfaces when waiting for it
	require.NoError(t, backend.InstallPublicParams(tms, []byte("pp")))
	err := backend.WaitForPublicParams(tms, 10*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	// the outcome is memoised: asking twice reports the same failure
	require.ErrorContains(t, backend.WaitForPublicParams(tms, 10*time.Second), "connection refused")
	require.ErrorContains(t, backend.PendingInstallError(tms), "connection refused")
}

func TestInstallPublicParamsClientNeverReadyIsReported(t *testing.T) {
	backend := newBackend(&fakeClientProvider{client: nil, nilTimes: 1000})
	tms := newTMS()

	require.NoError(t, backend.InstallPublicParams(tms, []byte("pp")))
	require.ErrorContains(t, backend.WaitForPublicParams(tms, 10*time.Second), "client [issuer] not ready after 3 attempts")
}

func TestInstallPublicParamsPanicIsRecovered(t *testing.T) {
	panicking := &fakeClient{callView: func(string, []byte) (any, error) { panic("boom") }}
	backend := newBackend(&fakeClientProvider{client: panicking})
	tms := newTMS()

	require.NoError(t, backend.InstallPublicParams(tms, []byte("pp")))
	err := backend.WaitForPublicParams(tms, 10*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic while installing public params")
	assert.Contains(t, err.Error(), "boom")
}

func TestWaitForPublicParamsTimeout(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	slow := &fakeClient{callView: func(string, []byte) (any, error) {
		<-blocked

		return nil, nil
	}}
	backend := newBackend(&fakeClientProvider{client: slow})
	tms := newTMS()

	require.NoError(t, backend.InstallPublicParams(tms, []byte("pp")))
	require.ErrorContains(t, backend.WaitForPublicParams(tms, 50*time.Millisecond), "timeout waiting for the installation")
	// an installation still in flight is not reported as a failure
	assert.NoError(t, backend.PendingInstallError(tms))
}

func TestWaitForPublicParamsWithoutInstall(t *testing.T) {
	backend := newBackend(&fakeClientProvider{client: okClient()})

	assert.NoError(t, backend.WaitForPublicParams(newTMS(), time.Millisecond))
	assert.NoError(t, backend.PendingInstallError(newTMS()))
}

// TestInstallPublicParamsBackgroundFailureDoesNotCrashProcess is the regression test for the
// original defect: the background installation goroutine panicked on a CallView error, which
// terminated the whole process asynchronously. An unrecovered goroutine panic kills the test
// binary too, so the check runs in a child process: the parent re-executes this same test
// with installCrashEnv set and asserts the child exits cleanly.
func TestInstallPublicParamsBackgroundFailureDoesNotCrashProcess(t *testing.T) {
	if os.Getenv(installCrashEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		//nolint:gosec // re-executes this very test binary with a compile-time test name
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
		cmd.Env = append(os.Environ(), installCrashEnv+"=1")
		out, runErr := cmd.CombinedOutput()

		require.NoError(t, runErr, "the background installation failure must not crash the process:\n%s", out)
		assert.NotContains(t, string(out), "panic:", "the background goroutine must not panic:\n%s", out)
		assert.Contains(t, string(out), "PASS")

		return
	}

	// child process: a failing installation must be reported, not panicked
	backend := newBackend(&fakeClientProvider{client: failingClient("connection refused")})
	tms := newTMS()

	require.NoError(t, backend.InstallPublicParams(tms, []byte("pp")))
	require.ErrorContains(t, backend.WaitForPublicParams(tms, 10*time.Second), "connection refused")
	// leave the goroutine time to do any further damage before the process exits
	time.Sleep(100 * time.Millisecond)
}
