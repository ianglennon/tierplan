// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import type { StackDraft } from "@/api/stacks";
import {
  COMPOSE_FILE,
  emptyWorkingCopy,
  filePathProblem,
  isDirty,
  toSaveBody,
  type WorkingCopy,
  withSecretsHidden,
  workingCopyReducer,
} from "@/features/stack-editor/working-copy";

function draft(over: Partial<StackDraft> = {}): StackDraft {
  return {
    isDraft: true,
    version: 3,
    savedBy: "ada",
    savedAt: "2026-10-19T10:00:00Z",
    baseSeq: 0,
    files: [
      { path: "nginx.conf", content: "events {}", secret: false },
      { path: COMPOSE_FILE, content: "services: {}\n", secret: false },
    ],
    vars: [{ name: "PORT", value: "8080", secret: false }],
    ...over,
  };
}

const loaded = (d: StackDraft = draft()): WorkingCopy =>
  workingCopyReducer(emptyWorkingCopy, { type: "load", draft: d });

describe("load", () => {
  it("holds every file with compose.yaml first and opens compose.yaml", () => {
    const copy = loaded();
    expect(copy.loaded).toBe(true);
    expect(copy.files.map((f) => f.path)).toEqual([COMPOSE_FILE, "nginx.conf"]);
    expect(copy.selected).toBe(COMPOSE_FILE);
    expect(copy.vars).toEqual([{ name: "PORT", value: "8080", secret: false, hidden: false }]);
  });

  it("keeps the open file open when it is still in the draft, and falls back to compose.yaml when not", () => {
    const open = workingCopyReducer(loaded(), { type: "select", path: "nginx.conf" });
    expect(workingCopyReducer(open, { type: "load", draft: draft() }).selected).toBe("nginx.conf");
    const gone = draft({ files: [{ path: COMPOSE_FILE, content: "x", secret: false }] });
    expect(workingCopyReducer(open, { type: "load", draft: gone }).selected).toBe(COMPOSE_FILE);
  });

  it("marks a secret that arrives as <hidden> hidden, with no text", () => {
    const copy = loaded(
      draft({
        files: [
          { path: COMPOSE_FILE, content: "x", secret: false },
          { path: "key.pem", content: "<hidden>", secret: true },
        ],
        vars: [{ name: "TOKEN", value: "<hidden>", secret: true }],
      }),
    );
    expect(copy.files[1]).toEqual({ path: "key.pem", content: "", secret: true, hidden: true });
    expect(copy.vars[0]).toEqual({ name: "TOKEN", value: "", secret: true, hidden: true });
  });

  it("copes with a draft that has no variables", () => {
    expect(loaded(draft({ vars: null })).vars).toEqual([]);
  });
});

describe("edit", () => {
  it("replaces the text of one file only", () => {
    const next = workingCopyReducer(loaded(), { type: "edit", path: COMPOSE_FILE, content: "new" });
    expect(next.files.map((f) => f.content)).toEqual(["new", "events {}"]);
  });

  it("takes a hidden secret file out of hiding: the person is replacing it", () => {
    const start = loaded(
      draft({
        files: [
          { path: COMPOSE_FILE, content: "x", secret: false },
          { path: "key.pem", content: "<hidden>", secret: true },
        ],
      }),
    );
    const next = workingCopyReducer(start, { type: "edit", path: "key.pem", content: "-----" });
    expect(next.files[1]).toMatchObject({ content: "-----", hidden: false, secret: true });
  });
});

