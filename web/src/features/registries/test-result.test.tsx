// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { CredentialTestResult } from "@/api/registries";
import { TooltipProvider } from "@/components/ui/tooltip";
import { LastTestCell } from "@/features/registries/credential-table";
import { TestResult } from "@/features/registries/test-result";
import "@/i18n";

afterEach(cleanup);

const passed = { state: "passed", code: "" } as const;
const notChecked = { state: "not_checked", code: "" } as const;

function result(extra: Partial<CredentialTestResult> = {}): CredentialTestResult {
  return {
    reachable: passed,
    signedIn: passed,
    canPull: passed,
    testedAt: "2026-10-12T10:00:00Z",
    ...extra,
  };
}

function show(
  r: CredentialTestResult,
  over: { repository?: string; type?: "ghcr"; stale?: boolean } = {},
) {
  render(
    <TestResult
      result={r}
      host="ghcr.io"
      repository={over.repository ?? "acme/app"}
      type={over.type ?? "ghcr"}
      stale={over.stale}
    />,
  );
  return screen.getByRole("status");
}

describe("TestResult", () => {
  it("lists the three steps with their states as text", () => {
    const region = show(result());
    const items = within(region).getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual([
      "Registry reachablePassed",
      "Signed inPassed",
      "Can pull acme/appPassed",
    ]);
  });

  it("announces a pass and a failure with the reason, once, as a status", () => {
    const region = show(
      result({ signedIn: { state: "failed", code: "sign_in_refused" }, canPull: notChecked }),
    );
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(
      within(region).getByText("Test failed: ghcr.io refused this username and secret."),
    ).toBeTruthy();
    expect(within(region).getAllByText("ghcr.io refused this username and secret.")).toHaveLength(
      1,
    );
    expect(within(region).getByText("Failed")).toBeTruthy();
    expect(within(region).getByText("Not checked")).toBeTruthy();
  });

  it("says Test passed for a clean result", () => {
    const region = show(result());
    expect(within(region).getByText("Test passed")).toBeTruthy();
  });

  it("maps each code to its fixed copy and never shows registry text", () => {
    const cases: [string, string][] = [
      ["registry_unreachable", "Couldn't reach ghcr.io. Check the address, DNS and firewall."],
      [
        "registry_tls",
        "The certificate from ghcr.io isn't trusted. Add the registry's CA under Advanced.",
      ],
      ["registry_http", "ghcr.io answered, but not like a Docker registry."],
      ["rate_limited", "ghcr.io is limiting requests. Try again in a moment."],
      ["check_failed", "The test couldn't finish. Try again."],
      ["<img src=x onerror=alert(1)>", "The test couldn't finish. Try again."],
    ];
    for (const [code, copy] of cases) {
      cleanup();
      const region = show(
        result({ reachable: { state: "failed", code }, signedIn: notChecked, canPull: notChecked }),
      );
      expect(within(region).getAllByText(copy).length).toBeGreaterThan(0);
      expect(region.querySelector("img")).toBeNull();
    }
  });

  it("says a missing pull scope in the words of the spec", () => {
    const region = show(result({ canPull: { state: "failed", code: "no_pull_access" } }));
    expect(
      within(region).getAllByText(
        "Signed in, but this credential can't pull acme/app. Check its scopes.",
      ).length,
    ).toBeGreaterThan(0);
  });

  it("says a registry that asks for no login was not checked, with why", () => {
    const region = show(
      result({ signedIn: { state: "not_checked", code: "no_login_asked" }, canPull: notChecked }),
    );
    expect(
      within(region).getByText("ghcr.io asks for no login, so there was nothing to sign in to."),
    ).toBeTruthy();
  });

  it("adds the hint to name a repository for a refused GHCR token that was tested without one", () => {
    const refused = result({
      signedIn: { state: "failed", code: "sign_in_refused" },
      canPull: notChecked,
    });
    show(refused, { repository: "" });
    expect(screen.getByText(/Add a test repository and test again/)).toBeTruthy();
    cleanup();
    show(refused, { repository: "acme/app" });
    expect(screen.queryByText(/Add a test repository and test again/)).toBeNull();
  });

  it("greys out a stale result and says it is for the earlier values, without announcing it", () => {
    const region = show(result(), { stale: true });
    expect(region.getAttribute("aria-live")).toBe("off");
    expect(region.className).toContain("opacity-60");
    expect(within(region).getByText("Result is for the earlier values")).toBeTruthy();
  });
});

describe("LastTestCell", () => {
  const at = "2026-10-12T10:00:00Z";

  it("shows Not tested, Passed and Failed with a time", () => {
    const { rerender } = render(
      <TooltipProvider>
        <LastTestCell test={null} />
      </TooltipProvider>,
    );
    expect(screen.getByText("Not tested")).toBeTruthy();
    rerender(
      <TooltipProvider>
        <LastTestCell test={{ at, ok: true, detail: "" }} />
      </TooltipProvider>,
    );
    expect(screen.getByText("Passed")).toBeTruthy();
    rerender(
      <TooltipProvider>
        <LastTestCell test={{ at, ok: false, detail: "sign_in_refused" }} />
      </TooltipProvider>,
    );
    expect(screen.getByText("Failed")).toBeTruthy();
  });

  it("words the stored code in the tooltip and shows any other detail as text", async () => {
    const cell = (detail: string) => (
      <TooltipProvider>
        <LastTestCell test={{ at, ok: false, detail }} />
      </TooltipProvider>
    );
    render(cell("sign_in_refused"));
    fireEvent.focus(screen.getByText("Failed"));
    expect(
      (await screen.findAllByText("The registry refused this username and secret.")).length,
    ).toBeGreaterThan(0);
    cleanup();
    render(cell("something else"));
    fireEvent.focus(screen.getByText("Failed"));
    expect((await screen.findAllByText("something else")).length).toBeGreaterThan(0);
  });
});
