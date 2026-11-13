// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import audit from "@/i18n/en/audit.json";
import { resetBasePathForTests } from "@/lib/base-path";
import {
  actionLabel,
  auditSearchSchema,
  ENV_SCOPED_ACTIONS,
  exportUrl,
  formatDay,
  KNOWN_ACTIONS,
  listParams,
  parseDay,
  rangeFor,
} from "./filters";

// A translate function that echoes the key it was asked for, so a test asserts
// on which copy key the screen will look up.
const echo = (key: string) => `t:${key}`;

// fromUrl turns a query string into the object TanStack Router hands to
// validateSearch: a repeated key becomes an array, a single one a string.
function fromUrl(qs: string): Record<string, unknown> {
  const params = new URLSearchParams(qs);
  const out: Record<string, unknown> = {};
  for (const key of new Set(params.keys())) {
    const all = params.getAll(key);
    out[key] = all.length > 1 ? all : all[0];
  }
  return out;
}

const NOW = new Date("2026-10-02T12:00:00.000Z");

describe("auditSearchSchema", () => {
  it("reads user, repeated actions and the range from the URL and writes the same params back", () => {
    const parsed = auditSearchSchema.parse(
      fromUrl("user=admin&action=auth.login&action=auth.logout&range=24h"),
    );
    expect(parsed).toEqual({
      user: "admin",
      action: ["auth.login", "auth.logout"],
      range: "24h",
    });
    const back = new URLSearchParams(listParams(parsed, NOW));
    expect(back.get("user")).toBe("admin");
    expect(back.getAll("action")).toEqual(["auth.login", "auth.logout"]);
    // A parsed search parses to itself.
    expect(auditSearchSchema.parse(parsed)).toEqual(parsed);
  });

  it("defaults the range to the last 24 hours and turns one action into a list", () => {
    expect(auditSearchSchema.parse({})).toEqual({ range: "24h" });
    expect(auditSearchSchema.parse({ action: "auth.login" }).action).toEqual(["auth.login"]);
  });

  it("keeps a numeric or boolean-looking user as text, because the router JSON-parses values", () => {
    expect(auditSearchSchema.parse({ user: 123 }).user).toBe("123");
    expect(auditSearchSchema.parse({ user: true }).user).toBe("true");
  });

  it("falls back to the default range for a range it does not know", () => {
    expect(auditSearchSchema.parse({ range: "forever" }).range).toBe("24h");
  });
});

describe("rangeFor", () => {
  it("counts a preset back from now and leaves the end open", () => {
    expect(rangeFor({ range: "1h" }, NOW)).toEqual({ from: "2026-10-02T11:00:00.000Z" });
    expect(rangeFor({ range: "24h" }, NOW)).toEqual({ from: "2026-10-01T12:00:00.000Z" });
    expect(rangeFor({ range: "7d" }, NOW)).toEqual({ from: "2026-09-25T12:00:00.000Z" });
    expect(rangeFor({ range: "30d" }, NOW)).toEqual({ from: "2026-09-02T12:00:00.000Z" });
  });

  it("sends neither bound for all time", () => {
    expect(rangeFor({ range: "all" }, NOW)).toEqual({});
  });

  it("turns a custom range into inclusive days in the browser time zone, as UTC", () => {
    const got = rangeFor({ range: "custom", from: "2026-09-01", to: "2026-09-03" }, NOW);
    expect(got.from).toBe(new Date(2026, 8, 1, 0, 0, 0, 0).toISOString());
    expect(got.to).toBe(new Date(2026, 8, 3, 23, 59, 59, 999).toISOString());
  });

  it("allows a custom range with one end open and ignores a custom range with no days", () => {
    expect(rangeFor({ range: "custom", from: "2026-09-01" }, NOW)).toEqual({
      from: new Date(2026, 8, 1, 0, 0, 0, 0).toISOString(),
    });
    expect(rangeFor({ range: "custom", to: "2026-09-03" }, NOW)).toEqual({
      to: new Date(2026, 8, 3, 23, 59, 59, 999).toISOString(),
    });
    expect(rangeFor({ range: "custom" }, NOW)).toEqual({});
  });

  it("ignores a day that is not a real calendar day", () => {
    expect(rangeFor({ range: "custom", from: "2026-02-31" }, NOW)).toEqual({});
  });
});