describe("addFile, renameFile, deleteFile and select", () => {
  it("adds a file in path order and opens it", () => {
    const next = workingCopyReducer(loaded(), { type: "addFile", path: ".env" });
    expect(next.files.map((f) => f.path)).toEqual([COMPOSE_FILE, ".env", "nginx.conf"]);
    expect(next.selected).toBe(".env");
  });

  it("adds a file with the text it is given", () => {
    const next = workingCopyReducer(loaded(), { type: "addFile", path: "a.txt", content: "hi" });
    expect(next.files.find((f) => f.path === "a.txt")?.content).toBe("hi");
  });

  it("does not add a path that is already there", () => {
    const start = loaded();
    expect(workingCopyReducer(start, { type: "addFile", path: "nginx.conf" })).toBe(start);
  });

  it("renames a file and follows it when it is the open one", () => {
    const open = workingCopyReducer(loaded(), { type: "select", path: "nginx.conf" });
    const next = workingCopyReducer(open, {
      type: "renameFile",
      from: "nginx.conf",
      to: "web.conf",
    });
    expect(next.files.map((f) => f.path)).toEqual([COMPOSE_FILE, "web.conf"]);
    expect(next.selected).toBe("web.conf");
  });

  it("refuses to rename compose.yaml or onto a path that is taken", () => {
    const start = loaded();
    expect(
      workingCopyReducer(start, { type: "renameFile", from: COMPOSE_FILE, to: "x.yaml" }),
    ).toBe(start);
    expect(
      workingCopyReducer(start, { type: "renameFile", from: "nginx.conf", to: COMPOSE_FILE }),
    ).toBe(start);
  });

  it("deletes a file, and opens compose.yaml when it was the open one", () => {
    const open = workingCopyReducer(loaded(), { type: "select", path: "nginx.conf" });
    const next = workingCopyReducer(open, { type: "deleteFile", path: "nginx.conf" });
    expect(next.files.map((f) => f.path)).toEqual([COMPOSE_FILE]);
    expect(next.selected).toBe(COMPOSE_FILE);
  });

  it("refuses to delete compose.yaml", () => {
    const start = loaded();
    expect(workingCopyReducer(start, { type: "deleteFile", path: COMPOSE_FILE })).toBe(start);
  });

  it("selects only a file that exists", () => {
    const start = loaded();
    expect(workingCopyReducer(start, { type: "select", path: "nope" })).toBe(start);
  });
});

describe("isDirty", () => {
  it("is false for the copy as loaded, whichever file is open", () => {
    const saved = loaded();
    expect(isDirty(saved, saved)).toBe(false);
    expect(isDirty(saved, workingCopyReducer(saved, { type: "select", path: "nginx.conf" }))).toBe(
      false,
    );
  });

  it("is true after an edit, and false again when the edit is undone", () => {
    const saved = loaded();
    const edited = workingCopyReducer(saved, { type: "edit", path: COMPOSE_FILE, content: "x" });
    expect(isDirty(saved, edited)).toBe(true);
    const undone = workingCopyReducer(edited, {
      type: "edit",
      path: COMPOSE_FILE,
      content: "services: {}\n",
    });
    expect(isDirty(saved, undone)).toBe(false);
  });

  it("is true after a file is added, renamed or deleted", () => {
    const saved = loaded();
    expect(isDirty(saved, workingCopyReducer(saved, { type: "addFile", path: "a" }))).toBe(true);
    expect(
      isDirty(
        saved,
        workingCopyReducer(saved, { type: "renameFile", from: "nginx.conf", to: "b" }),
      ),
    ).toBe(true);
    expect(
      isDirty(saved, workingCopyReducer(saved, { type: "deleteFile", path: "nginx.conf" })),
    ).toBe(true);
  });

  it("is true when a variable differs", () => {
    const saved = loaded();
    const changed: WorkingCopy = {
      ...saved,
      vars: [{ name: "PORT", value: "9", secret: false, hidden: false }],
    };
    expect(isDirty(saved, changed)).toBe(true);
  });
});

describe("toSaveBody", () => {
  it("sends every file and variable with the version it was read at", () => {
    expect(toSaveBody(loaded(), 3)).toEqual({
      version: 3,
      files: [
        { path: COMPOSE_FILE, content: "services: {}\n", secret: false },
        { path: "nginx.conf", content: "events {}", secret: false },
      ],
      vars: [{ name: "PORT", value: "8080", secret: false }],
    });
  });

  it("keeps a hidden secret the person did not touch: it goes as the server's keep marker, never as text of its own", () => {
    const start = loaded(
      draft({
        files: [
          { path: COMPOSE_FILE, content: "x", secret: false },
          { path: "key.pem", content: "<hidden>", secret: true },
        ],
        vars: [{ name: "TOKEN", value: "<hidden>", secret: true }],
      }),
    );
    const body = toSaveBody(start, 1);
    expect(body.files?.[1]).toEqual({ path: "key.pem", content: "<hidden>", secret: true });
    expect(body.vars?.[0]).toEqual({ name: "TOKEN", value: "<hidden>", secret: true });
  });

  it("sends the new text of a secret the person replaced", () => {
    const start = loaded(
      draft({
        files: [
          { path: COMPOSE_FILE, content: "x", secret: false },
          { path: "key.pem", content: "<hidden>", secret: true },
        ],
      }),
    );
    const next = workingCopyReducer(start, {
      type: "edit",
      path: "key.pem",
      content: "new secret",
    });
    expect(toSaveBody(next, 1).files?.[1]).toEqual({
      path: "key.pem",
      content: "new secret",
      secret: true,
    });
  });
});

