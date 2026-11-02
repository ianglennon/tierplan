// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";
import type { ZodType } from "zod";

import { readProblem } from "@/api/problem";
import {
  adminSchema,
  instanceSchema,
  routeServerError,
  routeServerFieldErrors,
  tokenSchema,
} from "@/features/setup/schemas";

// issueMessages runs a schema and returns each failing path with its message
// key, so a test asserts on the copy key the screen will translate.
function issues(schema: ZodType, value: unknown): string[] {
  const res = schema.safeParse(value);
  if (res.success) return [];
  return res.error.issues.map((i) => `${i.path.join(".")}:${i.message}`);
}

const goodAdmin = {
  username: "admin",
  password: "correct horse battery",
  confirm: "correct horse battery",
};

describe("tokenSchema", () => {
  it("needs a token of at most 128 characters and trims pasted whitespace", () => {
    expect(issues(tokenSchema, "")).toEqual([":setup.errors.tokenRequired"]);
    expect(issues(tokenSchema, "   ")).toEqual([":setup.errors.tokenRequired"]);
    expect(issues(tokenSchema, "a".repeat(129))).toHaveLength(1);
    expect(tokenSchema.parse("  dc5917c4-9866cf85  ")).toBe("dc5917c4-9866cf85");
  });
});

describe("adminSchema", () => {
  it("rejects a username that is too short or has a space with the username key", () => {
    expect(issues(adminSchema, { ...goodAdmin, username: "ab" })).toEqual([
      "username:setup.errors.username",
    ]);
    expect(issues(adminSchema, { ...goodAdmin, username: "bad name" })).toEqual([
      "username:setup.errors.username",
    ]);
    expect(issues(adminSchema, { ...goodAdmin, username: "a".repeat(33) })).toEqual([
      "username:setup.errors.username",
    ]);
  });

  it("checks the characters as typed, so a Unicode lookalike never folds to ASCII", () => {
    // U+212A KELVIN SIGN lower-cases to an ASCII k; the server refuses it, so must the UI.
    expect(issues(adminSchema, { ...goodAdmin, username: "Kevin" })).toEqual([
      "username:setup.errors.username",
    ]);
  });

  it("rejects an 11-character password with the too-short key and accepts 12", () => {
    expect(
      issues(adminSchema, { ...goodAdmin, password: "elevenchars", confirm: "elevenchars" }),
    ).toEqual(["password:setup.errors.passwordShort"]);
    expect(
      issues(adminSchema, { ...goodAdmin, password: "twelve chars", confirm: "twelve chars" }),
    ).toEqual([]);
  });

  it("counts characters, not UTF-16 units, like the server", () => {
    // Six emoji are 12 UTF-16 units but only 6 characters.
    const short = "😀😀😀😀😀😀";
    expect(issues(adminSchema, { ...goodAdmin, password: short, confirm: short })).toEqual([
      "password:setup.errors.passwordShort",
    ]);
  });

  it("rejects a mismatched confirmation on the confirm field", () => {
    expect(issues(adminSchema, { ...goodAdmin, confirm: "something else entirely" })).toEqual([
      "confirm:setup.errors.passwordMismatch",
    ]);
  });

  it("lower-cases a valid username", () => {
    expect(adminSchema.parse({ ...goodAdmin, username: "Admin.User_1" }).username).toBe(
      "admin.user_1",
    );
  });
});

describe("instanceSchema", () => {
  it("rejects an empty name and a 65-character name", () => {
    expect(issues(instanceSchema, { instanceName: "", publicUrl: "https://b.example" })).toEqual([
      "instanceName:setup.errors.instanceNameRequired",
    ]);
    expect(
      issues(instanceSchema, { instanceName: "n".repeat(65), publicUrl: "https://b.example" }),
    ).toEqual(["instanceName:setup.errors.instanceNameTooLong"]);
    expect(
      issues(instanceSchema, { instanceName: "n".repeat(64), publicUrl: "https://b.example" }),
    ).toEqual([]);
  });

  it("trims the name", () => {
    const out = instanceSchema.parse({ instanceName: "  Home lab  ", publicUrl: "http://b.lan" });
    expect(out.instanceName).toBe("Home lab");
  });

  it("rejects a public URL that is not an absolute http or https address", () => {
    for (const bad of [
      "example.com",
      "",
      "ftp://example.com",
      "/relative",
      "https://user:pw@example.com",
      "https://example.com/?q=1",
      "https://example.com/#top",
    ]) {
      expect(issues(instanceSchema, { instanceName: "Lab", publicUrl: bad })).toEqual([
        "publicUrl:setup.errors.publicUrl",
      ]);
    }
  });

  it("accepts an https address, an http address with a port and a sub-path", () => {
    for (const ok of [
      "https://tierplan.example.com",
      "http://192.168.1.10:7480",
      "https://example.com/tierplan",
    ]) {
      expect(issues(instanceSchema, { instanceName: "Lab", publicUrl: ok })).toEqual([]);
    }
  });
});