describe("actionLabel (networks)", () => {
  it("names the network a keep, a stop and the saved details were about", () => {
    expect(actionLabel("network.kept", echo, { name: "backend" })).toBe(
      "t:audit.actions.network.kept_named",
    );
    expect(actionLabel("network.unkept", echo, { name: "backend" })).toBe(
      "t:audit.actions.network.unkept_named",
    );
    expect(actionLabel("network.inspect_saved", echo, { name: "backend" })).toBe(
      "t:audit.actions.network.inspect_saved_named",
    );
    expect(audit.audit.actions.network.kept_named).toBe("Kept network {{name}}");
    expect(audit.audit.actions.network.unkept_named).toBe("Stopped keeping network {{name}}");
    expect(audit.audit.actions.network.inspect_saved_named).toBe(
      "Saved the details of network {{name}} before deleting it",
    );
    // Without a name the plain label stands.
    expect(actionLabel("network.kept", echo)).toBe("t:audit.actions.network.kept");
  });

  it("words a removal by the orphan clean-up, and any other removal as a removal", () => {
    expect(
      actionLabel("network.removed", echo, { name: "backend", source: "orphan_cleanup" }),
    ).toBe("t:audit.actions.network.removed_orphan_cleanup_named");
    expect(audit.audit.actions.network.removed_orphan_cleanup_named).toBe(
      "Deleted orphaned network {{name}}",
    );
    expect(actionLabel("network.removed", echo, { name: "backend" })).toBe(
      "t:audit.actions.network.removed",
    );
    expect(actionLabel("network.removed", echo, { name: "backend", source: "other" })).toBe(
      "t:audit.actions.network.removed",
    );
  });
});

// real is a small stand-in for i18next that reads the same audit.json the screen does: it walks the dotted key,
// picks _one or _other from a count, and fills {{name}} style variables. Echo cannot check wording.
function real(key: string, options: Record<string, unknown> = {}): string {
  const node = (path: string): unknown =>
    path.split(".").reduce<unknown>((acc, part) => (acc as Record<string, unknown>)?.[part], audit);
  let value = node(key);
  if (value === undefined && typeof options.count === "number") {
    value = node(`${key}_${options.count === 1 ? "one" : "other"}`);
  }
  if (typeof value !== "string") return `missing:${key}`;
  return value.replace(/\{\{(\w+)\}\}/g, (_m, name: string) => String(options[name] ?? ""));
}

const PHASE_11_ACTIONS = [
  "janitor.cleaned",
  "janitor.policy_changed",
  "janitor.node_policy_changed",
  "janitor.defaults_changed",
  "janitor.kept",
  "janitor.unkept",
  "stack.janitor_excluded",
  "stack.janitor_included",
  "swarm.task_history_limit_changed",
] as const;

