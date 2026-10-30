// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { newRow } from "@/components/key-value-editor";
import {
  checkLabelRows,
  isReservedLabel,
  MAX_LABEL_KEY_BYTES,
  MAX_LABEL_VALUE_BYTES,
  MAX_LABELS,
  reservedPrefixOf,
} from "@/features/nodes/reserved-labels";

// The label rules the editor shows before the server says them (plan 06-16, RES-05, E15). They are
// the server's: Docker's reserved prefixes compared without regard to the case of ASCII letters,
// and lengths in UTF-8 bytes.

describe("isReservedLabel", () => {
  it.each([
    ["com.docker.x", true],
    ["COM.Docker.x", true],
    ["com.docker.", true],
    ["io.docker.y", true],
    ["Io.DOCKER.y", true],
    ["org.dockerproject.z", true],
    ["ORG.DockerProject.z", true],
    ["com.dockerish", false],
    ["com.docker", false],
    ["disk", false],
    ["x.com.docker.y", false],
    ["org.dockerprojects.z", false],
    ["io.dockerx", false],
    ["", false],
    // The Kelvin sign folds to a k in Unicode, but it is not an ASCII letter.
    ["com.docKer.x", false],
  ])("%j is reserved: %s", (key, want) => {
    expect(isReservedLabel(key)).toBe(want);
  });

  it("names the prefix a key starts with, as it is written in the rule", () => {
    expect(reservedPrefixOf("COM.Docker.x")).toBe("com.docker.");
    expect(reservedPrefixOf("io.docker.y")).toBe("io.docker.");
    expect(reservedPrefixOf("disk")).toBeNull();
  });
});

describe("checkLabelRows", () => {
  it("has no problem for rows that are fine, and ignores a row with no name", () => {
    const rows = [newRow("disk", "ssd"), newRow("", "ignored"), newRow("zone", "")];
    const check = checkLabelRows(rows);
    expect(check.rows.size).toBe(0);
    expect(check.tooMany).toBe(false);
    expect(check.valid).toBe(true);
  });

  it("flags a reserved name on its row", () => {
    const bad = newRow("com.docker.swarm.x", "1");
    const check = checkLabelRows([newRow("a", "1"), bad]);
    expect(check.rows.get(bad.id)).toEqual({ kind: "reserved", prefix: "com.docker." });
    expect(check.valid).toBe(false);
  });

  it("counts a key and a value in UTF-8 bytes", () => {
    const keyOk = newRow("é".repeat(MAX_LABEL_KEY_BYTES / 2), "v");
    const keyTooLong = newRow("é".repeat(MAX_LABEL_KEY_BYTES / 2 + 1), "v");
    const valueOk = newRow("a", "v".repeat(MAX_LABEL_VALUE_BYTES));
    const valueTooLong = newRow("b", "é".repeat(MAX_LABEL_VALUE_BYTES / 2 + 1));
    const check = checkLabelRows([keyOk, keyTooLong, valueOk, valueTooLong]);
    expect(check.rows.has(keyOk.id)).toBe(false);
    expect(check.rows.get(keyTooLong.id)).toEqual({ kind: "keyTooLong" });
    expect(check.rows.has(valueOk.id)).toBe(false);
    expect(check.rows.get(valueTooLong.id)).toEqual({ kind: "valueTooLong" });
  });

  it("flags a name used twice on the later row", () => {
    const first = newRow("disk", "ssd");
    const second = newRow(" disk ", "hdd");
    const check = checkLabelRows([first, second]);
    expect(check.rows.has(first.id)).toBe(false);
    expect(check.rows.get(second.id)).toEqual({ kind: "duplicate" });
  });

  it("allows 64 labels and flags 65", () => {
    const sixtyFour = Array.from({ length: MAX_LABELS }, (_, i) => newRow(`k${i}`, "v"));
    expect(checkLabelRows(sixtyFour).tooMany).toBe(false);
    const sixtyFive = [...sixtyFour, newRow("one-more", "v")];
    const check = checkLabelRows(sixtyFive);
    expect(check.tooMany).toBe(true);
    expect(check.valid).toBe(false);
  });
});
