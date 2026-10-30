// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { afterEach, describe, expect, it } from "vitest";
import {
  hasYamlComments,
  loadSpecFormat,
  parseSpec,
  renderSpec,
  SPEC_FORMAT_KEY,
  type SpecFormat,
  saveSpecFormat,
} from "@/features/spec-editor/spec-format";
import fullSpec from "@/features/spec-editor/testdata/full-spec.json";
import "@/i18n";

// The lossless spec round trip (plan 06-19, D-01): a spec is converted between YAML and JSON
// through its parsed value, never through its text, so what Docker would store is what comes back.

const FORMATS: SpecFormat[] = ["yaml", "json"];

function parsed(text: string, format: SpecFormat) {
  const result = parseSpec(text, format);
  if (!result.ok) throw new Error(`line ${result.line}: ${result.message}`);
  return result.value;
}

// The text of a value with its keys in the order they are held: toEqual does not look at order.
const ordered = (value: unknown) => JSON.stringify(value);

describe("renderSpec and parseSpec", () => {
  it.each(FORMATS)("round-trips every field type of a swarm service spec in %s", (format) => {
    const back = parsed(renderSpec(fullSpec, format), format);
    expect(back).toEqual(fullSpec);
  });

  it.each(FORMATS)("keeps the order of the keys through %s", (format) => {
    const back = parsed(renderSpec(fullSpec, format), format);
    expect(ordered(back)).toBe(ordered(fullSpec));
  });

  it("keeps the parsed value from YAML to JSON to YAML", () => {
    const x = parsed(renderSpec(fullSpec, "yaml"), "yaml");
    const viaJson = parsed(renderSpec(x, "json"), "json");
    const back = parsed(renderSpec(viaJson, "yaml"), "yaml");
    expect(back).toEqual(x);
    expect(ordered(back)).toBe(ordered(x));
  });

  it("keeps the parsed value from JSON to YAML to JSON", () => {
    const x = parsed(renderSpec(fullSpec, "json"), "json");
    const viaYaml = parsed(renderSpec(x, "yaml"), "yaml");
    const back = parsed(renderSpec(viaYaml, "json"), "json");
    expect(back).toEqual(x);
    expect(ordered(back)).toBe(ordered(x));
  });

  it("never folds a long line in YAML", () => {
    const long = "word ".repeat(80).trim();
    const text = renderSpec({ Name: "web", Labels: { long } }, "yaml");
    expect(text.split("\n").some((line) => line.includes(long))).toBe(true);
    expect(parsed(text, "yaml")).toEqual({ Name: "web", Labels: { long } });
  });

  it("writes the spec as an indented JSON document", () => {
    expect(renderSpec({ Name: "web", Labels: {} }, "json")).toBe(
      '{\n  "Name": "web",\n  "Labels": {}\n}',
    );
  });

  const AMBIGUOUS = [
    "yes",
    "no",
    "on",
    "off",
    "y",
    "n",
    "true",
    "false",
    "null",
    "~",
    "0444",
    "0o17",
    "0x1f",
    "1e3",
    "1_000",
    ".inf",
    ".nan",
    "12",
    "-3.5",
    "",
    " ",
    "2026-10-15",
    "a: b",
    "- item",
    "# comment",
    "&anchor",
    "*alias",
    "!tag",
    "{x}",
    "[x]",
    "'quoted'",
    '"double"',
    "line one\nline two",
    "trailing space ",
  ];

  it.each(FORMATS)("keeps strings that look like something else as strings in %s", (format) => {
    const value = {
      Name: "web",
      Labels: Object.fromEntries(AMBIGUOUS.map((s, i) => [`k${i}`, s])),
    };
    const back = parsed(renderSpec(value, format), format);
    expect(back).toEqual(value);
    for (const v of Object.values(back.Labels as Record<string, unknown>)) {
      expect(typeof v).toBe("string");
    }
  });

  it("quotes the words an older YAML reads as booleans", () => {
    const text = renderSpec(
      { Name: "web", Labels: { a: "yes", b: "no", c: "on", d: "off" } },
      "yaml",
    );
    expect(text).toContain('a: "yes"');
    expect(text).toContain('d: "off"');
  });

  it("quotes a key that looks like another type", () => {
    const value = { Name: "web", Labels: { yes: "1", "0444": "2", null: "3", "": "4" } };
    expect(parsed(renderSpec(value, "yaml"), "yaml")).toEqual(value);
  });

  it.each(FORMATS)("keeps numbers, booleans and nulls as they are in %s", (format) => {
    const value = { a: 0, b: -7, c: 1.5, d: 4294967296, e: true, f: false, g: null, h: [] };
    const back = parsed(renderSpec(value, format), format);
    expect(back).toEqual(value);
    expect(typeof back.a).toBe("number");
    expect(back.g).toBeNull();
  });

  it("holds an integer above 2^31 and one at the largest safe integer exactly", () => {
    const value = {
      a: 2 ** 31,
      b: 2 ** 40,
      c: Number.MAX_SAFE_INTEGER,
      d: -Number.MAX_SAFE_INTEGER,
    };
    for (const format of FORMATS) {
      expect(parsed(renderSpec(value, format), format)).toEqual(value);
    }
  });
});

