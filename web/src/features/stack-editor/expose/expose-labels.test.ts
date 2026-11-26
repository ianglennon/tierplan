// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it, vi } from "vitest";

import {
  addRoute,
  type ExposeTarget,
  listServices,
  moveToTarget,
  readServiceRoutes,
  removeIgnoredCopy,
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
  | { kind: "remove"; service: string; route: string }
  | { kind: "move"; service: string }
  | { kind: "removeIgnored"; service: string };

interface GoldenDecl {
  service: string;
  name: string;
  host: string;
  port: number;
  path: string;
  scheme: "http" | "https";
  strip: boolean;
}

interface GoldenCase {
  name: string;
  target: ExposeTarget;
  input: string;
  ops: Op[];
  output: string;
  declarations?: GoldenDecl[];
  problems?: string[];
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
    case "move":
      return moveToTarget(text, op.service, target);
    case "removeIgnored":
      return removeIgnoredCopy(text, op.service, target);
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

  it("holds the cases the plan names, and at least twenty", () => {
    expect(golden.length).toBeGreaterThanOrEqual(20);
    const names = new Set(golden.map((c) => c.name));
    for (const want of [
      "swarm-list-set-host",
      "standalone-map-set",
      "standalone-list-set",
      "no-block-creates-map",
      "named-route-add",
      "first-add-is-short-form",
      "set-path-slash-omits-key",
      "set-scheme-http-omits-key",
      "set-strip-false-omits-key",
      "remove-named-route",
      "remove-last-route-deletes-empty-labels-and-deploy",
      "comments-and-order-kept",
      "swarm-wrong-place-move",
      "standalone-wrong-place-move",
      "both-places-remove-ignored",
    ]) {
      expect(names.has(want), want).toBe(true);
    }
  });

  // What the editor reads back is what the server's parser read from the same text: every case the
  // server read without a problem has the routes of its declarations, field by field.
  for (const c of golden.filter((g) => g.declarations && !g.problems)) {
    it(`case ${c.name}: the editor reads the routes the server reads`, () => {
      const decls = c.declarations ?? [];
      for (const service of new Set(decls.map((d) => d.service))) {
        const want = decls.filter((d) => d.service === service);
        const got = readServiceRoutes(c.output, service, c.target);
        expect(got.routes.map((r) => r.name).sort()).toEqual(want.map((d) => d.name).sort());
        for (const d of want) {
          const r = got.routes.find((x) => x.name === d.name);
          expect(r, d.name).toBeDefined();
          expect(r?.host).toBe(d.host);
          expect(r?.port).toBe(String(d.port));
          expect(r?.path || "/").toBe(d.path);
          expect(r?.scheme).toBe(d.scheme);
          expect(r?.strip).toBe(d.strip);
        }
      }
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

describe("placement", () => {
  const wrong = `services:
  api:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.example.com
      tierplan.ingress.port: "8080"
`;
  const both = `services:
  api:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: old.example.com
    deploy:
      labels:
        tierplan.ingress.host: app.example.com
        tierplan.ingress.port: "8080"
`;

  it("reports labels that sit only in the block the target does not read", () => {
    const swarm = readServiceRoutes(wrong, "api", "swarm");
    expect(swarm.wrongPlace).toBe(true);
    expect(swarm.bothPlaces).toBe(false);
    expect(swarm.routes).toEqual([]);
    const standalone = readServiceRoutes(wrong, "api", "standalone");
    expect(standalone.wrongPlace).toBe(false);
    expect(standalone.routes.map((r) => r.name)).toEqual(["default"]);
  });

  it("reports labels that sit in both blocks, and reads the one the target reads", () => {
    const swarm = readServiceRoutes(both, "api", "swarm");
    expect(swarm.bothPlaces).toBe(true);
    expect(swarm.wrongPlace).toBe(false);
    expect(swarm.routes[0]?.host).toBe("app.example.com");
    const standalone = readServiceRoutes(both, "api", "standalone");
    expect(standalone.bothPlaces).toBe(true);
    expect(standalone.routes[0]?.host).toBe("old.example.com");
  });

  it("moves to the target and then reports nothing wrong", () => {
    const moved = moveToTarget(wrong, "api", "swarm");
    expect(moved).not.toBe(wrong);
    const after = readServiceRoutes(moved, "api", "swarm");
    expect(after.wrongPlace).toBe(false);
    expect(after.bothPlaces).toBe(false);
    expect(after.routes.map((r) => r.host)).toEqual(["app.example.com"]);
  });

  it("removes the ignored copy and then reports nothing wrong", () => {
    const removed = removeIgnoredCopy(both, "api", "swarm");
    const after = readServiceRoutes(removed, "api", "swarm");
    expect(after.bothPlaces).toBe(false);
    expect(after.wrongPlace).toBe(false);
    expect(after.routes.map((r) => r.host)).toEqual(["app.example.com"]);
  });

  it("does not move or remove when the labels are not in that state", () => {
    expect(moveToTarget(both, "api", "swarm")).toBe(both);
    expect(removeIgnoredCopy(wrong, "api", "swarm")).toBe(wrong);
  });
});

describe("locked values", () => {
  const text = `x-host: &h app.example.com
x-labels: &base
  tierplan.ingress.port: "8080"
services:
  api:
    image: nginx:1.27
    deploy:
      labels:
        <<: *base
        tierplan.ingress.host: *h
        tierplan.ingress.path: \${APP_PATH}
        tierplan.ingress.scheme: $SCHEME
        tierplan.ingress.strip: "true"
`;

  it("marks an alias, a merged value and a variable locked, and leaves a plain value free", () => {
    const got = readServiceRoutes(text, "api", "swarm");
    expect(got.parseError).toBe(false);
    const r = got.routes[0];
    expect(r?.host).toBe("app.example.com");
    expect(r?.locked.host).toBe("anchor");
    expect(r?.port).toBe("8080");
    expect(r?.locked.port).toBe("anchor");
    expect(r?.locked.path).toBe("variable");
    expect(r?.locked.scheme).toBe("variable");
    expect(r?.locked.strip).toBeUndefined();
  });

  it("changes none of the locked fields and still changes the free one", () => {
    let t = text;
    for (const [field, value] of [
      ["host", "x.example.com"],
      ["port", "9000"],
      ["path", "/x"],
      ["scheme", "https"],
      ["path", "/"],
    ] as const) {
      t = setRouteField(t, "api", "swarm", "default", field, value);
    }
    expect(t).toBe(text);
    const free = setRouteField(text, "api", "swarm", "default", "strip", "false");
    expect(free).not.toBe(text);
    expect(readServiceRoutes(free, "api", "swarm").routes[0]?.strip).toBe(false);
  });

  it("reports a block that is an alias as locked", () => {
    const aliased = `x-l: &l
  a: b
services:
  api:
    image: nginx:1.27
    deploy:
      labels: *l
`;
    const got = readServiceRoutes(aliased, "api", "swarm");
    expect(got.blockLocked).toBe(true);
    expect(addRoute(aliased, "api", "swarm", null, "a.example.com", "80")).toBe(aliased);
  });
});

describe("a compose file that is not valid YAML", () => {
  const bad = "services:\n  api:\n    image: [nginx\n";

  it("reports parseError and changes nothing", () => {
    expect(readServiceRoutes(bad, "api", "swarm").parseError).toBe(true);
    expect(setRouteField(bad, "api", "swarm", "default", "host", "a.example.com")).toBe(bad);
    expect(addRoute(bad, "api", "swarm", null, "a.example.com", "80")).toBe(bad);
    expect(removeRoute(bad, "api", "swarm", "default")).toBe(bad);
    expect(moveToTarget(bad, "api", "swarm")).toBe(bad);
    expect(removeIgnoredCopy(bad, "api", "swarm")).toBe(bad);
  });
});

describe("the rest of the file is left as it was", () => {
  const crlf = (s: string) => s.replace(/\n/g, "\r\n");
  const base =
    "services:\n  api:\n    image: nginx:1.27\n    environment:\n      MODE: 0440\n      LIMIT: 1e3\n" +
    '    deploy:\n      labels:\n        tierplan.ingress.host: app.example.com\n        tierplan.ingress.port: "80"\n';

  it("removes a route without touching a leading-zero scalar or a line ending", () => {
    const out = removeRoute(crlf(base), "api", "swarm", "default");
    expect(out).toBe(
      crlf(
        "services:\n  api:\n    image: nginx:1.27\n    environment:\n      MODE: 0440\n      LIMIT: 1e3\n",
      ),
    );
  });

  it("keeps a leading zero and CRLF when the edited line is itself rewritten by the writer", () => {
    // The labels are a flow map, which the writer re-spaces, so the edit cannot be carried line by
    // line and the whole document is written; the zeros and the line endings still survive.
    const text = crlf(
      'services:\n  api:\n    image: nginx\n    environment:\n      MODE: 0440\n    deploy:\n      labels: {tierplan.ingress.host: a.example.com, tierplan.ingress.port: "80"}\n',
    );
    const out = setRouteField(text, "api", "swarm", "default", "host", "b.example.com");
    expect(out).toContain("MODE: 0440\r\n");
    expect(out).toContain("b.example.com");
    expect(out.replace(/\r\n/g, "")).not.toMatch(/\n/);
  });

  it("writes a number it creates itself as a quoted string", () => {
    const out = setRouteField(base, "api", "swarm", "default", "port", "0080");
    expect(out).toContain('tierplan.ingress.port: "0080"');
    expect(out).toContain("MODE: 0440\n");
  });
});
