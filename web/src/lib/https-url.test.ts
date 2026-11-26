// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { httpsUrl } from "@/lib/https-url";

describe("httpsUrl", () => {
  it("passes an https address through unchanged", () => {
    expect(httpsUrl("https://letsencrypt.org/documents/LE-SA-v1.5.pdf")).toBe(
      "https://letsencrypt.org/documents/LE-SA-v1.5.pdf",
    );
  });

  it("drops everything else", () => {
    for (const v of [
      undefined,
      "",
      "javascript:alert(1)",
      "data:text/html,<script>1</script>",
      "http://example.com/terms",
      "not a url",
    ]) {
      expect(httpsUrl(v)).toBeUndefined();
    }
  });
});
