// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  applyRestore,
  applySetupRestore,
  checkRestore,
  checkSetupRestore,
  RestoreError,
  uploadRestore,
  uploadSetupRestore,
} from "@/api/backup";
import { setCsrfToken } from "@/api/client";
import { resetBasePathForTests } from "@/lib/base-path";

const SUMMARY = {
  appVersion: "v0.1.0",
  chainVerified: true,
  counts: { users: 0, teams: 0, environments: 0, auditEntries: 0 },
  created: "2026-10-07T10:30:00Z",
  instanceName: "Old lab",
  older: false,
  publicUrl: "",
  publicUrlDiffers: false,
  schema: 12,
};

const file = () => new File(["bytes"], "lab.bayplan");

function stub(respond: (input: RequestInfo | URL, init?: RequestInit) => Response) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => respond(input, init));
  vi.stubGlobal("fetch", fn);
  return fn;
}

beforeEach(() => setCsrfToken("csrf-9"));
afterEach(() => {
  vi.unstubAllGlobals();
  setCsrfToken(null);
  for (const el of document.head.querySelectorAll('meta[name="bayplan-base"]')) el.remove();
  resetBasePathForTests();
});

describe("uploadRestore", () => {
  it("posts the file itself as the body, as octet-stream, with the CSRF header", async () => {
    const fn = stub(() =>
      Response.json({ uploadId: "u1", size: 5, expiresAt: "x" }, { status: 201 }),
    );
    const f = file();
    await expect(uploadRestore(f)).resolves.toEqual({ uploadId: "u1", size: 5, expiresAt: "x" });
    const [url, init] = fn.mock.calls[0] ?? [];
    expect(url).toBe("/api/backup/restore/upload");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBe(f);
    const headers = new Headers(init?.headers);
    expect(headers.get("Content-Type")).toBe("application/octet-stream");
    expect(headers.get("X-CSRF-Token")).toBe("csrf-9");
  });

  it("goes under the base path", async () => {
    const meta = document.createElement("meta");
    meta.name = "bayplan-base";
    meta.content = "/bayplan";
    document.head.append(meta);
    resetBasePathForTests();
    const fn = stub(() =>
      Response.json({ uploadId: "u1", size: 1, expiresAt: "x" }, { status: 201 }),
    );
    await uploadRestore(file());
    expect(fn.mock.calls[0]?.[0]).toBe("/bayplan/api/backup/restore/upload");
  });

  it("throws the server's problem type, status and sentence", async () => {
    stub(() =>
      Response.json(
        { status: 400, type: "upload_incomplete", detail: "The upload ended early." },
        { status: 400 },
      ),
    );
    const err = await uploadRestore(file()).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestoreError);
    expect(err).toMatchObject({
      status: 400,
      type: "upload_incomplete",
      detail: "The upload ended early.",
    });
  });

  it("throws a typeless error for a proxy's own page", async () => {
    stub(() => new Response("<html>413 Request Entity Too Large</html>", { status: 413 }));
    const err = await uploadRestore(file()).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RestoreError);
    expect(err).toMatchObject({ status: 413, type: undefined });
  });
});

describe("checkRestore and applyRestore", () => {
  it("check posts the upload ID and passphrase in the body and returns the summary", async () => {
    const fn = stub(() => Response.json(SUMMARY));
    await expect(checkRestore("u1", "pw")).resolves.toEqual(SUMMARY);
    const req = fn.mock.calls[0]?.[0] as Request;
    expect(new URL(req.url).pathname).toBe("/api/backup/restore/check");
    expect(await req.json()).toEqual({ uploadId: "u1", passphrase: "pw" });
    expect(req.url).not.toContain("pw");
  });

  it("check throws a RestoreError carrying the problem type and version-bearing detail", async () => {
    stub(() =>
      Response.json(
        {
          status: 409,
          type: "backup_newer",
          detail: "This backup was made by a newer Bayplan (v9.9.9).",
        },
        { status: 409 },
      ),
    );
    const err = await checkRestore("u1", "pw").catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 409, type: "backup_newer" });
  });

  it("apply sends the typed instance name and resolves on 202", async () => {
    const fn = stub(() => Response.json({ restarting: true }, { status: 202 }));
    await expect(applyRestore("u1", "pw", "Home lab")).resolves.toBeUndefined();
    const req = fn.mock.calls[0]?.[0] as Request;
    expect(new URL(req.url).pathname).toBe("/api/backup/restore/apply");
    expect(await req.json()).toEqual({ uploadId: "u1", passphrase: "pw", confirm: "Home lab" });
  });

  it("apply throws on a refusal", async () => {
    stub(() =>
      Response.json(
        { status: 422, type: "confirm_mismatch", detail: "Type the name." },
        { status: 422 },
      ),
    );
    await expect(applyRestore("u1", "pw", "x")).rejects.toMatchObject({
      status: 422,
      type: "confirm_mismatch",
    });
  });
});

describe("the wizard's variants", () => {
  it("upload sends the setup token in a header and no CSRF token is needed", async () => {
    setCsrfToken(null);
    const fn = stub(() =>
      Response.json({ uploadId: "u2", size: 5, expiresAt: "x" }, { status: 201 }),
    );
    await uploadSetupRestore(file(), "tok-1");
    const [url, init] = fn.mock.calls[0] ?? [];
    expect(url).toBe("/api/setup/restore/upload");
    const headers = new Headers(init?.headers);
    expect(headers.get("X-Setup-Token")).toBe("tok-1");
    expect(headers.get("X-CSRF-Token")).toBeNull();
    expect(String(url)).not.toContain("tok-1");
  });

  it("check and apply send the setup token in the body", async () => {
    const fn = stub((input) =>
      new URL((input as Request).url).pathname.endsWith("/check")
        ? Response.json(SUMMARY)
        : Response.json({ restarting: true }, { status: 202 }),
    );
    await checkSetupRestore("u2", "pw", "tok-1");
    await applySetupRestore("u2", "pw", "tok-1");
    const [check, apply] = fn.mock.calls.map((c) => c[0] as Request);
    expect(new URL(check?.url ?? "").pathname).toBe("/api/setup/restore/check");
    expect(await check?.json()).toEqual({ uploadId: "u2", passphrase: "pw", setupToken: "tok-1" });
    expect(new URL(apply?.url ?? "").pathname).toBe("/api/setup/restore/apply");
    expect(await apply?.json()).toEqual({ uploadId: "u2", passphrase: "pw", setupToken: "tok-1" });
  });

  it("a 409 setup_complete comes back as a RestoreError of that type", async () => {
    stub(() => Response.json({ status: 409, type: "setup_complete" }, { status: 409 }));
    await expect(checkSetupRestore("u", "pw", "t")).rejects.toMatchObject({
      status: 409,
      type: "setup_complete",
    });
  });
});
