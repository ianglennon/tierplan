<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# Testing

CI should pass or fail only because of a real defect. A test that fails one run in fifty is a defect in the test or in the code, and it costs the maintainer more than it finds, so this page sets out what "stable" means, how a test may wait, when a test may be quarantined, and how to reproduce a failure. `make lint` enforces the mechanical parts, so a change that breaks them fails before review.

The commands to run the suites are in the [make targets table](../CONTRIBUTING.md#make-targets).

## The stability bar

A Go package is stable when all of these pass on a machine that is under the load CI puts on it, that is, with another suite running beside it:

- `go test -race -count=20 -shuffle=on`. One failure in the 20 means the package is not stable.
- `-cpu=1` and `-cpu=4`, once each. A test that passes at only one parallelism level is flaky.

The web (Vitest) suite is stable when it passes ten runs in a row, each with a different shuffle seed.

The Docker-backed jobs (the integration matrix, the swarm legs, e2e, proxy-smoke and the upgrade smoke) are too slow to repeat twenty times. They are proved by repeated full CI runs of one commit instead: five in a row, all green, with no code change between them (`scripts/ci-gate.sh` judges such a series).

Shuffling is permanent. `make test` always runs with `-shuffle=on`, and the web suite always runs shuffled, so order dependence shows up as it is written and every failure prints the seed that reproduces it.

To check a package yourself:

```sh
# 20 shuffled race runs with every CPU busy beside the test
scripts/with-load.sh auto -- go test ./internal/<pkg> -race -count=20 -shuffle=on -timeout 3h

# the same bar for one share of the whole suite (api, core, rest or agent-web)
make test-stress STRESS_SHARD=api
```

`scripts/with-load.sh N -- command` starts N CPU busy loops (`auto` is one per CPU) and stops them when the command ends. The command runs in its own session and a signal reaches its whole process group, so the test binaries `go test` compiled and anything they started end with it. `make test-stress` runs the stability bar for one shard (see "Stress runs"); a shard takes hours, so run it on a machine you can leave alone.

Two more sweeps find tests that rely on timing:

- `TIERPLAN_TEST_PROBE_DELAY=150ms go test ./internal/<pkg>` makes every fake Docker engine answer its first probes late. A test that treats "the environment is online" as "the environment is ready to use" fails under it. Run it over any package that starts a monitor or a session.
- `-cpu=1` squeezes every goroutine onto one thread, which exposes a test that only works because two things happen to run at once.

## Waiting in tests

A test waits for the thing its next assertion depends on, never for a length of time. Waiting a fixed time is either too short on a loaded machine, which makes the test flaky, or longer than it needs to be everywhere else, which makes the suite slow.

Choose the way to wait in this order:

1. **A signal from the code under test.** Use the seam that exposes one:
   - `Monitor.AwaitStatus` (internal/envs) waits for an environment to reach a status.
   - the events hub's `Await` waits for a condition over its state.
   - `Session.AwaitStatus` and `Hub.AwaitSession` (internal/tunnel) wait for an agent session to reach a state.
   - `Fake.Await` (internal/dockerx/dockertest) waits for a request to reach the fake engine.
   - `FakeConfig.ProbeHold` holds the fake's `/_ping` answers on a channel the test closes, so the test decides when the engine becomes ready.
   - `testsupport.Await` and `testsupport.AwaitWithin` receive from a channel and fail with a message naming what was awaited. `testsupport.Context` gives a context that ends after the shared deadline.
2. **An injected clock or tick.** Code driven by time takes a clock or a ticker channel in its constructor, and the test advances it. `testing/synctest` is the right tool for pure in-process timers. It does not fit code that uses real sockets, files or processes, because those do not run on the fake clock.
3. **Polling, only for a condition outside the test's control.** `testsupport.WaitFor` and `testsupport.Poll` check a condition on an interval with a deadline and a message naming what was awaited. Use them only when something other than your own process brings the condition about: a real Docker engine, an operating-system lock, a child process writing a file. A test with no integration or swarm build tag must mark each call with `// testsupport: external, <what is external>` on the call line or the line above, and `scripts/poll-check.sh` (part of `make lint`) fails a call without it. If code in the same process makes the condition true, it has a signal; add one (an `Await` method or a channel) instead of polling.

Other rules:

- **No `time.Sleep` in a test file.** `make lint` fails on it. A sleep that is the behaviour under test, such as a fake that answers slowly on purpose or a window in which something must not happen, is allowed with a reason: `time.Sleep(d) //nolint:forbidigo // the handler streams one byte every 100 ms; the pace is what the read-timeout check measures`. A sleep used to "give it a moment" is not behaviour under test.
- **Assert that something did not happen only after a later event.** To show an event was ignored, send a second event that must be processed after the first, wait for that, and then assert the first had no effect. Do not sleep and hope the event would have arrived by then.
- **Use generous failure deadlines.** `testsupport.Deadline` is 30 seconds. A deadline only decides how long a broken test takes to fail, so a long one costs nothing when the test passes and a short one is a flake on a loaded runner.
- **Every top-level test in `internal/api` runs in parallel.** The `paralleltest` linter fails a test there that does not call `t.Parallel()`. Where a fixture shares state (a fixed port, a package variable, one store), remove the sharing instead of leaving the test serial.
- **Test helpers stay out of shipped code.** `proto/testsupport` may be imported by `_test.go` files and by the test helper packages (`internal/dockerx/dockertest`, `internal/tunnel/tunneltest`, `internal/server/servertest`, `internal/store/storetest`), nowhere else. `make lint` fails an import from product code.

### The wall-clock gate

A short wait on real time is a flake that has not failed yet. `scripts/wallclock-check.sh` (part of `make lint`) fails a line that waits on, or is bounded by, a short stretch of real time and says nothing about why that is safe. It checks:

- **Go (`_test.go`).** A call to `time.After(`, `time.NewTimer(`, `context.WithTimeout(`, `context.WithDeadline(` or `time.Since(`, and any `...Timeout:` struct field set to a short duration literal (`Timeout: 5 * time.Second`). Not a finding: a line that uses `testsupport.` (its deadlines are generous), holds a literal of 30 s or more (`N * time.Second` with N >= 30, `time.Minute`, `time.Hour`), or is a `t.Log`/`t.Logf` call. `proto/testsupport/` implements the waits and is exempt.
- **Web (`*.test.ts`, `*.test.tsx`).** `shouldAdvanceTime: true` (the fake clock then follows real time), `timeout: <n>` below `asyncUtilTimeout`, a per-test `}, <n>)` below `testTimeout`, and `vi.waitFor(` with no timeout (Vitest then waits only 1 s). The two thresholds are read from `web/src/test-setup.ts` (`asyncUtilTimeout`) and `web/vitest.config.ts` (`testTimeout`), so they follow the configured deadlines. A value the script cannot read is an error (exit 2), never a pass.

A finding is fixed in this order: wait on a signal or a `testsupport` deadline (above), compare against a frozen clock (`vi.useFakeTimers({ toFake: ["Date"] })` with a fixed time) instead of real elapsed time, or keep the line with its reason as a marker at the end of the line or on the line above:

```go
case <-time.After(100 * time.Millisecond): // wall-clock: CloseAll cannot return before release on a correct registry, so this window can only false-pass, never fail a correct run
```

```ts
// wall-clock: the test measures how a real timer paces a retry, so the clock must follow real time
vi.useFakeTimers({ shouldAdvanceTime: true });
```

The marker needs a reason; an empty one fails. A good reason says why the window is the behaviour under test and why it cannot fail a correct run (a lower bound, because a timer cannot fire early; an absence check that can only miss a late wrong answer, never fail a correct one), or that the line is not a wait at all (a request field, a container's own healthcheck setting).

What the gate cannot see is a short duration held somewhere else and used as a deadline later: a `within` parameter, a `timeout` variable, `Timeout: timeout`. The value is not on the line that uses it, and a rule that guessed would flag half the helpers in the tree. Keep those out by passing `testsupport.Deadline` (or nothing) from the caller, and mark the one place that must take a duration (the helper's own `context.WithTimeout(ctx, d)` line is checked, so it carries a marker saying the callers pass a long one). Review catches the rest.

When a flake turns out to be a real race or ordering bug in the product, fix the product code and add a regression test that fails every time without the fix. A test-only workaround that hides a product race is not acceptable.

## The swarm suite

Each swarm test needs a real swarm of dind nodes, and starting one costs about half of a test's time. The tests that leave the swarm itself as they found it (they make and remove their own services, secrets, configs, networks and volumes, and install and remove agents) take turns on one swarm of three managers and a worker: they are cases in `sharedSwarmCases`, run as subtests of `TestSwarmShared` (`internal/agents/swarm_shared_test.go`). Each case still gets its own TierPlan and agent environment. After each case `dockertest.Swarm.Reset` removes everything a test can leave (services, secrets, configs, networks, volumes, containers, images, node labels, the test registry's files and sign-in) and checks the swarm against the baseline recorded when it started; a swarm that fails the check is removed and a new one started, and the log says so.

- A test that stops, removes, demotes or promotes a node, makes a node leave, or needs another shape stays a `TestSwarm…` function with a swarm of its own. Two swarms never exist at once.
- The cases run in a shuffled order that follows `-shuffle`; the log names the seed, and `TIERPLAN_SWARM_SHARED_SEED=<seed>` replays it. Run one case with `-run 'TestSwarmShared/Networks$'`.
- A case that leaves something `Reset` does not know about makes the next case start a fresh swarm: that is safe but slow, so teach `Reset` about it.

### The network and address-space cases

Phase 10 adds four cases to `sharedSwarmCases`, so they share the one swarm and each gets its own TierPlan:

- `TestSwarmShared/AddressSpacePools` checks the numbers of a real swarm through the Address space route: the pool (`10.0.0.0/8` at `/24`, capacity 65,536, measured and not estimated), ingress counted once, and each overlay's used and capacity. A test-only reader clock (`WithAddressSpaceClock`, `servertest.Options.AddressSpaceClock`) lets the case move the 5-second cache without sleeping.
- `TestSwarmShared/HostRoutes` installs the agent on the swarm with the command TierPlan generates, route-file mount included, and reads a node's routes back through the tunnel. It proves that a swarm service accepts a bind mount of the single file `/proc/1/net/route`.
- `TestSwarmShared/OrphanMatrix` judges real networks on the swarm with the reader's clock a day and an hour on, so none is held back by the grace period (`sharedCaseOptions` gives the case its own server options). The list must hold a labelled overlay whose stack is gone and an unlabelled unused one, and must not hold the ones that look idle and are not: an overlay a service scaled to zero names, an overlay only a service's previous spec names, and an attachable overlay a standalone container on the worker uses. Deleting the orphan saves its inspect to the audit log first; deleting the previous-spec overlay is refused as in use again and nothing reaches the engine.
- `TestSwarmShared/SubnetOverlap` holds the rule of UAT gap G-10-8 against a real swarm: an overlay over an overlay is refused `subnet_overlap_swarm` with `createAnyway` on the network route and on service create with a staged network, with nothing left on any manager; a worker bridge over the overlay is created anyway and a second bridge on the same subnet is `subnet_refused`; `dockerx.NetworkCreateAllocated` reports an overlapping overlay not allocated and removed and a free one allocated; and a stack whose network overlaps the overlay fails its networks step with `network_not_allocated` and leaves no network or service. It uses subnets of `10.231.0.0/16` to `10.234.0.0/16` and runs on Engine 29 and Engine 24.

Run them on their own with `go test -tags=swarm ./internal/agents/ -run 'TestSwarmShared/(HostRoutes|AddressSpacePools|OrphanMatrix|SubnetOverlap)' -count=1 -timeout 80m`.

**The Engine 24 run.** `TIERPLAN_TEST_ENGINE=24` starts the swarm on Docker Engine 24 instead of the default 29. Run `TIERPLAN_TEST_ENGINE=24 go test -tags=swarm ./internal/agents/ -run 'TestSwarmShared/AddressSpacePools' -count=1 -timeout 80m` to see what an older engine gives: the pool figures are still computed, and an overlay's address counts read *Not measured*, because only Engine 29 reports them. Run `TIERPLAN_TEST_ENGINE=24 go test -tags=swarm ./internal/agents/ -run 'TestSwarmShared/SubnetOverlap$' -count=1 -timeout 80m` as well: the override rule and the read-back hold on both ends. All of these runs belong to the phase gate.

### The janitor case

Phase 11 adds `TestSwarmShared/Janitor` to the same swarm. A service of two replicas whose task exits at once and restarts after two seconds leaves exited task containers on the nodes, a second one carries the container label `tierplan.keep=true`, and an overlay labelled with a stack that does not exist is an orphan. With the janitor's clock and the address-space reader's clock 48 hours on (`sharedCaseOptions`), the case checks that:

- the containers carry the swarm labels the planner groups by (`com.docker.swarm.service.id`, `service.name`, `task.id` and `task.name`), and that the preview lists exactly the containers beyond the newest two of each slot, as the engines themselves hold them, on the node each is on;
- the labelled service's containers are protected, and the orphan overlay is removable;
- Clean now removes exactly those and nothing else, and the network is saved (`network.inspect_saved`) before it is removed (`network.removed`, source janitor);
- the task history limit reads as Docker's 5 through a manager's agent, is set to 7 through the agent's swarm update route, reads back as 7 and is put back, because the shared swarm's reset does not restore the cluster spec.

Run it with `go test -tags=swarm ./internal/agents/ -run 'TestSwarmShared/Janitor$' -count=1 -timeout 80m`, and again with `TIERPLAN_TEST_ENGINE=24`: the labels and the spec round trip hold on both.

Two integration-tagged tests prove the standalone half on real engines (`make test-int` runs them with the rest):

- `TestSmallSubnetHashStableOnARealEngine` (`internal/stackops`) deploys a standalone stack with small subnets on at `/26`, then deploys it again with the setting off, with a service changed and rolled back, and checks the network keeps its ID and subnet: the Compose engine stamps a hash of the whole network on it, so sending a different subnet would recreate it under its running containers. A stack deployed before the setting was turned on also keeps its network when it is deployed again with the setting on.
- `TestStandaloneAgentReportsHostNet` (`internal/agents`) installs a standalone agent with the generated command and checks its `hostNet` fact is `ok` and its routes come back through the server, recreates it without the route mount and checks `unmounted`, and then runs the fix command an operator is shown for an older agent and checks `ok` again.

The Docker Hub `toomanyrequests` failure can appear in these runs. It is the registry's limit and not a defect; run the step again.

## The git fixtures

The git tests use a real git server, never a stand-in, and a real mirror on disk. `internal/gitx/gittest` builds them:

- `gittest.NewRepo` makes a bare repository and a working tree. `Commit`, `CommitMany`, `Orphan`, `Branch`, `Tag`,
  `Ref`, `DeleteRef`, `Symlink` and `Submodule` make the history and the hostile content a test needs, with fixed dates so
  commit IDs are stable.
- `gittest.NewHTTPServer` serves one repository over smart HTTP with `git-http-backend`, optionally over TLS with a
  certificate the test trusts through its `CAPEM`, with basic auth, and with the server-side switch for asking a commit by
  its ID. `Requests` counts the ref advertisements, which the tests of polling and deduplication assert on.
- `gittest.NewSSHServer` serves a repository over SSH in the test process, with a host key `RotateHostKey` can change, to
  test trust on first use and a changed key.

**`git` and `git-http-backend` must be installed**, on a developer machine and on the runners. A fixture **fails the test**
when either is missing, never skips it, because a skipped test would pass the git code unchecked.
`scripts/ci-preflight.sh`, the first step of every Docker-heavy CI job, fails the job with the missing tool named before
any test runs. `git-http-backend` lives in the directory `git --exec-path` prints; the `git` package of Debian and Ubuntu includes it.

The git tests follow the rest of this page: time is a clock the test moves (the poller, the mirror sweep and the webhook
limiters all take one), a wait is a channel the code under test signals, and nothing sleeps.

## Quarantine

Quarantine is a last resort, not a parking lot. Use it only when a failing test cannot be fixed in the change that found it.

A quarantined test calls `testsupport.Quarantine(t, "<date> <tracking reference and reason>")` as its first line. The reason must start with the date in `YYYY-MM-DD` form and a space, and the helper rejects anything else. A quarantined test:

- is skipped in pull request CI;
- runs in `make test-stress` (`TIERPLAN_RUN_QUARANTINED=1` turns the skip off), so a stress run by hand tells you whether it has become stable;
- is listed in the table below, with the same date and a reason. `scripts/quarantine-check.sh` (part of `make lint`) fails when a call site and a table row disagree, in either direction;
- must be fixed or deleted before the next phase of the project ships. A phase does not exit with a quarantined test, and `scripts/quarantine-check.sh --none` is the check for that.

Add the row in the same change as the call, and remove both together.

## Quarantined tests

| Test | Package | Since | Reason |
|------|---------|-------|--------|

No test is quarantined.

## Reproducing a failure

Every failure names the package and test, and prints the shuffle seed. The failing-test summary at the end of a CI log and in the job summary lists both. Order dependence is the common cause of a failure that does not repeat, so reproduce it with the same seed.

Go: take the seed from the package's `-test.shuffle <seed>` line (the summary shows it as "shuffle seed N") and run

```sh
go test ./internal/<pkg> -run '^TestName$' -shuffle=<seed> -count=1 -race
```

Drop `-run` to replay the whole package in the order that failed, since an order failure usually needs the other tests that ran before it. Add `-count=20` for a failure that depends on timing rather than order, and run it under `scripts/with-load.sh` to match a busy runner.

Web: the seed is printed at the start of the run (`test-web: seed N`) and named in the summary.

```sh
make test-web WEB_SEED=<seed>
```

## No retries

There are none, anywhere. No test, job or step reruns itself, `gotestsum` is never given a rerun option, Playwright's `retries` is 0, and a network call in a script does not retry silently. `scripts/no-retries-check.sh` (part of `make lint`) fails on a retry option.

Do not rerun a failed CI job to get a green tick. A test that fails once is a defect to find, and a rerun that passes hides it until it fails again on someone else's change. The only reruns are the ones that measure: running a green commit in full again to see whether it stays green.

## Stress runs

`make test-stress STRESS_SHARD=<shard>` runs the stability bar over one of four shards of the Go suite and the web suite (`api`, `core`, `rest`, `agent-web`), including quarantined tests. No workflow runs it: the scheduled nightly stress workflow was removed on 2026-10-18, because five green CI runs of one commit cover the Docker-backed jobs and the per-package load runs cover the rest, and the runners' time is better spent elsewhere. Run a shard by hand before a release, or when chasing a flake; `make test-summary` then lists each failing test with how often it failed ("failed 2/20") and the seed.

A shard runs inside one time budget, `STRESS_BUDGET` in the Makefile (330 minutes): every go test pass and every web run gets the time left in it as its `-timeout`, and a pass that finds no time left fails the shard instead of being skipped. `scripts/stress.sh` refuses a `-timeout` of its own, because the budget governs. A shard also fails, naming the module, when `go list` fails or lists nothing, and it removes the previous run's `stress-web-*` results before the web runs, so a green shard means every package ran this time. If a workflow ever runs a shard again, its job must allow 10 minutes or more above the budget, which `scripts/timeouts-check.sh` checks.

The runners share their Docker daemons, so CI runs one at a time across every branch (one concurrency group). GitHub keeps one running and one pending run per group, and a newer pending run replaces an older one. A run that has already started is never cancelled.

A pending push-to-main run can be displaced the same way, by a later pull request run. This is accepted: it is the price of one run at a time on the shared Docker daemons. After merging, check that main's head commit has a run that was not cancelled:

```bash
gh run list --workflow CI --branch main --limit 1 --json headSha,conclusion
```

The `headSha` must be main's head commit and the `conclusion` must not be `cancelled`. If it is, start that run again with `gh run rerun <id>`. (This is a rerun that measures a commit that never ran, not a rerun of a failure; see "No retries".)

## Who may run code on the runners

`pull_request` workflows run on the self-hosted runners, which have Docker access, and Docker access is equivalent to root on the runner VM. That is acceptable while the repository is private and every contributor is trusted. Before the repository is made public, workflows from outside contributors must need a maintainer's approval before they run, or pull request workflows must be restricted to branches of the same repository (and run on GitHub-hosted runners otherwise). Make this change before the visibility change, not after.

## What CI tells you

- **The summary.** Every test job ends with the failing tests, the hung tests and the packages that did not build, by name, in the log and in the job summary. A test that ran several times and failed some shows "failed k/N". `make test-summary` produces the same list from the result files in `.devdata/test`.
- **Hangs.** The go test time of a job is always at least ten minutes below its `timeout-minutes`, so a hang ends in Go's goroutine dump naming the test instead of a silent kill. A make target that runs several go test passes in a row has one time budget, written as the literal `TEST_BUDGET=<N>m` in its recipe (`TEST_GO_BUDGET`, 30 minutes, for `make test-go`; `STRESS_BUDGET`, 330 minutes, for `make test-stress`), and each pass gets `-timeout <the time left in the budget>`, so the passes cannot add up past the job. `scripts/timeouts-check.sh` (part of `make lint`) reads the budget from `make -n`, adds up the budgets (or, without one, all the `-timeout` values) of the targets a job runs, and fails when the total is within ten minutes of the job's `timeout-minutes`. It also fails on a go test run in a shell loop with no budget, on a job that runs make targets but has no `timeout-minutes`, on a workflow in which it finds no job, and on `-timeout 0`. One limit: go test applies `-timeout` to each package's test binary, so in a pass over many packages a hang in a package that starts late can run past the budget by up to that package's own run. The ten minute margin and the job summary, which names a test that started and never ended as hung, cover that case.
- **The end-to-end run's budget.** Playwright's `globalTimeout` (20 minutes, in `e2e/playwright.config.ts`) and the e2e job's `timeout-minutes` (45, in `.github/workflows/ci.yml`) move together: the job covers the setup before Playwright starts plus the 20 minutes plus a margin, so a hang ends in Playwright's own report, and `scripts/timeouts-check.sh` checks the pair. The `stacks` project (two tests: a stack on the engine TierPlan manages and a stack with a secret file through a real agent) took about 30 seconds in a run of all 43 tests that took about 4 minutes of Playwright time, so neither figure moved. If the run ever nears 15 minutes, raise both together and write the new figures here. The `stacks` project needs a registry to pin images against, and the two engines have no route to a real one, so each runs a `registry:2` of its own on its loopback (the way the Go integration tests do), loaded from the machine running the suite and removed again at the end of the test.
- **Artifacts.** A failing test job keeps its result JSON, and a failing end-to-end run keeps its Playwright report, as job artifacts for seven days. Every upload runs when the job fails or is cancelled, so a job ended by its timeout still leaves them.
- **No leftovers.** The Docker-backed smokes label everything they create with `tierplan.ci.owner`. `make ci-clean TIERPLAN_CI_OWNER=<owner>` removes the containers, networks and volumes with that label and nothing else, and every Docker job runs it at the end whether it passed or not. `make e2e-down` takes the fixed-name e2e stack down and succeeds when nothing is running. `make ci-preflight` prints Docker's disk use and fails naming the filesystem when less than `TIERPLAN_CI_MIN_FREE_GB` (default 20) gigabytes are free, so a full disk shows as itself instead of as a test failure.
- **The same commands.** CI runs thin wrappers over the make targets, so any failure reproduces locally with the same command.
