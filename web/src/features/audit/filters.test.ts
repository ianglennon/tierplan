// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

describe("actionLabel", () => {
  it("looks up the human label of a known action", () => {
    expect(actionLabel("auth.login", echo)).toBe("t:audit.actions.auth.login");
    expect(audit.audit.actions.auth.login).toBe("Signed in");
  });

  it("falls back to the raw key for an action the UI does not know", () => {
    expect(actionLabel("something.new", echo)).toBe("something.new");
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
    const url = new URL(exportUrl("csv", search, NOW), "http://bayplan.test");
    expect(url.pathname).toBe("/api/audit/export");
    expect(url.searchParams.get("format")).toBe("csv");
    const filters = new URLSearchParams(url.searchParams);
    filters.delete("format");
    expect([...filters.entries()].sort()).toEqual([...list.entries()].sort());
    expect(filters.get("from")).toBe("2026-10-02T11:00:00.000Z");
  });

  it("puts the served base path in front of the export URL", () => {
    const meta = document.createElement("meta");
    meta.name = "bayplan-base";
    meta.content = "/bayplan";
    document.head.append(meta);
    resetBasePathForTests();
    try {
      const search = auditSearchSchema.parse({ user: "admin", range: "1h" });
      const url = new URL(exportUrl("csv", search, NOW), "http://bayplan.test");
      expect(url.pathname).toBe("/bayplan/api/audit/export");
      expect(url.searchParams.get("format")).toBe("csv");
      expect(url.searchParams.get("user")).toBe("admin");
    } finally {
      meta.remove();
      resetBasePathForTests();
    }
  });

  it("asks for JSON and sends no filters when there are none", () => {
    const url = new URL(exportUrl("json", { range: "all" }, NOW), "http://bayplan.test");
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
