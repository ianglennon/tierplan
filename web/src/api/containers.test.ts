// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { ActionRequestError, runBulk } from "@/api/containers";

interface Sent {
  path: string;
  body: { targets: { id: string }[]; bulkId?: string; [k: string]: unknown };
}

let sent: Sent[];
let answer: (n: number, req: Sent) => Response;

const targets = (n: number) => Array.from({ length: n }, (_, i) => ({ nodeId: "n1", id: `c${i}` }));

beforeEach(() => {
  sent = [];
  setCsrfToken(null);
  answer = (_n, req) =>
    Response.json({
      bulkId: "018f0000-0000-7000-8000-000000000001",
      results: req.body.targets.map((t) => ({
        nodeId: "n1",
        id: t.id,
        name: t.id,
        result: "done",
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

describe("runBulk", () => {
  it("sends 30 targets as 25 then 5, the second carrying the bulk ID the first was answered with", async () => {
    const results = await runBulk("e1", { kind: "action", action: "stop" }, targets(30));
    expect(sent.map((s) => s.body.targets.length)).toEqual([25, 5]);
    expect(sent[0]?.body.bulkId).toBeUndefined();
    expect(sent[1]?.body.bulkId).toBe("018f0000-0000-7000-8000-000000000001");
    expect(sent.every((s) => s.path === "/api/env/e1/containers/actions")).toBe(true);
    expect(results.map((r) => r.id)).toEqual(targets(30).map((t) => t.id));
  });

  it("sends 25 or fewer in one request with no bulk ID", async () => {
    await runBulk("e1", { kind: "action", action: "start" }, targets(25));
    expect(sent).toHaveLength(1);
    expect(sent[0]?.body.bulkId).toBeUndefined();
  });

  it("sends a remove to the remove route with its options", async () => {
    await runBulk("e1", { kind: "remove", force: true, removeVolumes: false }, targets(2));
    expect(sent[0]?.path).toBe("/api/env/e1/containers/remove");
    expect(sent[0]?.body).toMatchObject({ force: true, removeVolumes: false });
  });

  it("throws when the first chunk is refused as a whole, and keeps the problem type", async () => {
    answer = () => Response.json({ status: 422, type: "confirmation_required" }, { status: 422 });
    const err = await runBulk("e1", { kind: "action", action: "stop" }, targets(30)).catch(
      (e) => e,
    );
    expect(err).toBeInstanceOf(ActionRequestError);
    expect((err as ActionRequestError).status).toBe(422);
    expect((err as ActionRequestError).type).toBe("confirmation_required");
    expect(sent).toHaveLength(1);
  });

  it("answers the targets of a later chunk that is refused as failed and keeps the earlier results", async () => {
    answer = (n, req) =>
      n === 1
        ? Response.json({
            bulkId: "018f0000-0000-7000-8000-000000000001",
            results: req.body.targets.map((t) => ({
              nodeId: "n1",
              id: t.id,
              name: t.id,
              result: "done",
            })),
          })
        : Response.json({ status: 500 }, { status: 500 });
    const results = await runBulk("e1", { kind: "action", action: "stop" }, targets(30));
    expect(results).toHaveLength(30);
    expect(results.slice(0, 25).every((r) => r.result === "done")).toBe(true);
    expect(results.slice(25).every((r) => r.result === "failed")).toBe(true);
  });
});
