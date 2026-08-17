---
name: debug-integration-tests
description: "Techniques for debugging Panurus integration tests — log locations, Docker/network inspection, and Ginkgo focus/skip. Trigger: /debug-integration-tests"
trigger: /debug-integration-tests
---

# Debugging Integration Tests

This doc is the single source of truth. In Claude Code it is also exposed as the
`/debug-integration-tests` skill via a symlink at
`.claude/skills/debug-integration-tests/SKILL.md`.

## Running Integration Tests: `TEST_FILTER` Labels

`TEST_FILTER` is a Ginkgo `--label-filter` expression, and it can combine **two
independent kinds of label** with `&&`:

1. **Test-identifying labels** (`T1`, `T2`, `T2.1`, `T3`, ...) — identify the
   scenario itself, set via `Label("T1")` on the individual `It(...)` block
   (e.g. `integration/token/fungible/dlog/dlog_test.go`).
2. **Infrastructure-type labels** (`websocket`, `libp2p`, `replicas`) — identify
   the transport/replication configuration the scenario runs under, defined in
   `integration/ports.go` (`WebSocketNoReplication`, `LibP2PNoReplication`,
   `WebSocketWithReplication`). Suites that call `fungible.TestAll` loop over
   `integration.AllTestTypes` and wrap every `Describe` in the matching infra
   label, so a filter of just `T1` runs T1 once per infra type sequentially.

Combine them to pin a scenario to one infrastructure type:

```bash
make integration-tests-dlog-fabric TEST_FILTER="T1 && websocket"
make integration-tests-fabricx-dlog TEST_FILTER="T6 && libp2p"
```

`fungible.mk` and `fabricx.mk` already expose make targets for the common
combos, e.g. `integration-tests-dlog-fabric-t1-websocket`, `-t1-libp2p`,
`-t1-replicas` (CI uses these to run the three infra configurations as
parallel jobs). The plain `-tN` targets (no infra suffix, e.g.
`integration-tests-dlog-fabric-t1`) leave the infra label unset and run all
three types sequentially.

**Local default: websocket only.** Unless the user asks for `libp2p`,
`replicas`, or "all infra types", always add `&& websocket` (or use an
existing `-websocket` make target) when running integration tests locally.
The other two configurations are far more expensive to set up locally and
are already covered by CI's parallel per-infra jobs.

## Log Locations
- **Integration Tests**: System temp directory (`/tmp/fsc-integration-<random>/...`)
- **Containers**: `docker logs <container_name>`
- **Persisted Logs**: Temporarily modify test to use `NewLocalTestSuite` (outputs to `./testdata`)
- **CI**: For a failing PR, fetch the failed jobs' logs from the most recent failed CI run
  with `ci/scripts/get-pr-failed-logs.sh <PR_NUMBER> [REPO]` (requires `gh` authenticated).
  It saves one cleaned, timestamp-stripped log file per failed job under
  `pr_<PR_NUMBER>_failed_logs/`.

## Public Parameters Setup Failures (fabricx)

On a fabricx network the token public parameters are installed by invoking the
`SetupPublicParams` view on the `issuer` FSC node
(`integration/nwo/token/fabricx/factory.go`). Two things about how that failure is
reported are worth knowing when a suite goes red:

- **Installation is asynchronous.** `Backend.InstallPublicParams` is called from
  `PostRun` and returns as soon as the work is scheduled, because it has to wait for the
  issuer node to accept connections. Its outcome is recorded on the backend and reported
  later:
  - `NetworkHandler.UpdatePublicParams` checks it first, so a spec that updates the
    public parameters fails with the original installation error rather than with a
    confusing follow-up failure;
  - `NetworkHandler.Cleanup` logs it at teardown (`public params installation for [...]
    failed: ...`), so grep the suite log for that line when a network never became usable
    but no spec pointed at the public parameters;
  - a test can also block on it explicitly with
    `Backend.WaitForPublicParams(tms, timeout)`.
- **A not-yet-started issuer is a wait, not a failure.** Both `InstallPublicParams` and
  `UpdatePublicParams` retry the issuer client lookup (60 attempts, 1s apart by default,
  configurable via the `ClientRetries`/`ClientRetryDelay`/`InstallDelay` fields of
  `Backend`). `client [issuer] not ready after 60 attempts` therefore means the issuer FSC
  node never came up — look at its own logs, not at the token platform.
- **Neither path panics.** A `SetupPublicParams` failure surfaces as a test failure
  wrapping the view error (`failed setting up the public params on
  [network:channel:namespace:driver]`). A process that dies with
  `panic: failed updating pps` is running an old build.

## Debugging Techniques
- **Manual Inspection**: Use `time.Sleep()` or pause loops in tests to inspect Docker state
- **Network Preservation**: Check for `no-cleanup` option or manually comment test suite cleanup
- **Focused Tests**: Modify `It(...)` to `FIt(...)` to focus, or `XIt(...)` to skip (never commit these changes)
