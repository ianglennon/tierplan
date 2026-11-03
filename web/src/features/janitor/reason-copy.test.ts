// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import type { JanitorReason, JanitorRun } from "@/api/janitor";
import { reasonText } from "@/features/janitor/preview-summary";
import i18n from "@/i18n";
import { keyExists } from "@/i18n/use-t";

// Every word the server can give a janitor item or run has copy. The records are keyed by the
// generated enums, so a code added to the server fails the type check here until it is listed, and
// then fails the test below until it has English words.
const PREVIEW_REASONS: Record<JanitorReason["code"], true> = {
  task_beyond_keep: true,
  task_service_gone: true,
  stack_gone: true,
  compose_project_gone: true,
  compose_oneoff: true,
  stopped_standalone: true,
  dangling_image: true,
  network_orphaned: true,
  network_unused: true,
  opt_in_off_containers: true,
  opt_in_off_networks: true,
  lone_compose_container: true,
  kept_label: true,
  kept_row: true,
  stack_excluded: true,
  stack_declared_keep: true,
  younger_than_grace: true,
  node_not_reporting: true,
  networks_withheld: true,
  stack_unknown: true,
  inspect_capped: true,
  clock_skew: true,
  task_labels_missing: true,
  services_unread: true,
  node_disabled: true,
};

const RUN_REASONS: Record<NonNullable<Exclude<JanitorRun["reason"], "">>, true> = {
  deploy_in_progress: true,
  server_restarted: true,
  no_node_reporting: true,
  failed: true,
};

// The codes of a run item, as the run items route documents them (outcome refused_in_use has its own
// outcome word and the code in_use).
const ITEM_CODES = [
  "in_use",
  "no_longer_stale",
  "gone",
  "node_offline",
  "agent_policy_denied",
  "host_read_only",
  "node_unreachable",
  "action_failed",
  "evidence_unsaved",
];

// Translate as useT does, reading the namespace from the first segment of the key.
const t = (key: string, options?: Record<string, unknown>) =>
  String(i18n.t(key, { ...options, ns: key.split(".")[0] }));

describe("the words for what the server says about a janitor item", () => {
  it.each(Object.keys(PREVIEW_REASONS))("has a sentence for the preview reason %s", (code) => {
    expect(keyExists(`janitor.reason.${code}`)).toBe(true);
    const words = reasonText(
      {
        code,
        service: "web",
        slot: "1",
        rank: 2,
        node: "node-1",
        project: "shop",
        stack: "shop",
        ageSeconds: 86_400,
        remainingSeconds: 3_600,
      } as JanitorReason,
      t,
    );
    expect(words.length).toBeGreaterThan(0);
    // Every placeholder was filled: no braces are left in the sentence.
    expect(words).not.toMatch(/[{}]/);
  });

  it.each(Object.keys(RUN_REASONS))("has a sentence for the run reason %s", (code) => {
    expect(keyExists(`janitorRun.reason.${code}`)).toBe(true);
  });

  it.each(ITEM_CODES)("has a sentence for the run item code %s", (code) => {
    expect(keyExists(`janitorRun.itemReason.${code}`)).toBe(true);
  });
});
