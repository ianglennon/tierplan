// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import type { Credential } from "@/api/registries";
import { secretDestinationChanged } from "@/features/registries/secret-destination";

const stored = {
  type: "generic",
  host: "registry.example.com:5000",
  username: "deploy",
  caPem: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----",
  insecure: true,
} as Credential;

const same = {
  type: stored.type,
  host: stored.host,
  username: stored.username,
  caPem: stored.caPem,
  insecure: stored.insecure,
};

describe("secretDestinationChanged", () => {
  it("is false when nothing the secret is sent to differs, ignoring surrounding spaces", () => {
    expect(secretDestinationChanged(stored, same)).toBe(false);
    expect(
      secretDestinationChanged(stored, {
        ...same,
        host: ` ${stored.host} `,
        username: " deploy ",
        caPem: `${stored.caPem}\n`,
      }),
    ).toBe(false);
  });

  it.each([
    ["host", { host: "other.example.com" }],
    ["type", { type: "ghcr" as const }],
    ["username", { username: "someone-else" }],
    ["a different CA", { caPem: "-----BEGIN CERTIFICATE-----\nxyz\n-----END CERTIFICATE-----" }],
    ["a cleared CA", { caPem: "" }],
    ["insecure switched off", { insecure: false }],
  ])("is true for %s", (_name, change) => {
    expect(secretDestinationChanged(stored, { ...same, ...change })).toBe(true);
  });
});
