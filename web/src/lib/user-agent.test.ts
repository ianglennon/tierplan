// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { describeDevice } from "@/lib/user-agent";

const CASES: ReadonlyArray<[string, string, { browser: string; os: string }]> = [
  [
    "Chrome on Windows",
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
    { browser: "Chrome", os: "Windows" },
  ],
  [
    "Edge on Windows",
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.0.0",
    { browser: "Edge", os: "Windows" },
  ],
  [
    "Firefox on Linux",
    "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0",
    { browser: "Firefox", os: "Linux" },
  ],
  [
    "Chrome on Linux",
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
    { browser: "Chrome", os: "Linux" },
  ],
  [
    "Safari on macOS",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
    { browser: "Safari", os: "macOS" },
  ],
  [
    "Firefox on macOS",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:131.0) Gecko/20100101 Firefox/131.0",
    { browser: "Firefox", os: "macOS" },
  ],
  [
    "Chrome on Android, not Linux",
    "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Mobile Safari/537.36",
    { browser: "Chrome", os: "Android" },
  ],
  [
    "Firefox on Android",
    "Mozilla/5.0 (Android 14; Mobile; rv:131.0) Gecko/131.0 Firefox/131.0",
    { browser: "Firefox", os: "Android" },
  ],
  [
    "Safari on iOS, not macOS",
    "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
    { browser: "Safari", os: "iOS" },
  ],
  [
    "Chrome on iPad",
    "Mozilla/5.0 (iPad; CPU OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/129.0.6668.69 Mobile/15E148 Safari/604.1",
    { browser: "Chrome", os: "iOS" },
  ],
  [
    "Firefox on iOS",
    "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/131.0 Mobile/15E148 Safari/605.1.15",
    { browser: "Firefox", os: "iOS" },
  ],
  [
    "Edge on Android",
    "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Mobile Safari/537.36 EdgA/129.0.0.0",
    { browser: "Edge", os: "Android" },
  ],
];

describe("describeDevice", () => {
  it.each(CASES)("reads %s", (_name, ua, want) => {
    expect(describeDevice(ua)).toEqual(want);
  });

  it("returns null for an empty agent, a command line client and a browser it does not know", () => {
    expect(describeDevice("")).toBeNull();
    expect(describeDevice("curl/8.5.0")).toBeNull();
    expect(describeDevice("Go-http-client/1.1")).toBeNull();
    expect(
      describeDevice(
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 OPR/114.0.0.0",
      ),
    ).toBeNull();
  });

  it("returns null when the system is unknown, and never repeats anything from the agent", () => {
    expect(
      describeDevice("Mozilla/5.0 (PlayStation 5) AppleWebKit/605.1.15 Chrome/129.0.0.0"),
    ).toBeNull();
    const hostile = "<script>alert(1)</script> Firefox/131.0 (X11; Linux x86_64)";
    expect(describeDevice(hostile)).toEqual({ browser: "Firefox", os: "Linux" });
  });
});