describe("actionLabel (janitor)", () => {
  it("knows all nine Phase 11 actions, and without details each reads its base label", () => {
    expect(PHASE_11_ACTIONS).toHaveLength(9);
    const base: Record<string, string> = {
      "janitor.cleaned": "Cleaned up the environment",
      "janitor.policy_changed": "Changed the janitor policy",
      "janitor.node_policy_changed": "Changed a node's janitor setting",
      "janitor.defaults_changed": "Changed the janitor defaults",
      "janitor.kept": "Kept an item out of cleanup",
      "janitor.unkept": "Stopped keeping an item out of cleanup",
      "stack.janitor_excluded": "Excluded a stack from the janitor",
      "stack.janitor_included": "Included a stack in the janitor again",
      "swarm.task_history_limit_changed": "Changed the swarm task history limit",
    };
    for (const key of PHASE_11_ACTIONS) {
      expect(KNOWN_ACTIONS, key).toContain(key);
      expect(actionLabel(key, real), key).toBe(base[key]);
      expect(actionLabel(key, real, {}), key).toBe(base[key]);
    }
  });

  it("names the stack a janitor exclusion was about", () => {
    expect(actionLabel("stack.janitor_excluded", real, { name: "web" })).toBe(
      "Excluded stack web from the janitor",
    );
    expect(actionLabel("stack.janitor_included", real, { name: "web" })).toBe(
      "Included stack web in the janitor again",
    );
  });

  it("names the kind and the name of an item kept or no longer kept", () => {
    expect(actionLabel("janitor.kept", real, { kind: "container", name: "web.1" })).toBe(
      "Kept container web.1",
    );
    expect(actionLabel("janitor.kept", real, { kind: "image", name: "nginx:1.27" })).toBe(
      "Kept image nginx:1.27",
    );
    expect(actionLabel("janitor.kept", real, { kind: "network", name: "backend" })).toBe(
      "Kept network backend",
    );
    expect(actionLabel("janitor.unkept", real, { kind: "container", name: "web.1" })).toBe(
      "Stopped keeping container web.1",
    );
    expect(actionLabel("janitor.unkept", real, { kind: "image", name: "nginx:1.27" })).toBe(
      "Stopped keeping image nginx:1.27",
    );
    expect(actionLabel("janitor.unkept", real, { kind: "network", name: "backend" })).toBe(
      "Stopped keeping network backend",
    );
  });

  it("falls back to the base label for a kept item whose kind or name it cannot use", () => {
    expect(actionLabel("janitor.kept", real, { kind: "volume", name: "data" })).toBe(
      "Kept an item out of cleanup",
    );
    expect(actionLabel("janitor.kept", real, { kind: "image" })).toBe(
      "Kept an item out of cleanup",
    );
    expect(actionLabel("janitor.unkept", real, { name: "web.1" })).toBe(
      "Stopped keeping an item out of cleanup",
    );
  });

  it("names the image or workload a skip or an ignore of image updates was about", () => {
    const digest = `sha256:${"b".repeat(64)}`;
    expect(
      actionLabel("image.update_skipped", real, { image: "docker.io/library/nginx:1.27", digest }),
    ).toBe("Skipped version bbbbbbbbbbbb of docker.io/library/nginx:1.27");
    expect(
      actionLabel("image.update_unskipped", real, { image: "docker.io/library/nginx:1.27" }),
    ).toBe("Stopped skipping a version of docker.io/library/nginx:1.27");
    expect(
      actionLabel("image.updates_ignored", real, { scope: "image", image: "nginx:1.27" }),
    ).toBe("Ignored updates for nginx:1.27");
    expect(actionLabel("image.updates_ignored", real, { scope: "workload", workload: "web" })).toBe(
      "Ignored updates for web",
    );
    expect(
      actionLabel("image.updates_unignored", real, { scope: "workload", workload: "web" }),
    ).toBe("Stopped ignoring updates for web");
    expect(actionLabel("image.update_skipped", real, {})).toBe("Skipped a version of an image");
    expect(actionLabel("image.updates_unignored", real, { scope: "image" })).toBe(
      "Stopped ignoring image updates",
    );
  });

  it("names the workload an image update finished or failed for", () => {
    const digest = `sha256:${"b".repeat(64)}`;
    expect(actionLabel("image.updated", real, { workload: "web", to_digest: digest })).toBe(
      "Updated web to bbbbbbbbbbbb",
    );
    expect(
      actionLabel("image.update_failed", real, { workload: "web", reason: "pull_failed" }),
    ).toBe("Update of web failed: the image could not be pulled");
    expect(actionLabel("image.update_failed", real, { workload: "web", reason: "mystery" })).toBe(
      "Update of web failed: see the update's record",
    );
    expect(
      actionLabel("image.update_failed", real, { workload: "web", reason: "server_restarted" }),
    ).toBe("Update of web failed: TierPlan restarted during this update");
    expect(actionLabel("image.updated", real, {})).toBe("Updated an image");
    expect(actionLabel("image.update_failed", real, {})).toBe("An image update failed");
    expect(
      actionLabel("image.update_rolled_back", real, { workload: "web", from_digest: digest }),
    ).toBe("Rolled back web to bbbbbbbbbbbb after a failed update");
    expect(actionLabel("image.update_rolled_back", real, {})).toBe(
      "An image update was rolled back",
    );
    expect(
      actionLabel("image.bulk_updated", real, {
        done: "2",
        total: "3",
        bulk: "0195a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
      }),
    ).toBe("Updated 2 items (bulk 0195a1b2)");
    expect(actionLabel("image.bulk_updated", real, {})).toBe("Updated image versions in bulk");
  });

  it("names the node whose janitor setting changed", () => {
    expect(actionLabel("janitor.node_policy_changed", real, { node: "worker-2" })).toBe(
      "Changed a node's janitor setting (worker-2)",
    );
  });

  it("states the task history limit that was set", () => {
    expect(actionLabel("swarm.task_history_limit_changed", real, { to: "10" })).toBe(
      "Set the task history limit to 10",
    );
  });

  it("says what a run removed, leaving out what it removed none of", () => {
    expect(
      actionLabel("janitor.cleaned", real, {
        removed_containers: "3",
        removed_images: "0",
        removed_networks: "1",
      }),
    ).toBe("Cleaned up the environment (removed 3 containers, 1 network)");
    expect(
      actionLabel("janitor.cleaned", real, {
        removed_containers: "1",
        removed_images: "2",
        removed_networks: "0",
      }),
    ).toBe("Cleaned up the environment (removed 1 container, 2 images)");
    expect(actionLabel("janitor.cleaned", real, { removed_images: "1" })).toBe(
      "Cleaned up the environment (removed 1 image)",
    );
  });

  it("reads the base label when a run removed nothing or its counts are not numbers", () => {
    expect(
      actionLabel("janitor.cleaned", real, {
        removed_containers: "0",
        removed_images: "0",
        removed_networks: "0",
      }),
    ).toBe("Cleaned up the environment");
    expect(actionLabel("janitor.cleaned", real, { removed_containers: "many" })).toBe(
      "Cleaned up the environment",
    );
    expect(actionLabel("janitor.cleaned", real, { outcome: "completed" })).toBe(
      "Cleaned up the environment",
    );
  });

  it("is an environment event except the instance-wide defaults", () => {
    for (const key of PHASE_11_ACTIONS) {
      if (key === "janitor.defaults_changed") {
        expect(ENV_SCOPED_ACTIONS, key).not.toContain(key);
      } else {
        expect(ENV_SCOPED_ACTIONS, key).toContain(key);
      }
    }
  });
});

