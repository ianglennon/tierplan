// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it, vi } from "vitest";

import type { ObjectValue } from "@/features/secrets/value-source-field";
import { createStagedStore, hasValue, objectsForApply } from "@/features/service-editor/staged";

// The staged store (plan 06-26, D-06, D-07; UI-SPEC "Service editor", "Review and apply"): the
// objects the editor will create when the service is applied, held in memory with their values.

const typed = (text: string): ObjectValue => ({ source: "type", text, file: null });
const fileValue = (bytes: Uint8Array): ObjectValue => ({
  source: "file",
  text: "",
  file: { name: "key.pem", size: bytes.length, bytes },
});

describe("createStagedStore", () => {
  it("holds objects by kind and name, replacing one put again", () => {
    const store = createStagedStore();
    store.put({ kind: "secret", name: "db", value: typed("one") });
    store.put({ kind: "secret", name: "db", value: typed("two") });
    store.put({ kind: "config", name: "db", value: typed("cfg") });
    expect(store.list().map((o) => `${o.kind}:${o.name}`)).toEqual(["secret:db", "config:db"]);
    expect(store.get("secret", "db")?.value?.text).toBe("two");
    expect(store.get("network", "db")).toBeUndefined();
  });

  it("removes one object and tells subscribers about every change", () => {
    const store = createStagedStore();
    const seen = vi.fn();
    const off = store.subscribe(seen);
    store.put({ kind: "network", name: "front" });
    store.remove("network", "front");
    expect(store.list()).toEqual([]);
    expect(seen).toHaveBeenCalledTimes(2);
    off();
    store.put({ kind: "network", name: "back" });
    expect(seen).toHaveBeenCalledTimes(2);
  });

  it("gives a new snapshot only when something changed, so React can compare it", () => {
    const store = createStagedStore();
    const before = store.getSnapshot();
    expect(store.getSnapshot()).toBe(before);
    store.put({ kind: "network", name: "front" });
    expect(store.getSnapshot()).not.toBe(before);
  });

  it("sets a value on an object that is not staged yet, keeping the rest of it", () => {
    const store = createStagedStore();
    store.setValue("secret", "db", typed("hunter2"));
    expect(store.get("secret", "db")?.value?.text).toBe("hunter2");
    store.put({ kind: "config", name: "app", templating: "golang" });
    store.setValue("config", "app", typed("x=1"));
    expect(store.get("config", "app")).toMatchObject({
      templating: "golang",
      value: { text: "x=1" },
    });
  });

  it("clears every value on reset, zeroing the bytes of a file", () => {
    const store = createStagedStore();
    const bytes = new Uint8Array([1, 2, 3, 4]);
    store.put({ kind: "secret", name: "key", value: fileValue(bytes) });
    store.put({ kind: "secret", name: "db", value: typed("hunter2") });
    const kept = store.get("secret", "db");
    store.reset();
    expect(store.list()).toEqual([]);
    expect(Array.from(bytes)).toEqual([0, 0, 0, 0]);
    expect(kept?.value?.text).not.toBe("hunter2");
  });
});

describe("hasValue", () => {
  it("is true for a usable value and false for none, an empty one or one over the limit", () => {
    expect(hasValue({ kind: "secret", name: "a", value: typed("x") })).toBe(true);
    expect(hasValue({ kind: "secret", name: "a" })).toBe(false);
    expect(hasValue({ kind: "secret", name: "a", value: typed("") })).toBe(false);
    expect(
      hasValue({ kind: "config", name: "a", value: { source: "file", text: "", file: null } }),
    ).toBe(false);
    const over = { name: "big", size: 600 * 1024, bytes: null };
    expect(
      hasValue({ kind: "secret", name: "a", value: { source: "file", text: "", file: over } }),
    ).toBe(false);
  });

  it("is true for a network, which has no value to add", () => {
    expect(hasValue({ kind: "network", name: "front" })).toBe(true);
  });
});

describe("objectsForApply", () => {
  const b64 = (s: string) => btoa(s);

  it("sends every missing network, secret and config that has what it needs, values as base64", () => {
    const store = createStagedStore();
    store.put({ kind: "network", name: "front", subnet: "10.9.0.0/24" });
    store.put({ kind: "secret", name: "db", value: typed("hunter2") });
    store.put({ kind: "config", name: "app", value: typed("x=1"), templating: "golang" });
    const got = objectsForApply(
      { networks: ["front", "back"], secrets: ["db"], configs: ["app"] },
      store,
    );
    expect(got.lacking).toBe(0);
    expect(got.objects).toEqual([
      { kind: "network", name: "front", subnet: "10.9.0.0/24" },
      { kind: "network", name: "back" },
      { kind: "secret", name: "db", data: b64("hunter2") },
      { kind: "config", name: "app", data: b64("x=1"), templating: "golang" },
    ]);
  });

  it("carries createAnyway on a network that has a subnet, and only then", () => {
    const store = createStagedStore();
    store.put({ kind: "network", name: "front", subnet: "10.9.0.0/25", createAnyway: true });
    store.put({ kind: "network", name: "back", createAnyway: true });
    const got = objectsForApply({ networks: ["front", "back"], secrets: [], configs: [] }, store);
    expect(got.objects).toEqual([
      { kind: "network", name: "front", subnet: "10.9.0.0/25", createAnyway: true },
      { kind: "network", name: "back" },
    ]);
  });

  it("encodes a value as UTF-8, and a file as its bytes", () => {
    const store = createStagedStore();
    store.put({ kind: "secret", name: "pw", value: typed("pässword") });
    store.put({
      kind: "secret",
      name: "key",
      value: fileValue(new Uint8Array([0xff, 0x00, 0x7f])),
    });
    const got = objectsForApply({ networks: [], secrets: ["pw", "key"], configs: [] }, store);
    expect(got.objects).toEqual([
      { kind: "secret", name: "pw", data: b64("p\xc3\xa4ssword") },
      { kind: "secret", name: "key", data: b64("\xff\x00\x7f") },
    ]);
  });

  it("counts the secrets and configs that still lack a value and sends none of them", () => {
    const store = createStagedStore();
    store.put({ kind: "secret", name: "db", value: typed("hunter2") });
    const got = objectsForApply(
      { networks: [], secrets: ["db", "other"], configs: ["app"] },
      store,
    );
    expect(got.lacking).toBe(2);
    expect(got.objects.map((o) => o.name)).toEqual(["db"]);
  });

  it("leaves out a staged object that nothing names any more", () => {
    const store = createStagedStore();
    store.put({ kind: "secret", name: "gone", value: typed("x") });
    expect(objectsForApply({ networks: [], secrets: [], configs: [] }, store)).toEqual({
      objects: [],
      lacking: 0,
    });
  });
});
