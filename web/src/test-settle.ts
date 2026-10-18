// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";
import { vi } from "vitest";

// Resolves once no query is fetching and no mutation is pending, and checks nothing else. A
// request that has not started yet (one behind a debounce or a timer) is not fetching, so
// settled() would pass before it: release it first, by advancing the fake clock by the delay
// constant the code exports, and only then await settled(). A test that asserts "nothing more
// happens" (no request, no navigation) awaits this first, so the assertion runs after the
// work that could have caused it has finished, not after a guessed delay.
export async function settled(client: QueryClient): Promise<void> {
  await waitFor(() => {
    if (client.isFetching() !== 0 || client.isMutating() !== 0) {
      throw new Error("queries or mutations are still running");
    }
  });
}

// Waits for a condition in a test whose clock is fully fake. Testing Library's waitFor and findBy
// poll on the faked timers, so under vi.useFakeTimers() they never poll; vitest's waitFor polls on
// the real timers it captured, and moves the fake clock on by the interval at every poll, which
// also lets the timers behind a query's notifications run. Its own deadline is 1 s, which a
// loaded runner misses for a render that is merely slow, so this gives it the same failure
// deadline as every other wait (asyncUtilTimeout in test-setup.ts): a condition that never comes
// still fails, just later. Returns what the callback returns.
export function waitUnderFakeClock<T>(callback: () => T | Promise<T>): Promise<T> {
  return vi.waitFor(callback, { timeout: 30000, interval: 50 });
}