describe("actionLabel for small stack subnets", () => {
  it("words turning them on with the size, and turning them off", () => {
    const key = "environment.small_subnets_changed";
    expect(actionLabel(key, echo, { enabled: "true", mask: "26" })).toBe(
      "t:audit.actions.environment.small_subnets_on",
    );
    expect(actionLabel(key, echo, { enabled: "false", mask: "26" })).toBe(
      "t:audit.actions.environment.small_subnets_off",
    );
    expect(actionLabel(key, echo)).toBe("t:audit.actions.environment.small_subnets_changed");
    expect(audit.audit.actions.environment.small_subnets_on).toBe(
      "Turned on small stack subnets (/{{mask}})",
    );
    expect(audit.audit.actions.environment.small_subnets_off).toBe(
      "Turned off small stack subnets",
    );
    expect(audit.audit.actions.environment.address_pools_saved).toBe("Saved address pools");
  });
});

describe("actionLabel", () => {
  it("looks up the human label of a known action", () => {
    expect(actionLabel("auth.login", echo)).toBe("t:audit.actions.auth.login");
    expect(audit.audit.actions.auth.login).toBe("Signed in");
  });

  it("falls back to the raw key for an action the UI does not know", () => {
    expect(actionLabel("something.new", echo)).toBe("something.new");
  });

  it("labels a roll back of a stack with the revision it restored, and any other deploy as a deploy", () => {
    const rollback = { mode: "rollback", stack: "web", restoredFrom: "4" };
    expect(actionLabel("stack.deployed", echo, rollback)).toBe(
      "t:audit.actions.stack.deployed_rollback",
    );
    expect(actionLabel("stack.deployed", echo, { mode: "deploy", stack: "web" })).toBe(
      "t:audit.actions.stack.deployed",
    );
    expect(actionLabel("stack.deployed", echo)).toBe("t:audit.actions.stack.deployed");
    // The mode of another action says nothing.
    expect(actionLabel("stack.stopped", echo, rollback)).toBe("t:audit.actions.stack.stopped");
    expect(audit.audit.actions.stack.deployed_rollback).toBe(
      "Rolled back stack {{name}} to revision #{{revision}}",
    );
  });

  it("labels a restore of a stack from drift, and nothing else that has a mode", () => {
    const restore = { mode: "restore", stack: "web", restoredFrom: "4" };
    expect(actionLabel("stack.deployed", echo, restore)).toBe(
      "t:audit.actions.stack.deployed_restore",
    );
    expect(actionLabel("stack.stopped", echo, restore)).toBe("t:audit.actions.stack.stopped");
    expect(audit.audit.actions.stack.deployed_restore).toBe("Restored stack {{name}} from drift");
  });

  it("labels accepting live state into the draft with the stack's name when the entry has it", () => {
    expect(actionLabel("stack.drift_accepted", echo, { stack: "web", revision: "4" })).toBe(
      "t:audit.actions.stack.drift_accepted_named",
    );
    expect(actionLabel("stack.drift_accepted", echo)).toBe("t:audit.actions.stack.drift_accepted");
    expect(KNOWN_ACTIONS).toContain("stack.drift_accepted");
    expect(audit.audit.actions.stack.drift_accepted_named).toBe(
      "Accepted live state into the draft of {{name}}",
    );
    expect(audit.audit.actions.stack.drift_accepted).toBe("Accepted live state into the draft");
  });

  it("labels a failed git sync with the stack and the reason when the entry has them", () => {
    expect(KNOWN_ACTIONS).toContain("git.sync_failed");
    expect(
      actionLabel("git.sync_failed", echo, { stack: "web", reason: "The branch is gone." }),
    ).toBe("t:audit.actions.git.sync_failed_named");
    expect(actionLabel("git.sync_failed", echo)).toBe("t:audit.actions.git.sync_failed");
    expect(audit.audit.actions.git.sync_failed_named).toBe("Sync of {{name}} failed: {{reason}}");
  });

  it("labels a change of a git stack's update checks with the stack when the entry has it", () => {
    expect(KNOWN_ACTIONS).toContain("git.updates_changed");
    expect(actionLabel("git.updates_changed", echo, { stack: "web" })).toBe(
      "t:audit.actions.git.updates_changed_named",
    );
    expect(actionLabel("git.updates_changed", echo)).toBe("t:audit.actions.git.updates_changed");
    expect(audit.audit.actions.git.updates_changed_named).toBe(
      "Changed how {{name}} checks for updates",
    );
  });

  it("labels a git webhook's deliveries and rejections with the stack when the entry has it", () => {
    for (const key of ["git.webhook_delivered", "git.webhook_rejected"] as const) {
      expect(KNOWN_ACTIONS).toContain(key);
      expect(actionLabel(key, echo, { stack: "web" })).toBe(`t:audit.actions.${key}_named`);
      expect(actionLabel(key, echo)).toBe(`t:audit.actions.${key}`);
    }
    expect(audit.audit.actions.git.webhook_delivered_named).toBe(
      "Webhook for {{name}} delivered a push",
    );
    expect(audit.audit.actions.git.webhook_rejected_named).toBe(
      "Webhook for {{name}} rejected a request",
    );
  });

  it("labels the creation, rotation and removal of a git webhook with the stack when the entry has it", () => {
    for (const key of [
      "git.webhook_created",
      "git.webhook_rotated",
      "git.webhook_removed",
    ] as const) {
      expect(KNOWN_ACTIONS).toContain(key);
      expect(actionLabel(key, echo, { stack: "web" })).toBe(`t:audit.actions.${key}_named`);
      expect(actionLabel(key, echo)).toBe(`t:audit.actions.${key}`);
    }
    expect(audit.audit.actions.git.webhook_created_named).toBe("Created a webhook for {{name}}");
    expect(audit.audit.actions.git.webhook_rotated_named).toBe("Rotated the webhook of {{name}}");
    expect(audit.audit.actions.git.webhook_removed_named).toBe("Removed the webhook of {{name}}");
  });

  it("labels a git stack's auto-deploy with the stack, and an automatic deploy with its commit", () => {
    for (const key of [
      "git.autodeploy_enabled",
      "git.autodeploy_disabled",
      "git.autodeploy_paused",
      "git.stack_detached",
    ] as const) {
      expect(KNOWN_ACTIONS).toContain(key);
      expect(actionLabel(key, echo, { stack: "web" })).toBe(`t:audit.actions.${key}_named`);
      expect(actionLabel(key, echo)).toBe(`t:audit.actions.${key}`);
    }
    expect(KNOWN_ACTIONS).toContain("git.autodeployed");
    expect(
      actionLabel("git.autodeployed", echo, { stack: "web", commit: "0123456789abcdef" }),
    ).toBe("t:audit.actions.git.autodeployed_named");
    expect(actionLabel("git.autodeployed", echo, { stack: "web" })).toBe(
      "t:audit.actions.git.autodeployed",
    );
    expect(audit.audit.actions.git.autodeployed_named).toBe("Auto-deployed {{name}} at {{sha}}");
    expect(audit.audit.actions.git.autodeploy_enabled_named).toBe(
      "Turned on auto-deploy for {{name}}",
    );
  });

  it("has a label in audit.json for every known action", () => {
    for (const key of KNOWN_ACTIONS) {
      const [area, name] = key.split(".") as [string, string];
      const labels = audit.audit.actions as Record<string, Record<string, string>>;
      expect(labels[area]?.[name], key).toBeTruthy();
    }
  });
});

