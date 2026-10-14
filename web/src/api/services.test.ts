// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { postScale, ScaleRequestError } from "@/api/services";

interface Sent {
  path: string;
  body: { targets: { serviceId: string; replicas: number }[]; bulkId?: string };
}

let sent: Sent[];
let answer: (n: number, req: Sent) => Response;

const targets = (n: number) =>
  Array.from({ length: n }, (_, i) => ({ serviceId: `svc${i}`, replicas: 2 }));

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
  answer = (_n, req) =>
    Response.json({
      results: req.body.targets.map((t) => ({
        serviceId: t.serviceId,
        name: t.serviceId,
        result: "done",
        replicas: t.replicas,
        changed: true,
      })),
    });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = { path: new URL(req.url).pathname, body: await req.clone().json() };
      sent.push(entry);
      return answer(sent.length, entry);
    }),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("postScale", () => {
  it("sends up to 100 targets in one request with the bulk ID it was given", async () => {
    const results = await postScale("e1", { targets: targets(100), bulkId: "b-1" });
    expect(sent).toHaveLength(1);
    expect(sent[0]?.path).toBe("/api/env/e1/services/scale");
    expect(sent[0]?.body.bulkId).toBe("b-1");
    expect(results).toHaveLength(100);
  });

  it("sends 250 targets as 100, 100 and 50 that all carry the same bulk ID, answered in order", async () => {
    const results = await postScale("e1", { targets: targets(250), bulkId: "b-1" });
    expect(sent.map((s) => s.body.targets.length)).toEqual([100, 100, 50]);
    expect(sent.every((s) => s.body.bulkId === "b-1")).toBe(true);
    expect(results.map((r) => r.serviceId)).toEqual(targets(250).map((t) => t.serviceId));
  });

  it("throws when the first request is refused as a whole", async () => {
    answer = () => Response.json({ status: 403, title: "Forbidden" }, { status: 403 });
    await expect(postScale("e1", { targets: targets(250), bulkId: "b-1" })).rejects.toBeInstanceOf(
      ScaleRequestError,
    );
    expect(sent).toHaveLength(1);
  });

  it("answers the targets of a later request that is refused as failed and keeps the earlier ones", async () => {
    const ok = answer;
    answer = (n, req) =>
      n === 2 ? Response.json({ status: 500, title: "Server error" }, { status: 500 }) : ok(n, req);
    const results = await postScale("e1", { targets: targets(150), bulkId: "b-1" });
    expect(results).toHaveLength(150);
    expect(results.slice(0, 100).every((r) => r.result === "done")).toBe(true);
    expect(
      results.slice(100).every((r) => r.result === "failed" && r.code === "scale_failed"),
    ).toBe(true);
  });
});
