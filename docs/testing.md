<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Bayplan Authors -->

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

`scripts/with-load.sh N -- command` starts N CPU busy loops (`auto` is one per CPU) and stops them when the command ends. `make test-stress` is what the nightly run uses (see "Nightly stress"); a shard takes hours, so run it on a machine you can leave alone.

Two more sweeps find tests that rely on timing:

- `BAYPLAN_TEST_PROBE_DELAY=150ms go test ./internal/<pkg>` makes every fake Docker engine answer its first probes late. A test that treats "the environment is online" as "the environment is ready to use" fails under it. Run it over any package that starts a monitor or a session.
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

When a flake turns out to be a real race or ordering bug in the product, fix the product code and add a regression test that fails every time without the fix. A test-only workaround that hides a product race is not acceptable.

## Quarantine

Quarantine is a last resort, not a parking lot. Use it only when a failing test cannot be fixed in the change that found it.

A quarantined test calls `testsupport.Quarantine(t, "<date> <tracking reference and reason>")` as its first line. The reason must start with the date in `YYYY-MM-DD` form and a space, and the helper rejects anything else. A quarantined test:

- is skipped in pull request CI;
- runs in the nightly stress job (`BAYPLAN_RUN_QUARANTINED=1` turns the skip off), so you learn whether it has become stable;
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

## Nightly stress

A scheduled workflow (`.github/workflows/nightly.yml`, every night at 21:30 UTC, also runnable by hand) runs the stability bar over the whole Go suite and the web suite, cut into four shards so each job fits a runner. It includes quarantined tests. It never blocks a pull request. A failure is a red scheduled run, and its summary lists each failing test with how often it failed ("failed 2/20") and the seed.

The runners share their Docker daemons, so the nightly and the main CI workflow use one concurrency group and never run at the same time. GitHub keeps one running and one pending run per group. A nightly that arrives while a pull request run is running and another is pending replaces the pending pull request run, which then never runs and has to be started again once the nightly has finished. A run that has already started is never cancelled.

## What CI tells you

- **The summary.** Every test job ends with the failing tests, the hung tests and the packages that did not build, by name, in the log and in the job summary. A test that ran several times and failed some shows "failed k/N". `make test-summary` produces the same list from the result files in `.devdata/test`.
- **Hangs.** `go test -timeout` is always at least ten minutes below the job's `timeout-minutes`, so a hang ends in Go's goroutine dump naming the test instead of a silent kill. `scripts/timeouts-check.sh` (part of `make lint`) checks the margin for every job.
- **Artifacts.** A failing test job keeps its result JSON, and a failing end-to-end run keeps its Playwright report, as job artifacts for seven days.
- **No leftovers.** The Docker-backed smokes label everything they create with `bayplan.ci.owner`. `make ci-clean BAYPLAN_CI_OWNER=<owner>` removes the containers, networks and volumes with that label and nothing else, and every Docker job runs it at the end whether it passed or not. `make e2e-down` takes the fixed-name e2e stack down and succeeds when nothing is running. `make ci-preflight` prints Docker's disk use and fails naming the filesystem when less than `BAYPLAN_CI_MIN_FREE_GB` (default 20) gigabytes are free, so a full disk shows as itself instead of as a test failure.
- **The same commands.** CI runs thin wrappers over the make targets, so any failure reproduces locally with the same command.
