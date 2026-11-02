// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { failureCopy } from "@/features/pulls/failure-copy";
import i18n from "@/i18n";

// The pull rows' failure sentences (UI-SPEC "Shared refusals and failures", plan 05-09). The
// translate function is the real one, so a key that is missing fails here.
const t = (key: string, options?: Record<string, unknown>) =>
  String(i18n.t(key, { ...options, ns: key.split(".")[0] }));

const REF = "ghcr.io/acme/app:1";
const base = { ref: REF, node: "node-1", credential: null } as const;
const cred = { id: "c-1", name: "ghcr-acme", layer: "instance" } as const;

describe("failureCopy", () => {
  it("names the credential and offers to test it when a pull with one was refused", () => {
    for (const code of ["pull_denied", "pull_unauthorized"]) {
      const c = failureCopy(t, code, { ...base, credential: cred });
      expect(c.text).toBe(
        "Pull denied using credential “ghcr-acme” (Instance). Check its scopes or test it.",
      );
      expect(c.action).toEqual({
        kind: "test",
        label: "Test credential",
        to: "/registries",
        search: { credential: "c-1" },
      });
    }
  });

  it("says an environment credential's scope by the environment's name", () => {
    const c = failureCopy(t, "pull_denied", {
      ...base,
      credential: { id: "c-2", name: "team", layer: "environment" },
      envName: "Production",
    });
    expect(c.text).toBe(
      "Pull denied using credential “team” (Production). Check its scopes or test it.",
    );
  });

  it("says nothing matched and offers to add one when an anonymous pull was refused", () => {
    const c = failureCopy(t, "pull_denied", base);
    expect(c.text).toBe(
      "No stored credential matches ghcr.io/acme/app:1, so it was pulled anonymously. Add one.",
    );
    expect(c.action).toEqual({
      kind: "add",
      label: "Add credential",
      to: "/registries",
      search: { credential: "new" },
    });
  });

  it("words the other codes from the UI-SPEC and gives them no action", () => {
    const hub = { ...base, ref: "docker.io/library/nginx:1" };
    expect(failureCopy(t, "image_not_found", base).text).toBe(
      "ghcr.io/acme/app:1 wasn't found on ghcr.io. Check the name and the tag.",
    );
    expect(failureCopy(t, "rate_limited", hub).text).toBe(
      "Docker Hub's pull limit was reached. Add a Docker Hub credential and tick “Use for all Docker Hub pulls”, or try again later.",
    );
    expect(failureCopy(t, "rate_limited", base).text).toBe(
      "ghcr.io's pull limit was reached. Try again later.",
    );
    expect(failureCopy(t, "registry_unreachable", base).text).toBe(
      "node-1 couldn't reach ghcr.io. Check its network, DNS and firewall.",
    );
    expect(failureCopy(t, "registry_tls", base).text).toMatch(/certs\.d.*insecure-registries/);
    expect(failureCopy(t, "cancelled", base).text).toBe("Cancelled before it finished on node-1.");
    for (const code of [
      "image_not_found",
      "rate_limited",
      "registry_unreachable",
      "registry_tls",
      "cancelled",
    ])
      expect(failureCopy(t, code, base).action).toBeUndefined();
  });

  it("words the masked plain-HTTP retry with the node and the registry, never as a node fault", () => {
    const c = failureCopy(t, "registry_http_fallback", base);
    expect(c.text).toBe(
      "node-1 retried ghcr.io over plain HTTP, because the name resolves to a loopback address or is listed in insecure-registries, and that answer hid the real one. Check the name, the tag and the sign-in.",
    );
    expect(c.text).not.toContain("Check the node");
    expect(c.action).toBeUndefined();
  });

  it("gives an unknown code the generic sentence and never shows the code", () => {
    const c = failureCopy(t, "weird_new_code", base);
    expect(c.text).toBe("The pull failed on node-1. Check the node and try again.");
    expect(c.text).not.toContain("weird_new_code");
  });
});
