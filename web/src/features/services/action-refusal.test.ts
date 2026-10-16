// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import { ServiceActionError } from "@/api/services";
import { refusalOf } from "@/features/services/action-refusal";
import i18n from "@/i18n";

// The words for a refused force update or rollback (plan 06-48). The translate function is the
// real one, so a key that is missing fails here.
const t = (key: string, options?: Record<string, unknown>) =>
  String(i18n.t(key, { ...options, ns: key.split(".")[0] }));

const ctx = { environment: "Home", image: "nginx:1", action: "update" } as const;

// A caller without the reveal permission who adds a secret the service does not use.
const entries = [{ location: "TaskTemplate.ContainerSpec.Secrets[0]", message: "db-pass" }];
const revealRequired = () =>
  new ServiceActionError(
    403,
    "secret_reveal_required",
    new Map(entries.map((f) => [f.location, f.message])),
    "Adding a secret to a service needs the reveal secrets permission: db-pass. Ask an administrator to add it.",
    entries,
  );

describe("refusalOf secret_reveal_required", () => {
  it("names the secrets and says adding one needs the reveal permission, not the generic sentence", () => {
    const r = refusalOf(t, revealRequired(), ctx);
    expect(r.message).toBe(
      "Can't add db-pass: adding a secret to a service needs the reveal secrets permission. Ask an administrator to add it.",
    );
    expect(r.message).not.toContain("no longer have permission");
    expect(r.agentPolicy).toBe(false);
  });

  it("words a rollback by what the earlier version uses", () => {
    const r = refusalOf(t, revealRequired(), { ...ctx, action: "rollback" });
    expect(r.message).toBe(
      "Can't roll back: the earlier version uses db-pass, which this service doesn't use now, and adding a secret needs the reveal secrets permission. Ask an administrator to roll it back.",
    );
  });

  it("joins several secrets", () => {
    const many = [
      ...entries,
      { location: "TaskTemplate.ContainerSpec.Secrets[1]", message: "api-key" },
    ];
    const e = new ServiceActionError(403, "secret_reveal_required", new Map(), undefined, many);
    expect(refusalOf(t, e, ctx).message).toContain("db-pass, api-key");
  });
});