describe("parseSpec errors", () => {
  it("names the line of a YAML syntax error", () => {
    const result = parseSpec("Name: web\nLabels:\n  a: 1\n  - b\n", "yaml");
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.line).toBe(4);
      expect(result.message).not.toBe("");
      expect(result.message).not.toMatch(/line \d/i);
    }
  });

  it("names the line of a JSON syntax error", () => {
    const result = parseSpec('{\n  "Name": "web",\n  "Labels": }\n', "json");
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.line).toBe(3);
      expect(result.message).not.toBe("");
    }
  });

  it.each([
    ['{"a": 1,}', 1],
    ['{"a": 1\n"b": 2}', 2],
    ["{'a': 1}", 1],
    ['{"a": 01}', 1],
    ['{"a": 1} {"b": 2}', 1],
    ['{"a": "x\ny"}', 1],
    ["", 1],
    ['{"a": tru}', 1],
    ['{"a": [1, 2}', 1],
  ] as const)("refuses the JSON %j", (text, line) => {
    const result = parseSpec(text, "json");
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.line).toBe(line);
  });

  it("refuses JSON that YAML would accept", () => {
    expect(parseSpec('{"a": 1, // note\n "b": 2}', "json").ok).toBe(false);
    expect(parseSpec("{a: 1}", "json").ok).toBe(false);
  });

  it.each(FORMATS)("refuses a spec that is not an object in %s", (format) => {
    for (const text of ["[]", "42", '"x"', "null", ""]) {
      const result = parseSpec(text, format);
      expect(result.ok, text).toBe(false);
    }
  });

  it.each([
    [
      "yaml",
      "Name: web\nTaskTemplate:\n  Resources:\n    Limits:\n      MemoryBytes: 9007199254740993\n",
    ],
    [
      "json",
      '{\n  "Name": "web",\n  "TaskTemplate": {\n    "Resources": {\n      "Limits": {\n        "MemoryBytes": 9007199254740993\n      }\n    }\n  }\n}',
    ],
  ] as const)(
    "refuses an integer that is not a safe JavaScript integer in %s, naming its path",
    (format, text) => {
      const result = parseSpec(text, format);
      expect(result.ok).toBe(false);
      if (!result.ok) {
        expect(result.message).toContain("TaskTemplate.Resources.Limits.MemoryBytes");
        expect(result.line).toBe(format === "yaml" ? 5 : 6);
      }
    },
  );

  it.each(FORMATS)(
    "names a list entry's place in the path of an unsafe integer in %s",
    (format) => {
      const text =
        format === "yaml"
          ? "Name: web\nPorts:\n  - 80\n  - 18446744073709551615\n"
          : '{"Name": "web", "Ports": [80, 18446744073709551615]}';
      const result = parseSpec(text, format);
      expect(result.ok).toBe(false);
      if (!result.ok) expect(result.message).toContain("Ports[1]");
    },
  );

  it("accepts the largest safe integer and refuses the next one", () => {
    expect(parseSpec(`{"a": ${Number.MAX_SAFE_INTEGER}}`, "json").ok).toBe(true);
    expect(parseSpec(`{"a": ${Number.MAX_SAFE_INTEGER + 2}}`, "json").ok).toBe(false);
    expect(parseSpec(`a: ${Number.MAX_SAFE_INTEGER}`, "yaml").ok).toBe(true);
    expect(parseSpec("a: 9007199254740992", "yaml").ok).toBe(false);
    expect(parseSpec("a: -9007199254740993", "yaml").ok).toBe(false);
  });

  it("refuses a number JSON cannot hold in YAML", () => {
    expect(parseSpec("a: .inf", "yaml").ok).toBe(false);
    expect(parseSpec("a: .nan", "yaml").ok).toBe(false);
  });

  it("refuses a YAML document with two documents in it", () => {
    expect(parseSpec("a: 1\n---\nb: 2\n", "yaml").ok).toBe(false);
  });

  it("refuses a repeated key", () => {
    expect(parseSpec("a: 1\na: 2\n", "yaml").ok).toBe(false);
  });

  it("does not let a key named __proto__ change the value's prototype", () => {
    const yaml = parseSpec("__proto__:\n  polluted: true\nName: web\n", "yaml");
    const json = parseSpec('{"__proto__": {"polluted": true}, "Name": "web"}', "json");
    for (const r of [yaml, json]) {
      expect(r.ok).toBe(true);
      expect(({} as Record<string, unknown>).polluted).toBeUndefined();
      if (r.ok) expect((r.value as Record<string, unknown>).polluted).toBeUndefined();
    }
  });

  it("reads JSON escapes, surrogate pairs and exponents", () => {
    const r = parseSpec(
      '{"a": "\\u00e9\\ud83d\\ude00\\n\\t\\/\\"", "b": 1E3, "c": -0.5e-2}',
      "json",
    );
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.value).toEqual({ a: 'é😀\n\t/"', b: 1000, c: -0.005 });
  });
});

