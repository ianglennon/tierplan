// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api, setAccessChangeHandler, setCsrfToken, setUnauthorizedHandler } from "@/api/client";
import { resetBasePathForTests } from "@/lib/base-path";

function stubFetch(status: number, body: unknown = {}) {
  const fn = vi.fn(async (_input: RequestInfo | URL) =>
    status === 204 ? new Response(null, { status }) : Response.json(body, { status }),
  );
  vi.stubGlobal("fetch", fn);
  return fn;
}

const SUMMARY_BODY = {};

function sentRequest(fn: ReturnType<typeof stubFetch>): Request {
  const first = fn.mock.calls[0];
  if (!first) throw new Error("fetch was not called");
  return first[0] as Request;
}

beforeEach(() => {
  setCsrfToken(null);
  setUnauthorizedHandler(null);
  setAccessChangeHandler(null);
  window.history.pushState({}, "", "/");
});

afterEach(() => {
  vi.unstubAllGlobals();
  for (const el of document.head.querySelectorAll('meta[name="bayplan-base"]')) el.remove();
  resetBasePathForTests();
  vi.resetModules();
});

describe("CSRF header", () => {
  it("is attached to a POST once a token is set", async () => {
    const fetchMock = stubFetch(204);
    setCsrfToken("t1");
    await api.POST("/api/auth/logout");
    expect(sentRequest(fetchMock).headers.get("X-CSRF-Token")).toBe("t1");
  });

  it("is not attached to a GET", async () => {
    const fetchMock = stubFetch(200, { instanceName: "x", needsSetup: false, rememberDays: 30 });
    setCsrfToken("t1");
    await api.GET("/api/instance");
    expect(sentRequest(fetchMock).headers.has("X-CSRF-Token")).toBe(false);
  });

  it("is not sent after the token is cleared", async () => {
    const fetchMock = stubFetch(204);
    setCsrfToken("t1");
    setCsrfToken(null);
    await api.POST("/api/auth/logout");
    expect(sentRequest(fetchMock).headers.has("X-CSRF-Token")).toBe(false);
  });

  it("is never written to browser storage", async () => {
    stubFetch(204);
    setCsrfToken("secret-token");
    await api.POST("/api/auth/logout");
    expect(JSON.stringify({ ...window.localStorage })).not.toContain("secret-token");
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain("secret-token");
    expect(document.cookie).not.toContain("secret-token");
  });
});

describe("401 handling", () => {
  it("calls the handler with the current path on a 401 from GET /api/me", async () => {
    stubFetch(401, { status: 401 });
    window.history.pushState({}, "", "/environments/e1?tab=x");
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    await api.GET("/api/me");
    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler).toHaveBeenCalledWith("/environments/e1?tab=x");
  });

  it("does not call the handler for a 401 from POST /api/auth/login", async () => {
    stubFetch(401, { status: 401, type: "invalid_credentials" });
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    await api.POST("/api/auth/login", { body: { username: "a", password: "b" } });
    expect(handler).not.toHaveBeenCalled();
  });

  it("does not call the handler for a 401 from the setup endpoints", async () => {
    stubFetch(401, { status: 401 });
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    await api.POST("/api/setup/token", { body: { token: "x" } });
    expect(handler).not.toHaveBeenCalled();
  });

  it("does not call the handler on a success or a 403", async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    stubFetch(200, {});
    await api.GET("/api/me");
    stubFetch(403, { status: 403 });
    await api.POST("/api/auth/logout");
    expect(handler).not.toHaveBeenCalled();
  });

  it("still returns the 401 response to the caller", async () => {
    stubFetch(401, { status: 401 });
    const { response } = await api.GET("/api/me");
    expect(response.status).toBe(401);
  });
});