describe("the agent and environment lifecycle actions (D-13)", () => {
  const D13 = [
    "agent.enrolled",
    "agent.enrol_refused",
    "agent.cert_renewed",
    "agent.revoked",
    "agent.blocked",
    "agent.unblocked",
    "agent.auto_revoked",
    "agent.updated",
    "agent.update_failed",
    "agent.uninstalled",
    "agent.policy_denied",
    "environment.token_rotated",
    "environment.token_disabled",
    "environment.failover",
    "environment.cluster_adopted",
    "environment.local_folded",
    "environment.auto_update_changed",
  ];

  it("lists every one in the Action filter", () => {
    for (const key of D13) expect(KNOWN_ACTIONS, key).toContain(key);
  });

  it("names every one with its own audit.json label, not the raw key", () => {
    const labels = audit.audit.actions as Record<string, Record<string, string>>;
    for (const key of D13) {
      const [area, name] = key.split(".") as [string, string];
      expect(labels[area]?.[name], key).toBeTruthy();
      expect(actionLabel(key, echo)).toBe(`t:audit.actions.${key}`);
    }
  });
});

// The labels of UI-SPEC "Audit log changes", plus the two actions the backend added
// after it (automation.resumed, backup.schedule_updated). Written out here, not
// read back from audit.json, so a label that drifts fails.
const PHASE3_LABELS: Record<string, string> = {
  "user.created": "Created user",
  "user.updated": "Updated user",
  "user.disabled": "Disabled user",
  "user.enabled": "Enabled user",
  "user.deleted": "Deleted user",
  "user.invite_created": "Created invite link",
  "user.invite_regenerated": "Regenerated invite link",
  "user.invite_accepted": "Accepted invite",
  "user.password_reset_issued": "Issued password reset link",
  "user.password_reset_completed": "Completed password reset",
  "user.admin_granted": "Made instance admin",
  "user.admin_revoked": "Removed instance admin",
  "session.revoked": "Revoked session",
  "user.sessions_revoked": "Ended all sessions",
  "team.created": "Created team",
  "team.renamed": "Renamed team",
  "team.deleted": "Deleted team",
  "team.member_added": "Added team member",
  "team.member_removed": "Removed team member",
  "access.granted": "Granted access",
  "access.updated": "Changed access",
  "access.revoked": "Removed access",
  "owner.transferred": "Transferred ownership",
  "automation.paused": "Paused automation",
  "automation.resumed": "Resumed automation",
  "backup.exported": "Exported backup",
  "backup.scheduled_run": "Ran scheduled backup",
  "backup.failed": "Backup failed",
  "backup.checked": "Checked backup file",
  "backup.restored": "Restored backup",
  "backup.schedule_updated": "Changed backup schedule",
  "backup.upload_discarded": "Discarded uploaded backup",
  "audit.reanchored": "Re-anchored audit log",
  "key.rotated": "Rotated master key",
};

