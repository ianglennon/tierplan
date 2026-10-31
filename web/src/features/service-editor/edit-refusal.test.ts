// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { ServiceActionError } from "@/api/services";
import { editRefusalOf } from "@/features/service-editor/edit-refusal";
import i18n from "@/i18n";

// The sentence of a refused review or apply (UI-SPEC "Shared refusals and failures"). The
// translate function is the real one, so a key that is missing fails here.
const t = (key: string, options?: Record<string, unknown>) =>
  String(i18n.t(key, { ...options, ns: key.split(".")[0] }));

const missing = (...entries: [string, string][]) =>
  new ServiceActionError(
    422,
    "objects_missing",
    new Map(entries),
    undefined,
    entries.map(([location, message]) => ({ location, message })),
  );

const ctx = { environment: "Home", image: "nginx:1" } as const;

describe("editRefusalOf objects_missing", () => {
  it("names a missing secret and no network", () => {
    const r = editRefusalOf(t, missing(["secrets", "db-pass"]), ctx);
    expect(r.message).toBe(
      "The spec names objects that don't exist: secrets db-pass. Create them first, then review again.",
    );
    expect(r.message).not.toContain("networks");
  });

  it("names every missing kind with its names, in the order networks, secrets, configs", () => {
    const r = editRefusalOf(
      t,
      missing(
        ["networks", "front"],
        ["secrets", "db-pass"],
        ["secrets", "api-key"],
        ["configs", "app.conf"],
      ),
      ctx,
    );
    expect(r.message).toBe(
      "The spec names objects that don't exist: networks front; secrets db-pass, api-key; configs app.conf. Create them first, then review again.",
    );
  });

  it("words a create the same way", () => {
    const r = editRefusalOf(t, missing(["configs", "app.conf"]), { ...ctx, action: "create" });
    expect(r.message).toBe(
      "The spec names objects that don't exist: configs app.conf. Create them first, then review again.",
    );
  });
});

describe("editRefusalOf an overlap the swarm refuses", () => {
  it("shows the server's sentence, which names the network and says why", () => {
    const detail =
      "10.77.0.0/25 overlaps swarm network existing (10.77.0.0/24). A swarm can't give two of its networks overlapping subnets, so it can't be created anyway. Choose another subnet.";
    const r = editRefusalOf(
      t,
      new ServiceActionError(422, "subnet_overlap_swarm", new Map(), detail),
      ctx,
    );
    expect(r.message).toBe(detail);
  });
});
