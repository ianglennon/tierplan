// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { HstsCard } from "@/features/https/hsts-card";
import "@/i18n";
import { hstsBody, hstsSending, httpsHSTSStatus } from "@/test-https";
import { makeMe } from "@/test-me";

interface Sent {
  path: string;
  method: string;
  body?: unknown;
}

let sent: Sent[];
let status: () => HTTPSStatus;
let me: () => Response;
let putAnswer: () => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const viewer = () => Response.json(makeMe({ allEnvironments: "viewer" }));

beforeEach(() => {
  sent = [];
  status = () => httpsHSTSStatus();
  me = instanceAdmin;
  putAnswer = () => Response.json(hstsSending(1));
  successToast = vi.spyOn(toast, "success");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "PUT") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return Response.json(status());
      if (path === "/api/https/hsts" && req.method === "PUT") return putAnswer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
});

function renderCard() {
  render(
    <QueryClientProvider client={queryClient}>
      <HstsCard />
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "HSTS" }));
const switchControl = () => card().getByRole("switch", { name: "Send HSTS" });
const saveButton = () => card().queryByRole("button", { name: /^(Save HSTS setting|Saving)/ });
const puts = () => sent.filter((s) => s.method === "PUT");
const toasts = () =>
  successToast.mock.calls.map(
    (c: unknown[]) => (c[0] as { props: { children: string } }).props.children,
  );

// The card, once the status and who is signed in have both arrived.
async function ready() {
  renderCard();
  await card().findByText("Status");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

describe("HSTS card: the setting (TLS-06, D-10, J4)", () => {
  it("is the third card's anchor and says HSTS is off by default and never adds includeSubDomains or preload", async () => {
    await ready();
    expect(document.getElementById("hsts")?.tagName.toLowerCase()).toBe("section");
    expect(card().getByText(/HSTS tells browsers to use only HTTPS for this address/)).toBeTruthy();
    expect(
      card().getByText(
        "Off by default. Once a browser has seen HSTS it refuses plain HTTP for this address until the lifetime ends. TierPlan never adds includeSubDomains or preload.",
      ),
    ).toBeTruthy();
    expect(switchControl().getAttribute("aria-checked")).toBe("false");
  });

  it("switching on only changes a draft: Save sends {enabled: true}, toasts, and the refetched status reads Sending at max-age=300", async () => {
    await ready();
    fireEvent.click(switchControl());
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    // The lifetime promise appears under the helper while the draft is on and the saved choice is off.
    expect(
      card().getByText(
        "The lifetime starts at 5 minutes and only grows while TierPlan serves a trusted certificate without a problem.",
      ),
    ).toBeTruthy();
    expect(puts()).toEqual([]);

    status = () => httpsHSTSStatus(hstsSending(1));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(toasts()).toEqual(["HSTS turned on."]));
    expect(puts()).toEqual([{ path: "/api/https/hsts", method: "PUT", body: { enabled: true } }]);

    expect(await card().findByText("Sending")).toBeTruthy();
    expect(card().getByText("max-age=300")).toBeTruthy();
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("true");
  });

  it("switching off sends {enabled: false}, toasts 'HSTS turned off.' and says browsers are told to forget it while the draft is off", async () => {
    status = () => httpsHSTSStatus(hstsSending(2));
    await ready();
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    fireEvent.click(switchControl());
    expect(
      card().getByText(
        "Browsers that visit while a trusted certificate is served are told to forget HSTS (max-age=0).",
      ),
    ).toBeTruthy();

    status = () => httpsHSTSStatus(hstsBody({ state: "off_clearing", clearing: true }));
    putAnswer = () => Response.json(hstsBody({ state: "off_clearing", clearing: true }));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(toasts()).toEqual(["HSTS turned off."]));
    expect(puts()[0]?.body).toEqual({ enabled: false });
    expect(
      await card().findByText(/TierPlan tells browsers to forget HSTS \(max-age=0\)\. This also/),
    ).toBeTruthy();
  });

  it("Save is aria-disabled and does nothing while the draft equals the stored value", async () => {
    await ready();
    const save = saveButton() as HTMLElement;
    expect(save.getAttribute("aria-disabled")).toBe("true");
    expect(save.hasAttribute("disabled")).toBe(false);
    fireEvent.click(save);
    // Switching on and back off leaves nothing to save.
    fireEvent.click(switchControl());
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("false");
    fireEvent.click(switchControl());
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(saveButton() as HTMLElement);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(puts()).toEqual([]);
  });

  it("Save HSTS setting is an outline button, so the card adds no filled button", async () => {
    await ready();
    const save = saveButton() as HTMLElement;
    expect(save.getAttribute("data-variant")).toBe("outline");
  });

  it("while the status is pending shows skeleton rows and a disabled Switch", () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    renderCard();
    expect(screen.getByTestId("hsts-loading")).toBeTruthy();
    const control = card().getByRole("switch", { name: "Send HSTS" });
    expect(control.hasAttribute("disabled")).toBe(true);
    expect(saveButton()).toBeNull();
  });

  it("a failed save shows 'Something went wrong. Try again.' in the card and keeps the draft", async () => {
    putAnswer = () =>
      Response.json(
        { type: "about:blank", title: "x", status: 500, detail: "d" },
        { status: 500, headers: { "Content-Type": "application/problem+json" } },
      );
    await ready();
    fireEvent.click(switchControl());
    fireEvent.click(saveButton() as HTMLElement);
    expect(await card().findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(toasts()).toEqual([]);
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("false");
  });
});

describe("HSTS card: the Status row (D-12, D-23)", () => {
  it("off with nothing sent: Off, 'Nothing is sent.', no Lifetime row, no steps", async () => {
    await ready();
    expect(card().getByText("Off")).toBeTruthy();
    expect(card().getByText("Nothing is sent.")).toBeTruthy();
    expect(card().queryByText("Lifetime")).toBeNull();
  });

  it("off, clearing: Off with max-age=0 and the earlier one-year header note", async () => {
    status = () => httpsHSTSStatus(hstsBody({ state: "off_clearing", clearing: true }));
    await ready();
    expect(card().getByText("Off")).toBeTruthy();
    expect(
      card().getByText(
        "TierPlan tells browsers to forget HSTS (max-age=0). This also clears the one-year setting that earlier versions sent for certificate files.",
      ),
    ).toBeTruthy();
  });

  it("sending: the value, the max-age and the step in words", async () => {
    status = () => httpsHSTSStatus(hstsSending(2));
    await ready();
    expect(card().getByText("Sending")).toBeTruthy();
    expect(card().getByText("The browser keeps the policy for the lifetime shown.")).toBeTruthy();
    expect(card().getByText("max-age=604800")).toBeTruthy();
    expect(card().getByText("step 2 of 4, 1 week")).toBeTruthy();
  });
});

describe("HSTS card: without settings.write", () => {
  it("disables the Switch, draws no Save and shows the read-only line, and still shows the status", async () => {
    me = viewer;
    status = () => httpsHSTSStatus(hstsSending(1));
    renderCard();
    await card().findByText("Sending");
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    expect(switchControl().hasAttribute("disabled")).toBe(true);
    expect(saveButton()).toBeNull();
    expect(card().getByText("You can view these settings but not change them.")).toBeTruthy();
  });
});