describe("the Phase 3 actions", () => {
  it("lists every one in the Action filter", () => {
    for (const key of Object.keys(PHASE3_LABELS)) expect(KNOWN_ACTIONS, key).toContain(key);
  });

  it("gives every one the label of the UI-SPEC, looked up by its own key", () => {
    const labels = audit.audit.actions as Record<string, Record<string, string>>;
    for (const [key, label] of Object.entries(PHASE3_LABELS)) {
      const [area, name] = key.split(".") as [string, string];
      expect(labels[area]?.[name], key).toBe(label);
      expect(actionLabel(key, echo)).toBe(`t:audit.actions.${key}`);
    }
  });

  it("still shows an unknown key as itself", () => {
    expect(actionLabel("user.teleported", echo)).toBe("user.teleported");
  });
});

describe("ENV_SCOPED_ACTIONS", () => {
  it("holds exactly the actions that can carry an environment, all of them known", () => {
    for (const key of ENV_SCOPED_ACTIONS) expect(KNOWN_ACTIONS, key).toContain(key);
    for (const key of [
      "permission.denied",
      "access.granted",
      "access.updated",
      "access.revoked",
      "environment.renamed",
      "environment.failover",
      "agent.enrolled",
      "agent.revoked",
      "automation.paused",
      "automation.resumed",
      "owner.transferred",
    ]) {
      expect(ENV_SCOPED_ACTIONS, key).toContain(key);
    }
  });

  it("leaves out the instance-level events an env Admin never reads", () => {
    for (const key of [
      "auth.login",
      "auth.login_failed",
      "setup.completed",
      "settings.updated",
      "user.created",
      "user.deleted",
      "team.created",
      "session.revoked",
      "backup.exported",
      "audit.reanchored",
      "audit.pruned",
      "key.rotated",
      "warning.acknowledged",
    ]) {
      expect(ENV_SCOPED_ACTIONS, key).not.toContain(key);
    }
  });
});

