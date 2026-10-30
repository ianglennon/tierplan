// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { parseDotenv } from "@/features/stack-editor/dotenv";

// The .env reader of the Import dialog (plan 07-12, D-04). It is a client-side convenience for
// filling the table; the one parser a deploy uses is compose-go's on the server.

describe("parseDotenv", () => {
  it("reads NAME=value, export NAME=value and an empty value", () => {
    const got = parseDotenv("A=1\nexport B=two\nC=\n");
    expect(got.vars).toEqual([
      { name: "A", value: "1" },
      { name: "B", value: "two" },
      { name: "C", value: "" },
    ]);
    expect(got.ignored).toEqual([]);
  });

  it("takes single quotes literally and expands the escapes of double quotes", () => {
    const got = parseDotenv('A=\'a\\nb\'\nB="a\\nb"\nC="say \\"hi\\""\nD="x\\qy"\n');
    expect(got.vars).toEqual([
      { name: "A", value: "a\\nb" },
      { name: "B", value: "a\nb" },
      { name: "C", value: 'say "hi"' },
      { name: "D", value: "x\\qy" },
    ]);
  });

  it("drops a comment after a space on an unquoted value, but not a # inside a word or a quote", () => {
    const got = parseDotenv('A=1 # one\nB=a#b\nC="x # y" # z\n');
    expect(got.vars).toEqual([
      { name: "A", value: "1" },
      { name: "B", value: "a#b" },
      { name: "C", value: "x # y" },
    ]);
    expect(got.ignored).toEqual([]);
  });

  it("reads a quoted value that runs over several lines", () => {
    const got = parseDotenv('KEY="line one\nline two"\nNEXT=1\n');
    expect(got.vars).toEqual([
      { name: "KEY", value: "line one\nline two" },
      { name: "NEXT", value: "1" },
    ]);
  });

  it("reports comments, blank lines and malformed lines with their numbers", () => {
    const got = parseDotenv("A=1\nB=\n# c\nbad line\n\n9X=1\nOK=2");
    expect(got.vars.map((v) => v.name)).toEqual(["A", "B", "OK"]);
    expect(got.ignored).toEqual([
      { line: 3, text: "# c", reason: "comment" },
      { line: 4, text: "bad line", reason: "malformed" },
      { line: 5, text: "", reason: "blank" },
      { line: 6, text: "9X=1", reason: "malformed" },
    ]);
  });

  it("calls an unterminated quote, text after a closing quote and a name with a dash malformed", () => {
    const got = parseDotenv('A="never closed\nB="x" y\nC-D=1\n');
    expect(got.vars).toEqual([]);
    expect(got.ignored.map((l) => [l.line, l.reason])).toEqual([
      [1, "malformed"],
      [2, "malformed"],
      [3, "malformed"],
    ]);
  });

  it("keeps the last value of a name written twice, in the place it first appeared", () => {
    expect(parseDotenv("A=1\nB=2\nA=3\n").vars).toEqual([
      { name: "A", value: "3" },
      { name: "B", value: "2" },
    ]);
  });

  it("reads Windows line endings and a byte order mark", () => {
    const got = parseDotenv("\uFEFFA=1\r\nB=2\r\n");
    expect(got.vars).toEqual([
      { name: "A", value: "1" },
      { name: "B", value: "2" },
    ]);
    expect(got.ignored).toEqual([]);
  });

  it("does not count the end of the last line as a blank line", () => {
    expect(parseDotenv("A=1\n").ignored).toEqual([]);
    expect(parseDotenv("").ignored).toEqual([]);
    expect(parseDotenv("\n\n").ignored.map((l) => l.reason)).toEqual(["blank", "blank"]);
  });

  it("refuses a name longer than the server accepts", () => {
    const got = parseDotenv(`${"A".repeat(129)}=1\n${"B".repeat(128)}=2\n`);
    expect(got.vars.map((v) => v.name.length)).toEqual([128]);
    expect(got.ignored).toHaveLength(1);
  });
});