describe("hasYamlComments", () => {
  it("finds a comment on its own line and one after a value", () => {
    expect(hasYamlComments("# the spec\nName: web\n")).toBe(true);
    expect(hasYamlComments("Name: web # the name\n")).toBe(true);
    expect(hasYamlComments("Labels:\n  a: 1\n  # last\n")).toBe(true);
  });

  it("does not take a hash inside a string for a comment", () => {
    expect(hasYamlComments('Name: "web # not a comment"\nURL: http://x/#frag\n')).toBe(false);
  });

  it("is false for a spec the editor wrote", () => {
    expect(hasYamlComments(renderSpec(fullSpec, "yaml"))).toBe(false);
  });

  it("is false for text that does not parse", () => {
    expect(hasYamlComments("a: [1,")).toBe(false);
  });
});

describe("the remembered format", () => {
  afterEach(() => localStorage.clear());

  it("is YAML until a person chooses", () => {
    expect(loadSpecFormat()).toBe("yaml");
  });

  it("remembers the choice under coaming.spec-format", () => {
    saveSpecFormat("json");
    expect(localStorage.getItem(SPEC_FORMAT_KEY)).toBe("json");
    expect(SPEC_FORMAT_KEY).toBe("coaming.spec-format");
    expect(loadSpecFormat()).toBe("json");
    saveSpecFormat("yaml");
    expect(loadSpecFormat()).toBe("yaml");
  });

  it("falls back to YAML for anything else that is stored", () => {
    localStorage.setItem(SPEC_FORMAT_KEY, "toml");
    expect(loadSpecFormat()).toBe("yaml");
  });

  it("stores only the format", () => {
    saveSpecFormat("json");
    expect(localStorage.length).toBe(1);
  });
});