describe("exportUrl and listParams", () => {
  it("builds the export URL with exactly the filters the list uses", () => {
    const search = auditSearchSchema.parse({ user: "admin", action: ["auth.login"], range: "1h" });
    const list = new URLSearchParams(listParams(search, NOW));
    const url = new URL(exportUrl("csv", search, NOW), "http://tierplan.test");
    expect(url.pathname).toBe("/api/audit/export");
    expect(url.searchParams.get("format")).toBe("csv");
    const filters = new URLSearchParams(url.searchParams);
    filters.delete("format");
    expect([...filters.entries()].sort()).toEqual([...list.entries()].sort());
    expect(filters.get("from")).toBe("2026-10-02T11:00:00.000Z");
  });

  it("puts the served base path in front of the export URL", () => {
    const meta = document.createElement("meta");
    meta.name = "tierplan-base";
    meta.content = "/tierplan";
    document.head.append(meta);
    resetBasePathForTests();
    try {
      const search = auditSearchSchema.parse({ user: "admin", range: "1h" });
      const url = new URL(exportUrl("csv", search, NOW), "http://tierplan.test");
      expect(url.pathname).toBe("/tierplan/api/audit/export");
      expect(url.searchParams.get("format")).toBe("csv");
      expect(url.searchParams.get("user")).toBe("admin");
    } finally {
      meta.remove();
      resetBasePathForTests();
    }
  });

  it("asks for JSON and sends no filters when there are none", () => {
    const url = new URL(exportUrl("json", { range: "all" }, NOW), "http://tierplan.test");
    expect(url.search).toBe("?format=json");
  });

  it("leaves a cursor and a page size out of the list filters", () => {
    const list = new URLSearchParams(listParams({ range: "all" }, NOW));
    expect(list.has("cursor")).toBe(false);
    expect(list.has("limit")).toBe(false);
  });
});

describe("formatDay and parseDay", () => {
  it("write and read a calendar day of the browser's time zone", () => {
    const date = new Date(2026, 8, 3, 15, 30);
    expect(formatDay(date)).toBe("2026-09-03");
    expect(parseDay("2026-09-03")).toEqual(new Date(2026, 8, 3));
    expect(parseDay(formatDay(date))).toEqual(new Date(2026, 8, 3));
  });

  it("read nothing from a missing or impossible day", () => {
    expect(parseDay(undefined)).toBeUndefined();
    expect(parseDay("2026-13-01")).toBeUndefined();
    expect(parseDay("not a day")).toBeUndefined();
  });
});

describe("the bulk filter", () => {
  const BULK = "018f0000-0000-7000-8000-000000000001";

  it("reads a bulk ID from the URL and sends it to the list and the export", () => {
    const parsed = auditSearchSchema.parse(fromUrl(`bulk=${BULK}`));
    expect(parsed.bulk).toBe(BULK);
    expect(listParams(parsed, NOW).get("bulk")).toBe(BULK);
    expect(new URL(exportUrl("csv", parsed, NOW), "http://x").searchParams.get("bulk")).toBe(BULK);
    expect(auditSearchSchema.parse(parsed)).toEqual(parsed);
  });

  it("sends no bulk parameter when there is none", () => {
    expect(listParams(auditSearchSchema.parse({}), NOW).has("bulk")).toBe(false);
  });

  it("drops a value that is not a UUID instead of sending it", () => {
    for (const bad of ["nope", "123", "018f0000-0000-7000-8000-00000000000g", "x' OR '1'='1", ""]) {
      const parsed = auditSearchSchema.parse({ bulk: bad });
      expect(parsed.bulk).toBeUndefined();
      expect(listParams(parsed, NOW).has("bulk")).toBe(false);
    }
  });
});

describe("the service.created action (06-27)", () => {
  it("is in the Action filter with its own label", () => {
    expect(KNOWN_ACTIONS).toContain("service.created");
    const labels = audit.audit.actions as Record<string, Record<string, string>>;
    expect(labels.service?.created).toBe("Service created");
  });
});

describe("the secret.rotated and config.rotated actions (06-28)", () => {
  it("are in the Action filter with their own labels", () => {
    expect(KNOWN_ACTIONS).toContain("secret.rotated");
    expect(KNOWN_ACTIONS).toContain("config.rotated");
    const labels = audit.audit.actions as Record<string, Record<string, string>>;
    expect(labels.secret?.rotated).toBe("Secret rotated");
    expect(labels.config?.rotated).toBe("Config rotated");
  });
});

