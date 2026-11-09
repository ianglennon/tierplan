// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, describe, expect, it } from "vitest";

import { Toaster } from "@/components/ui/sonner";
import { toastSuccess } from "@/lib/toast";

// test-setup.ts dismisses every toast after each test, so a toast left over from one test is not
// replayed into the next test's <Toaster>. Each test here leaves a toast active and then checks
// that none came from the test before it. Whichever runs second, in any shuffled order, proves
// the reset.

afterEach(() => {
  cleanup();
});

describe("toasts between tests", () => {
  for (const name of ["first", "second"]) {
    it(`starts the ${name} test with no toast left over`, async () => {
      expect(toast.getToasts()).toEqual([]);
      render(<Toaster />);
      toastSuccess(`Done ${name}.`);
      expect(await screen.findByText(`Done ${name}.`)).toBeTruthy();
      expect(toast.getToasts()).toHaveLength(1);
    });
  }
});
