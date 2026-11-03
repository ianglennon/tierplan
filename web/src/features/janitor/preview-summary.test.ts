// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import i18n from "@/i18n";
import { formatSize, summaryLine, type Translate } from "./preview-summary";

// The words come from the janitor namespace, read the way useT reads it.
const t: Translate = (key, options) => String(i18n.t(key, { ...options, ns: key.split(".")[0] }));

describe("the summary line", () => {
  it("says 1 container, not 1 containers", () => {
    expect(summaryLine({ containers: 1 }, t)).toBe("Would remove 1 container");
  });

  it("leaves out the parts that are zero", () => {
    expect(summaryLine({ containers: 14, networks: 0, images: 2 }, t)).toBe(
      "Would remove 14 containers, 2 images",
    );
    expect(summaryLine({ networks: 1 }, t)).toBe("Would remove 1 network");
  });

  it("ends with the estimated size when one is known", () => {
    expect(
      summaryLine({ containers: 14, networks: 3, images: 2, estBytes: 1_200_000_000 }, t),
    ).toBe("Would remove 14 containers, 3 networks, 2 images, ~1.2 GB");
  });

  it("leaves the size out when it is not known, which is not zero", () => {
    expect(summaryLine({ containers: 2, estBytes: null }, t)).toBe("Would remove 2 containers");
  });

  it("says Nothing selected when everything is zero, whatever the size", () => {
    expect(summaryLine({}, t)).toBe("Nothing selected");
    expect(summaryLine({ containers: 0, networks: 0, images: 0, estBytes: 5_000 }, t)).toBe(
      "Nothing selected",
    );
  });

  it("groups the digits of a big count", () => {
    expect(summaryLine({ containers: 2000 }, t)).toBe("Would remove 2,000 containers");
  });
});

describe("the size", () => {
  it("is one decimal in the unit that fits", () => {
    expect(formatSize(1_200_000_000, t)).toBe("1.2 GB");
    expect(formatSize(1_200_000, t)).toBe("1.2 MB");
    expect(formatSize(512, t)).toBe("512 byte");
  });

  it("is a dash when it is not known, never zero", () => {
    expect(formatSize(null, t)).toBe("—");
    expect(formatSize(undefined, t)).toBe("—");
  });
});
