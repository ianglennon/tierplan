// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSStatus } from "@/api/https";
import { isLockProblem } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { hasEnvLocks } from "@/features/https/card-state";
import { HttpsPage } from "@/features/https/https-page";
import { LockedNote } from "@/features/https/locked-note";
import "@/i18n";
import { httpsLockedStatus, httpsStatus } from "@/test-https";
import { TestRouter } from "@/test-router";

// The two lock sentences every locked field shows, and the notice at the top of the page that
// explains them (SET-02, D-03, D-17, UI-SPEC J2/J3/A3).

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
});

const lines = (id: string) =>
  Array.from(document.getElementById(id)?.querySelectorAll("p") ?? []).map((p) => p.textContent);

describe("LockedNote", () => {
  it("says who sets the field and what is kept underneath, with the variable from the props", () => {
    render(<LockedNote id="n" lock={{ var: "TIERPLAN_ACME_PROFILE", stored: "classic" }} />);
    expect(lines("n")).toEqual([
      "Set by the TIERPLAN_ACME_PROFILE environment variable.",
      "Stored value: classic, used if TIERPLAN_ACME_PROFILE is removed.",
    ]);
  });

  it("names whichever variable the API gives (the token may come from a _FILE variable)", () => {
    render(<LockedNote id="n" lock={{ var: "TIERPLAN_SOMETHING_ELSE_FILE", stored: "x" }} />);
    expect(lines("n")[0]).toBe("Set by the TIERPLAN_SOMETHING_ELSE_FILE environment variable.");
    expect(lines("n")[1]).toContain("used if TIERPLAN_SOMETHING_ELSE_FILE is removed.");
  });

  it("reads 'none' when nothing is stored, so line 2 is never blank", () => {
    render(<LockedNote id="a" lock={{ var: "TIERPLAN_ACME_EMAIL" }} />);
    render(<LockedNote id="b" lock={{ var: "TIERPLAN_ACME_EMAIL", stored: "" }} />);
    const none = "Stored value: none, used if TIERPLAN_ACME_EMAIL is removed.";
    expect(lines("a")[1]).toBe(none);
    expect(lines("b")[1]).toBe(none);
  });

  it("draws the variable and a free-text stored value in mono, broken anywhere", () => {
    const long = `${"a".repeat(80)}@example.com`;
    render(<LockedNote id="n" lock={{ var: "TIERPLAN_ACME_EMAIL", stored: long }} mono />);
    const codes = Array.from(document.querySelectorAll("#n code"));
    expect(codes.map((c) => c.textContent)).toEqual([
      "TIERPLAN_ACME_EMAIL",
      long,
      "TIERPLAN_ACME_EMAIL",
    ]);
    for (const c of codes) {
      expect(c.className).toContain("font-mono");
      expect(c.className).toContain("break-all");
    }
  });

  it("draws an enum stored value as plain words, not mono", () => {
    render(<LockedNote id="n" lock={{ var: "TIERPLAN_ACME", stored: "on" }} />);
    expect(
      Array.from(document.querySelectorAll("#n code")).map((c) => c.textContent),
    ).not.toContain("on");
  });

  it("words the terms as accepted and has no stored line", () => {
    render(<LockedNote id="n" lock={{ var: "TIERPLAN_ACME_ACCEPT_TOS" }} variant="accepted" />);
    expect(lines("n")).toEqual(["Accepted by the TIERPLAN_ACME_ACCEPT_TOS environment variable."]);
  });

  it("leaves the stored line out on request and keeps the lock icon only when asked", () => {
    render(<LockedNote id="a" lock={{ var: "TIERPLAN_ACME_EMAIL" }} showStored={false} />);
    expect(lines("a")).toHaveLength(1);
    expect(screen.getByRole("img", { name: "Locked" })).toBeTruthy();
    cleanup();
    render(<LockedNote id="b" lock={{ var: "TIERPLAN_ACME_EMAIL" }} showIcon={false} />);
    expect(screen.queryByRole("img", { name: "Locked" })).toBeNull();
  });

  it("gives an action its own reason sentence, ahead of the stored line", () => {
    render(<LockedNote id="a" lock={{ var: "TIERPLAN_ACME", stored: "on" }} variant="order" />);
    expect(lines("a")).toEqual([
      "Set by the TIERPLAN_ACME environment variable. Set it to on, or remove it, to order certificates.",
      "Stored value: on, used if TIERPLAN_ACME is removed.",
    ]);
    cleanup();
    render(<LockedNote id="b" lock={{ var: "TIERPLAN_ACME", stored: "off" }} variant="stop" />);
    expect(lines("b")[0]).toBe(
      "Set by the TIERPLAN_ACME environment variable. Set it to off, or remove it, to stop.",
    );
  });
});

describe("hasEnvLocks and isLockProblem", () => {
  it("is true for any HTTPS lock and false for none", () => {
    expect(hasEnvLocks(httpsStatus())).toBe(false);
    expect(hasEnvLocks(httpsLockedStatus({}))).toBe(false);
    expect(hasEnvLocks(httpsLockedStatus({ email: { var: "TIERPLAN_ACME_EMAIL" } }))).toBe(true);
    expect(hasEnvLocks({ ...httpsStatus(), acme: undefined })).toBe(false);
  });

  it("recognises a per-field *_locked problem and nothing else", () => {
    expect(isLockProblem({ type: "acme_email_locked" })).toBe(true);
    expect(isLockProblem({ type: "acme_locked" })).toBe(true);
    expect(isLockProblem({ type: "acme_run_active" })).toBe(false);
    expect(isLockProblem({ type: undefined })).toBe(false);
    expect(isLockProblem(new Error("x"))).toBe(false);
    expect(isLockProblem(null)).toBe(false);
  });
});

describe("the env-lock notice on the HTTPS page", () => {
  let answer: HTTPSStatus;
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        if (new URL(req.url).pathname === "/api/https/status") return Response.json(answer);
        return Response.json({}, { status: 404 });
      }),
    );
  });

  function renderPage() {
    render(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <TestRouter>
            <HttpsPage />
          </TestRouter>
        </TooltipProvider>
      </QueryClientProvider>,
    );
  }

  it("shows while a field is locked, above the cards, as a note with no button", async () => {
    answer = httpsLockedStatus({ profile: { var: "TIERPLAN_ACME_PROFILE" } });
    renderPage();
    const notice = await screen.findByText("Some settings are set by environment variables");
    const alert = notice.closest("[role='note']") as HTMLElement;
    expect(alert.textContent).toContain("Locked settings show a lock and the variable");
    expect(alert.textContent).toContain("used again if you remove the variable");
    expect(alert.querySelector("button")).toBeNull();
    const firstCard = screen.getAllByRole("region")[0] as HTMLElement;
    expect(
      alert.compareDocumentPosition(firstCard) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("does not show without a lock", async () => {
    answer = httpsStatus();
    renderPage();
    await screen.findByText("Certificate in use");
    expect(screen.queryByText("Some settings are set by environment variables")).toBeNull();
  });
});
