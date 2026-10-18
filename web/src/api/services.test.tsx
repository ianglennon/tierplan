// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  postScale,
  ScaleRequestError,
  ServiceActionError,
  servicesKey,
  useApplyServiceUpdate,
  useReviewServiceUpdate,
} from "@/api/services";
import { reviewBody } from "@/test-service";

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

describe("the service editor's requests", () => {
  const spec = { Name: "web", Mode: { Replicated: { Replicas: 4 } } };
  let client: QueryClient;
  let posted: { path: string; body: unknown }[];
  let reply: () => Response | Promise<Response>;

  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );

  beforeEach(() => {
    client = new QueryClient({ defaultOptions: { mutations: { retry: 3 } } });
    posted = [];
    reply = () => Response.json(reviewBody());
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        posted.push({ path: new URL(req.url).pathname, body: await req.clone().json() });
        return reply();
      }),
    );
  });

  afterEach(() => client.clear());

  it("posts the version and the spec to the review and answers what the server said", async () => {
    const { result } = renderHook(() => useReviewServiceUpdate("e1"), { wrapper });
    const answer = await result.current.mutateAsync({ serviceId: "svc1", version: 7, spec });
    expect(posted).toEqual([
      { path: "/api/env/e1/services/svc1/review", body: { version: 7, spec } },
    ]);
    expect(answer.version).toBe(7);
  });

  it("posts the version and the spec to the apply", async () => {
    reply = () =>
      Response.json({ image: "web:2", digest: "", pinned: false, warnings: [] }, { status: 202 });
    const { result } = renderHook(() => useApplyServiceUpdate("e1"), { wrapper });
    const answer = await result.current.mutateAsync({ serviceId: "svc1", version: 7, spec });
    expect(posted).toEqual([
      { path: "/api/env/e1/services/svc1/apply", body: { version: 7, spec } },
    ]);
    expect(answer.image).toBe("web:2");
  });

  it.each([
    ["review", useReviewServiceUpdate],
    ["apply", useApplyServiceUpdate],
  ] as const)("reads the problem of a refused %s, and does not try again", async (_name, hook) => {
    reply = () =>
      Response.json(
        { type: "service_changed", title: "Conflict", status: 409, detail: "x" },
        { status: 409 },
      );
    const { result } = renderHook(() => hook("e1"), { wrapper });
    const err = await result.current
      .mutateAsync({ serviceId: "svc1", version: 7, spec })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ServiceActionError);
    expect((err as ServiceActionError).status).toBe(409);
    expect((err as ServiceActionError).type).toBe("service_changed");
    expect(posted).toHaveLength(1);
  });

  it.each([
    ["review", useReviewServiceUpdate],
    ["apply", useApplyServiceUpdate],
  ] as const)(
    "says status 0 for a %s that got no answer, and does not try again",
    async (_name, hook) => {
      vi.stubGlobal(
        "fetch",
        vi.fn(async () => {
          throw new TypeError("network down");
        }),
      );
      const { result } = renderHook(() => hook("e1"), { wrapper });
      const err = await result.current
        .mutateAsync({ serviceId: "svc1", version: 7, spec })
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(ServiceActionError);
      expect((err as ServiceActionError).status).toBe(0);
      expect(fetch).toHaveBeenCalledTimes(1);
    },
  );

  it("keeps no spec in the cache once nobody is watching the request", async () => {
    const { result, unmount } = renderHook(() => useReviewServiceUpdate("e1"), { wrapper });
    await result.current.mutateAsync({ serviceId: "svc1", version: 7, spec });
    unmount();
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
  });

  it("reads the services again after an apply, whatever the answer, and not after a review", async () => {
    const invalidate = vi.spyOn(client, "invalidateQueries");
    const review = renderHook(() => useReviewServiceUpdate("e1"), { wrapper });
    await review.result.current.mutateAsync({ serviceId: "svc1", version: 7, spec });
    expect(invalidate).not.toHaveBeenCalled();
    reply = () =>
      Response.json({ type: "update_failed", title: "x", status: 500 }, { status: 500 });
    const apply = renderHook(() => useApplyServiceUpdate("e1"), { wrapper });
    await apply.result.current
      .mutateAsync({ serviceId: "svc1", version: 7, spec })
      .catch(() => undefined);
    await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: servicesKey("e1") }));
  });
});