describe("the stack.adopted action (08-13)", () => {
  it("is in the Action filter, scoped to an environment, with its label", () => {
    expect(KNOWN_ACTIONS).toContain("stack.adopted");
    expect(ENV_SCOPED_ACTIONS).toContain("stack.adopted");
    const labels = audit.audit.actions as Record<string, Record<string, string>>;
    expect(labels.stack?.adopted).toBe("Stack adopted");
  });
});

describe("the stack actions (07-01)", () => {
  it("are in the Action filter, scoped to an environment, with their own labels", () => {
    for (const key of [
      "stack.created",
      "stack.draft_saved",
      "stack.draft_discarded",
      "stack.deployed",
      "stack.stopped",
      "stack.started",
      "stack.removed",
      "stack.value_revealed",
    ]) {
      expect(KNOWN_ACTIONS, key).toContain(key);
      expect(ENV_SCOPED_ACTIONS, key).toContain(key);
    }
    const labels = audit.audit.actions as Record<string, Record<string, string>>;
    expect(labels.stack?.created).toBe("Stack created");
    expect(labels.stack?.draft_saved).toBe("Stack draft saved");
    expect(labels.stack?.draft_discarded).toBe("Stack draft discarded");
    expect(labels.stack?.deployed).toBe("Stack deployed");
    expect(labels.stack?.stopped).toBe("Stack stopped");
    expect(labels.stack?.started).toBe("Stack started");
    expect(labels.stack?.removed).toBe("Stack removed");
    expect(labels.stack?.value_revealed).toBe("Stack secret value revealed");
  });
});

describe("actionLabel (HTTPS)", () => {
  const real = (key: string) => {
    const parts = key.split(".");
    let node: unknown = audit;
    for (const part of parts) node = (node as Record<string, unknown>)?.[part];
    return typeof node === "string" ? node : key;
  };
  const HTTPS_ACTIONS = [
    "https.account_registered",
    "https.account_forgotten",
    "https.settings_updated",
    "https.issue_requested",
    "https.renew_requested",
    "https.issued",
    "https.renewed",
    "https.run_failed",
    "https.resumed",
    "https.stopped",
    "https.dns_tested",
    "https.fallback_started",
    "https.fallback_ended",
    "https.hsts_withdrawn",
    "https.hsts_restored",
  ];

  it("labels every HTTPS action and lists it in the Action filter", () => {
    for (const key of HTTPS_ACTIONS) {
      expect(KNOWN_ACTIONS, key).toContain(key);
      expect(actionLabel(key, real), key).not.toBe(key);
    }
    expect(actionLabel("https.issued", real)).toBe("Issued Let's Encrypt certificate");
    expect(actionLabel("https.run_failed", real)).toBe("Let's Encrypt run failed");
  });

  it("names the section a settings change was about", () => {
    expect(actionLabel("https.settings_updated", real, { section: "dns_credential" })).toBe(
      "Changed the Cloudflare token",
    );
    expect(actionLabel("https.settings_updated", real, { section: "validation" })).toBe(
      "Changed the validation method",
    );
    expect(actionLabel("https.settings_updated", real, { section: "profile" })).toBe(
      "Changed the certificate profile",
    );
    expect(actionLabel("https.settings_updated", real, { section: "hsts" })).toBe("Changed HSTS");
  });

  it("reads the base label for an unknown or absent section", () => {
    expect(actionLabel("https.settings_updated", real, { section: "mystery" })).toBe(
      "Changed HTTPS settings",
    );
    expect(actionLabel("https.settings_updated", real)).toBe("Changed HTTPS settings");
  });

  it("is an instance event: no HTTPS action is environment-scoped", () => {
    for (const key of HTTPS_ACTIONS) expect(ENV_SCOPED_ACTIONS, key).not.toContain(key);
  });
});

describe("actionLabel (agent replaced)", () => {
  const real = (key: string) => {
    const parts = key.split(".");
    let node: unknown = audit;
    for (const part of parts) node = (node as Record<string, unknown>)?.[part];
    return typeof node === "string" ? node : key;
  };

  it("says an agent retired by its host's new enrolment was replaced", () => {
    expect(actionLabel("agent.revoked", real, { reason: "replaced", node: "edge-1" })).toBe(
      "Agent replaced (the host enrolled again)",
    );
  });

  it("keeps the plain label for every other revoke", () => {
    expect(actionLabel("agent.revoked", real, { reason: "node_removed" })).toBe("Agent revoked");
    expect(actionLabel("agent.revoked", real)).toBe("Agent revoked");
  });
});