// 403 and 404 are how the server says access changed (D-08, D-10): an unseen
// environment is a 404 that looks like a missing one, and a refused action is a
// 403. The signed-in layout registers the handler that reacts.
describe("access changes (D-10)", () => {
  const read = () => api.GET("/api/env/{envId}/summary", { params: { path: { envId: "e1" } } });
  const mutate = () => api.POST("/api/env/{envId}/reconnect", { params: { path: { envId: "e1" } } });

  it("tells the handler about a 404 environment_not_found on an environment request", async () => {
    stubFetch(404, { status: 404, type: "environment_not_found" });
    const handler = vi.fn();
    setAccessChangeHandler(handler);
    await read();
    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler).toHaveBeenCalledWith("unseen", "/api/env/e1/summary");
  });

  it("tells the handler about a 403 on an environment read", async () => {
    stubFetch(403, { status: 403 });
    const handler = vi.fn();
    setAccessChangeHandler(handler);
    await read();
    expect(handler).toHaveBeenCalledWith("unseen", "/api/env/e1/summary");
  });

  it("tells the handler a mutation was refused on a 403", async () => {
    stubFetch(403, { status: 403 });
    const handler = vi.fn();
    setAccessChangeHandler(handler);
    await mutate();
    expect(handler).toHaveBeenCalledWith("refused", "/api/env/e1/reconnect");
  });

  it("ignores a 404 that is about something other than the environment", async () => {
    stubFetch(404, { status: 404, type: "container_not_found" });
    const handler = vi.fn();
    setAccessChangeHandler(handler);
    await read();
    await mutate();
    expect(handler).not.toHaveBeenCalled();
  });

  it("ignores a 403 that is a CSRF or origin failure, which is not a change of access", async () => {
    const handler = vi.fn();
    setAccessChangeHandler(handler);
    stubFetch(403, { status: 403, type: "csrf_failed" });
    await mutate();
    stubFetch(403, { status: 403, type: "origin_mismatch" });
    await mutate();
    expect(handler).not.toHaveBeenCalled();
  });

  it("ignores a success and the sign-in calls", async () => {
    const handler = vi.fn();
    setAccessChangeHandler(handler);
    stubFetch(200, SUMMARY_BODY);
    await read();
    stubFetch(403, { status: 403 });
    await api.POST("/api/auth/login", { body: { username: "a", password: "b" } });
    expect(handler).not.toHaveBeenCalled();
  });

  it("does nothing, and still returns the response, when no handler is registered", async () => {
    stubFetch(404, { status: 404, type: "environment_not_found" });
    const { response } = await read();
    expect(response.status).toBe(404);
  });

  it("leaves the body readable for the caller", async () => {
    stubFetch(404, { status: 404, type: "environment_not_found" });
    setAccessChangeHandler(vi.fn());
    const { error } = await read();
    expect(error).toMatchObject({ type: "environment_not_found" });
  });
});

// The typed client is a module singleton that reads the base once, so each
// test announces a base the way the server does and loads a fresh copy.
describe("under a base path", () => {
  async function loadUnderBase(base: string) {
    const meta = document.createElement("meta");
    meta.name = "bayplan-base";
    meta.content = base;
    document.head.append(meta);
    resetBasePathForTests();
    vi.resetModules();
    return import("@/api/client");
  }

  it("sends requests to the origin plus the base", async () => {
    const fetchMock = stubFetch(200, { instanceName: "x", needsSetup: false, rememberDays: 30 });
    const client = await loadUnderBase("/bayplan");
    await client.api.GET("/api/instance");
    expect(sentRequest(fetchMock).url).toBe(`${window.location.origin}/bayplan/api/instance`);
  });

  it("treats a 401 from the prefixed login URL as an answer", async () => {
    stubFetch(401, { status: 401, type: "invalid_credentials" });
    const client = await loadUnderBase("/bayplan");
    const handler = vi.fn();
    client.setUnauthorizedHandler(handler);
    await client.api.POST("/api/auth/login", { body: { username: "a", password: "b" } });
    expect(handler).not.toHaveBeenCalled();
  });

  it("treats a 401 from the prefixed setup URLs as an answer", async () => {
    stubFetch(401, { status: 401 });
    const client = await loadUnderBase("/bayplan");
    const handler = vi.fn();
    client.setUnauthorizedHandler(handler);
    await client.api.POST("/api/setup/token", { body: { token: "x" } });
    expect(handler).not.toHaveBeenCalled();
  });

  it("tells the handler the path without the base", async () => {
    stubFetch(401, { status: 401 });
    window.history.pushState({}, "", "/bayplan/environments/e1?tab=x");
    const client = await loadUnderBase("/bayplan");
    const handler = vi.fn();
    client.setUnauthorizedHandler(handler);
    await client.api.GET("/api/me");
    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler).toHaveBeenCalledWith("/environments/e1?tab=x");
  });
});