describe("readProblem", () => {
  it("extracts type and the field list from a huma problem body", () => {
    const body = {
      type: "validation_failed",
      status: 422,
      errors: [
        { location: "body.username", message: "invalid_username", value: "ab" },
        { location: "body.password", message: "password_too_short" },
      ],
    };
    expect(readProblem(body, { status: 422 })).toEqual({
      status: 422,
      type: "validation_failed",
      fields: [
        { location: "body.username", message: "invalid_username" },
        { location: "body.password", message: "password_too_short" },
      ],
    });
  });

  it("never echoes the submitted value back into the result", () => {
    const p = readProblem({
      errors: [{ location: "body.password", message: "x", value: "hunter2" }],
    });
    expect(JSON.stringify(p)).not.toContain("hunter2");
  });

  it("falls back to the body's own status and tolerates a non-JSON or missing body", () => {
    expect(readProblem({ type: "rate_limited", status: 429 })).toMatchObject({
      status: 429,
      type: "rate_limited",
      fields: [],
    });
    for (const odd of ["<html>502 Bad Gateway</html>", undefined, null, 42, [], { errors: "no" }]) {
      expect(readProblem(odd, { status: 502 })).toEqual({ status: 502, fields: [] });
    }
  });

  it("skips error entries that are not objects with string members", () => {
    expect(
      readProblem({
        errors: [null, 3, { location: 1, message: "a" }, { location: "l", message: "m" }],
      }).fields,
    ).toEqual([{ location: "l", message: "m" }]);
  });
});

describe("routeServerError", () => {
  const validation = (...errors: [string, string][]) =>
    readProblem(
      {
        type: "validation_failed",
        errors: errors.map(([location, message]) => ({ location, message })),
      },
      { status: 422 },
    );

  it("sends username and password codes to step 2", () => {
    expect(routeServerError(validation(["body.username", "invalid_username"]))).toEqual({
      step: 2,
      field: "username",
      messageKey: "setup.errors.username",
    });
    expect(routeServerError(validation(["body.password", "password_too_short"]))).toEqual({
      step: 2,
      field: "password",
      messageKey: "setup.errors.passwordShort",
    });
    expect(routeServerError(validation(["body.password", "password_common"]))).toEqual({
      step: 2,
      field: "password",
      messageKey: "setup.errors.passwordCommon",
    });
  });

  it("sends an invalid setup token to step 1", () => {
    expect(routeServerError(readProblem({ type: "invalid_setup_token" }, { status: 401 }))).toEqual(
      {
        step: 1,
        field: "token",
        messageKey: "setup.errors.tokenInvalid",
      },
    );
  });

  it("sends instance name and public URL codes to step 3", () => {
    expect(routeServerError(validation(["body.instanceName", "instance_name_required"]))).toEqual({
      step: 3,
      field: "instanceName",
      messageKey: "setup.errors.instanceNameRequired",
    });
    expect(routeServerError(validation(["body.instanceName", "instance_name_too_long"]))).toEqual({
      step: 3,
      field: "instanceName",
      messageKey: "setup.errors.instanceNameTooLong",
    });
    expect(routeServerError(validation(["body.publicUrl", "invalid_public_url"]))).toEqual({
      step: 3,
      field: "publicUrl",
      messageKey: "setup.errors.publicUrl",
    });
  });

  it("answers rate limiting as a form-level error, not a field error", () => {
    expect(routeServerError(readProblem({ type: "rate_limited" }, { status: 429 }))).toEqual({
      step: null,
      messageKey: "setup.errors.rateLimited",
    });
  });

  it("maps unknown problems and server failures to the form-level server error", () => {
    for (const p of [
      readProblem("boom", { status: 500 }),
      readProblem({ type: "about:blank" }, { status: 400 }),
      validation(["body.unknown", "something_new"]),
      readProblem(undefined, { status: 503 }),
    ]) {
      expect(routeServerError(p)).toEqual({ step: null, messageKey: "setup.errors.server" });
    }
  });

  it("returns every field error and puts the earliest step first", () => {
    const all = routeServerFieldErrors(
      validation(
        ["body.publicUrl", "invalid_public_url"],
        ["body.password", "password_common"],
        ["body.username", "invalid_username"],
      ),
    );
    expect(all.map((e) => e.field)).toEqual(["username", "password", "publicUrl"]);
    expect(
      routeServerError(
        validation(["body.publicUrl", "invalid_public_url"], ["body.username", "invalid_username"]),
      ).step,
    ).toBe(2);
  });

  it("keeps a known field whose code is new and falls back to the server message", () => {
    expect(routeServerError(validation(["body.password", "password_new_rule"]))).toEqual({
      step: 2,
      field: "password",
      messageKey: "setup.errors.server",
    });
  });
});
