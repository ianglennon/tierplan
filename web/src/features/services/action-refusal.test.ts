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

// The 422 of plan 06-52: a previous spec (or an edited one) with a secret reference whose name is
// not the name of the secret its ID points at.
const mismatch =
  "TaskTemplate.ContainerSpec.Secrets[0]: the SecretName victim-secret is not the name of the secret its SecretID points at";
const specInvalid = (detail?: string, specEntries = [{ location: "spec", message: mismatch }]) =>
  new ServiceActionError(422, "spec_invalid", new Map(), detail, specEntries);

describe("refusalOf spec_invalid", () => {
  it("words a rollback with the server's message and who can fix it, not the generic sentence", () => {
    const r = refusalOf(t, specInvalid(), { ...ctx, action: "rollback" });
    expect(r.message).toBe(
      `Can't roll back: the earlier version can't be sent as it is (${mismatch}). Ask an administrator to correct the service.`,
    );
    expect(r.message).not.toContain("Something went wrong");
    expect(r.agentPolicy).toBe(false);
  });

  it("words an update with the same message", () => {
    expect(refusalOf(t, specInvalid(), ctx).message).toBe(
      `The change can't be sent as it is: ${mismatch}.`,
    );
  });

  it("falls back to the detail when no entry is at spec", () => {
    const r = refusalOf(t, specInvalid("The service spec can't be read", []), ctx);
    expect(r.message).toBe("The change can't be sent as it is: The service spec can't be read.");
  });

  it("is the generic sentence with neither an entry nor a detail", () => {
    const r = refusalOf(t, specInvalid(undefined, []), ctx);
    expect(r.message).toContain("Something went wrong on the server");
  });
});