describe("filePathProblem", () => {
  const existing = [COMPOSE_FILE, "nginx.conf"];

  it("accepts a path inside the stack", () => {
    for (const p of [
      ".env",
      "config/nginx.conf",
      "a/b/c/d.txt",
      "my file.txt",
      "zażółć.conf",
      "app.prod.env",
    ]) {
      expect(filePathProblem(p, existing), p).toBeNull();
    }
  });

  it("accepts a path of 255 characters and a segment of 100", () => {
    expect(
      filePathProblem(`${"a".repeat(100)}/${"b".repeat(100)}/${"c".repeat(53)}`, existing),
    ).toBeNull();
    expect(filePathProblem("ż".repeat(100), existing)).toBeNull();
  });

  it("refuses what could leave the stack or is not a name", () => {
    const refused = [
      "",
      "/etc/passwd",
      "../x",
      "a/../b",
      "a/..",
      "..",
      ".",
      "a/./b",
      "./x",
      "a//b",
      "a/",
      "a\\b",
      "C:/x",
      "c:/x",
      "C:",
      "a:b",
      "x\u0000",
      "a\nb",
      "a\tb",
      "a\u007fb",
      "a\u202eb",
      "a\u2028b",
      `${"a".repeat(100)}/${"b".repeat(100)}/${"c".repeat(54)}`,
      "a".repeat(101),
      `ok/${"a".repeat(101)}/x`,
      "ż".repeat(101),
    ];
    for (const p of refused)
      expect(filePathProblem(p, existing), JSON.stringify(p)).toBe("invalid");
  });

  it("says taken for a path the stack already holds, and invalid first for a bad one", () => {
    expect(filePathProblem("nginx.conf", existing)).toBe("taken");
    expect(filePathProblem(COMPOSE_FILE, existing)).toBe("taken");
    expect(filePathProblem("Nginx.conf", existing)).toBeNull();
    expect(filePathProblem("../nginx.conf", ["../nginx.conf"])).toBe("invalid");
  });
});

