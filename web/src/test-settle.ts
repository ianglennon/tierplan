// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

// Resolves once no query is fetching and no mutation is pending. A test that asserts
// "nothing more happens" (no request, no navigation) awaits this first, so the assertion
// runs after the work that could have caused it has finished, not after a guessed delay.
export async function settled(client: QueryClient): Promise<void> {
  await waitFor(() => {
    if (client.isFetching() !== 0 || client.isMutating() !== 0) {
      throw new Error("queries or mutations are still running");
    }
  });
}
