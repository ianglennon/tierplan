// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { UpdateInfo, UpdateState } from "@/api/updates";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdateBadge } from "@/features/updates/update-badge";
import "@/i18n";

afterEach(cleanup);

const info = (extra: Partial<UpdateInfo> = {}): UpdateInfo => ({
  state: "not_checked",
  reason: "",
  registry: "registry.example.com",
  repository: "acme/web",
  tag: "1.4",
  runningDigest: `sha256:${"a".repeat(64)}`,
  remoteDigest: `sha256:${"b".repeat(64)}`,
  changedPlatforms: [],
  checkedAt: new Date().toISOString(),
  successAt: "",
  note: "",
  ...extra,
});

function show(extra: Partial<UpdateInfo>, count?: number) {
  return render(
    <TooltipProvider>
      <UpdateBadge info={info(extra)} count={count} />
    </TooltipProvider>,
  );
}

// The words of UI-SPEC "Update state vocabulary", one for each state the server names.
const WORDS: Record<UpdateState, string> = {
  update_available: "Update available",
  updating: "Updating…",
  busy: "Busy",
  queued: "Queued",
  up_to_date: "Up to date",
  check_stale: "Check stale",
  check_failed: "Check failed",
  rolled_back: "Update failed, rolled back",
  rollback_failed: "Rollback failed",
  not_from_registry: "Not from a registry",
  pinned_in_file: "Pinned in file",
  not_checked: "Not checked yet",
  skipped: "Version skipped",
  ignored: "Updates ignored",
};

describe("UpdateBadge", () => {
  it.each(Object.entries(WORDS))("renders %s as %s", (state, word) => {
    show({ state: state as UpdateState });
    expect(screen.getByText(word)).toBeTruthy();
  });
});
