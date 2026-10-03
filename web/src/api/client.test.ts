// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api, setCsrfToken, setUnauthorizedHandler } from "@/api/client";
import { resetBasePathForTests } from "@/lib/base-path";

function stubFetch(status: number, body: unknown = {}) {
  const fn = vi.fn(async (_input: RequestInfo | URL) =>
    status === 204 ? new Response(null, { status }) : Response.json(body, { status }),
  );
  vi.stubGlobal("fetch", fn);
  return fn;
}

function sentRequest(fn: ReturnType<typeof stubFetch>): Request {
  const first = fn.mock.calls[0];
  if (!first) throw new Error("fetch was not called");
  return first[0] as Request;
}

beforeEach(() => {
  setCsrfToken(null);
  setUnauthorizedHandler(null);
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
