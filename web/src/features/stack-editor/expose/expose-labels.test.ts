// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it, vi } from "vitest";

import {
  addRoute,
  type ExposeTarget,
  listServices,
  readServiceRoutes,
  removeRoute,
  setRouteField,
  validatePort,
  validateRouteName,
} from "@/features/stack-editor/expose/expose-labels";

// The label editor of the Expose panel (D-18, D-19). Its writer and the server's parser share one
// golden file (A-08): this suite applies each case's ops to its input and wants its output byte for
// byte; internal/stacks/expose_golden_test.go runs the real check on the same output and wants the
// routes the case names. A key written here that the server read differently would fail there.

type Op =
  | {
      kind: "set";
      service: string;
      route: string;
      field: "host" | "port" | "path" | "scheme" | "strip";
      value: string;
    }
  | { kind: "add"; service: string; name?: string; host: string; port: string }
  | { kind: "remove"; service: string; route: string };

interface GoldenCase {
  name: string;
  target: ExposeTarget;
  input: string;
  ops: Op[];
  output: string;
}

// The file lives with the Go test that reads it. jsdom replaces the URL class, so the path comes
// from the directory of this file; import.meta.dirname is vitest's, and not in the type.
const here = (import.meta as unknown as { dirname: string }).dirname;
const { readFileSync } = await vi.importActual<{
  readFileSync(path: string, encoding: "utf8"): string;
}>("node:fs");
const golden: GoldenCase[] = JSON.parse(
  readFileSync(`${here}/../../../../../internal/stacks/testdata/expose-golden.json`, "utf8"),
);

function apply(text: string, target: ExposeTarget, op: Op): string {
  switch (op.kind) {
    case "set":
      return setRouteField(text, op.service, target, op.route, op.field, op.value);
    case "add":
      return addRoute(text, op.service, target, op.name ?? null, op.host, op.port);
    case "remove":
      return removeRoute(text, op.service, target, op.route);
  }
}

describe("the golden file", () => {
  it("has cases with unique names", () => {
    const names = golden.map((c) => c.name);
    expect(names.length).toBeGreaterThan(0);
    expect(new Set(names).size).toBe(names.length);
  });

  for (const c of golden) {
    it(`case ${c.name}: the ops turn the input into the output`, () => {
      const got = c.ops.reduce((text, op) => apply(text, c.target, op), c.input);
      expect(got).toBe(c.output);
    });
  }
});

describe("validateRouteName", () => {
  it("refuses default, upper case, a leading hyphen, 33 characters and a duplicate", () => {
    expect(validateRouteName("default", [])).toBe("invalid");
    expect(validateRouteName("API", [])).toBe("invalid");
    expect(validateRouteName("-x", [])).toBe("invalid");
    expect(validateRouteName("x-", [])).toBe("invalid");
    expect(validateRouteName("", [])).toBe("invalid");
    expect(validateRouteName("a".repeat(33), [])).toBe("invalid");
    expect(validateRouteName("api", ["default", "api"])).toBe("duplicate");
  });

  it("accepts admin-2 and a name of 32 characters", () => {
    expect(validateRouteName("admin-2", ["default"])).toBe("ok");
    expect(validateRouteName("a".repeat(32), [])).toBe("ok");
    expect(validateRouteName("a", [])).toBe("ok");
  });
});

describe("validatePort", () => {
  it("accepts a whole number from 1 to 65535", () => {
    expect(validatePort("1")).toBe(true);
    expect(validatePort("8080")).toBe(true);
    expect(validatePort("65535")).toBe(true);
  });

  it("refuses 0, 65536, a fraction, a sign, a hex number and nothing", () => {
    for (const v of ["0", "65536", "8.5", "", " ", "+80", "-1", "0x50", "80a", "1e3"]) {
      expect(validatePort(v), v).toBe(false);
    }
  });
});

describe("listServices and readServiceRoutes", () => {
  const text = `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: shop.example.com
      tierplan.ingress.port: "80"
      tierplan.ingress.path: /shop
      tierplan.ingress.scheme: HTTPS
      tierplan.ingress.strip: "true"
      tierplan.ingress.admin.host: admin.example.com
      tierplan.ingress.admin.port: 8081
  db:
    image: postgres:17
`;

  it("lists the services in file order, and none for text that is not YAML", () => {
    expect(listServices(text)).toEqual(["web", "db"]);
    expect(listServices("services: [")).toEqual([]);
    expect(listServices("")).toEqual([]);
  });

  it("reads the routes back in file order, from a standalone labels block", () => {
    const got = readServiceRoutes(text, "web", "standalone");
    expect(got.services).toEqual(["web", "db"]);
    expect(got.form).toBe("map");
    expect(got.wrongPlace).toBe(false);
    expect(got.routes.map((r) => r.name)).toEqual(["default", "admin"]);
    expect(got.routes[0]).toMatchObject({
      host: "shop.example.com",
      port: "80",
      path: "/shop",
      scheme: "https",
      strip: true,
    });
    expect(got.routes[1]).toMatchObject({
      host: "admin.example.com",
      port: "8081",
      path: "",
      scheme: "http",
    });
  });

  it("reads nothing for a service that is not there or has no block", () => {
    expect(readServiceRoutes(text, "nope", "standalone").routes).toEqual([]);
    const db = readServiceRoutes(text, "db", "standalone");
    expect(db.routes).toEqual([]);
    expect(db.form).toBe("none");
  });
});