describe("the variables table", () => {
  const withHidden = () =>
    loaded(
      draft({
        vars: [
          { name: "PORT", value: "8080", secret: false },
          { name: "TOKEN", value: "<hidden>", secret: true },
        ],
      }),
    );

  it("adds a variable by name in name order, once", () => {
    const copy = workingCopyReducer(loaded(), { type: "addVar", name: "ALPHA" });
    expect(copy.vars.map((v) => v.name)).toEqual(["ALPHA", "PORT"]);
    expect(copy.vars[0]).toEqual({ name: "ALPHA", value: "", secret: false, hidden: false });
    expect(workingCopyReducer(copy, { type: "addVar", name: "ALPHA" })).toBe(copy);
  });

  it("sets a value, creating the variable when the compose file uses it and the table has none", () => {
    const copy = workingCopyReducer(loaded(), { type: "setVar", name: "WEB_PORT", value: "9090" });
    expect(copy.vars.find((v) => v.name === "WEB_PORT")).toEqual({
      name: "WEB_PORT",
      value: "9090",
      secret: false,
      hidden: false,
    });
    expect(isDirty(loaded(), copy)).toBe(true);
  });

  it("typing over a hidden secret replaces it, and reset puts the hidden value back", () => {
    const typed = workingCopyReducer(withHidden(), { type: "setVar", name: "TOKEN", value: "new" });
    expect(typed.vars.find((v) => v.name === "TOKEN")).toMatchObject({
      value: "new",
      hidden: false,
      secret: true,
    });
    const reset = workingCopyReducer(typed, { type: "resetVar", name: "TOKEN" });
    expect(reset.vars.find((v) => v.name === "TOKEN")).toMatchObject({
      value: "",
      hidden: true,
      secret: true,
    });
    expect(isDirty(withHidden(), reset)).toBe(false);
  });

  it("clears the value and the hidden mark when Secret is turned off on a hidden value", () => {
    const off = workingCopyReducer(withHidden(), {
      type: "setSecret",
      name: "TOKEN",
      secret: false,
    });
    expect(off.vars.find((v) => v.name === "TOKEN")).toEqual({
      name: "TOKEN",
      value: "",
      secret: false,
      hidden: false,
    });
    const on = workingCopyReducer(loaded(), { type: "setSecret", name: "PORT", secret: true });
    expect(on.vars.find((v) => v.name === "PORT")).toEqual({
      name: "PORT",
      value: "8080",
      secret: true,
      hidden: false,
    });
  });

  it("removes a variable", () => {
    const copy = workingCopyReducer(loaded(), { type: "removeVar", name: "PORT" });
    expect(copy.vars).toEqual([]);
  });

  it("sends an untouched hidden secret as the keep marker and a typed one as its text", () => {
    const body = toSaveBody(withHidden(), 3);
    expect(body.vars).toEqual([
      { name: "PORT", value: "8080", secret: false },
      { name: "TOKEN", value: "<hidden>", secret: true },
    ]);
    const typed = workingCopyReducer(withHidden(), {
      type: "setVar",
      name: "TOKEN",
      value: "abcd",
    });
    expect(toSaveBody(typed, 3).vars?.[1]).toEqual({ name: "TOKEN", value: "abcd", secret: true });
  });

  it("lets go of a secret's text once a save of it succeeded, and keeps what was typed since", () => {
    const typed = workingCopyReducer(withHidden(), {
      type: "setVar",
      name: "TOKEN",
      value: "abcd1",
    });
    const afterSave = workingCopyReducer(typed, { type: "saved", sent: typed });
    expect(afterSave.vars.find((v) => v.name === "TOKEN")).toMatchObject({
      value: "",
      hidden: true,
    });
    expect(afterSave.vars.find((v) => v.name === "PORT")?.value).toBe("8080");
    expect(isDirty(withSecretsHidden(typed), afterSave)).toBe(false);

    const typedMore = workingCopyReducer(typed, { type: "setVar", name: "TOKEN", value: "abcd12" });
    const stillTyped = workingCopyReducer(typedMore, { type: "saved", sent: typed });
    expect(stillTyped.vars.find((v) => v.name === "TOKEN")).toMatchObject({
      value: "abcd12",
      hidden: false,
    });
  });

  describe("importVars", () => {
    const imported = [
      { name: "PORT", value: "9090" },
      { name: "NEW_ONE", value: "x" },
    ];

    it("merges: adds the new names and replaces the value of a name that clashes", () => {
      const copy = workingCopyReducer(withHidden(), {
        type: "importVars",
        vars: imported,
        mode: "merge",
        secret: false,
      });
      expect(copy.vars).toEqual([
        { name: "NEW_ONE", value: "x", secret: false, hidden: false },
        { name: "PORT", value: "9090", secret: false, hidden: false },
        { name: "TOKEN", value: "", secret: true, hidden: true },
      ]);
      expect(isDirty(withHidden(), copy)).toBe(true);
    });

    it("replaces: the table is exactly the imported names", () => {
      const copy = workingCopyReducer(withHidden(), {
        type: "importVars",
        vars: imported,
        mode: "replace",
        secret: false,
      });
      expect(copy.vars.map((v) => v.name)).toEqual(["NEW_ONE", "PORT"]);
    });

    it("marks every imported value as secret when asked, and never turns a secret plain", () => {
      const marked = workingCopyReducer(loaded(), {
        type: "importVars",
        vars: imported,
        mode: "merge",
        secret: true,
      });
      expect(marked.vars.every((v) => v.secret)).toBe(true);
      const kept = workingCopyReducer(withHidden(), {
        type: "importVars",
        vars: [{ name: "TOKEN", value: "from-the-file" }],
        mode: "merge",
        secret: false,
      });
      expect(kept.vars.find((v) => v.name === "TOKEN")).toEqual({
        name: "TOKEN",
        value: "from-the-file",
        secret: true,
        hidden: false,
      });
    });
  });
});
