// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { httpsStatusQuery } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { StopDialog } from "@/features/https/stop-dialog";
import "@/i18n";
import { httpsLetsEncryptStatus, httpsStatus } from "@/test-https";

let sent: { path: string; method: string; csrf: string | null }[];
let stop: () => Response | Promise<Response>;
let status: () => Response;
let successToast: ReturnType<typeof vi.spyOn>;
let closed: number;

beforeEach(() => {
  sent = [];
  closed = 0;
  status = () => Response.json(httpsStatus());
  stop = () => new Response(null, { status: 204 });
  successToast = vi.spyOn(toast, "success");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/stop" && req.method === "POST") return stop();
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

function Harness() {
  const [open, setOpen] = useState(true);
  // The card reads the status; the dialog's mutation reads it again through this observer.
  useQuery(httpsStatusQuery);
  return (
    <StopDialog
      open={open}
      onOpenChange={(next) => {
        if (!next) closed += 1;
        setOpen(next);
      }}
    />
  );
}

function renderDialog() {
  render(
    <QueryClientProvider client={queryClient}>
      <Harness />
    </QueryClientProvider>,
  );
}

const dialog = () => within(screen.getByRole("alertdialog"));
const confirmButton = () => dialog().getByRole("button", { name: /^(Stop using|Stopping)/ });
const stops = () => sent.filter((s) => s.path === "/api/https/acme/stop");

// A stop request that stays open until the test lets it answer.
function holdStop() {
  let release: () => void = () => {};
  stop = () =>
    new Promise<Response>((resolve) => {
      release = () => resolve(new Response(null, { status: 204 }));
    });
  return () => release();
}

describe("Stop using Let's Encrypt dialog (D-14)", () => {
  it("opens with the UI-SPEC copy and focus on 'Keep Let's Encrypt'", async () => {
    renderDialog();
    expect(dialog().getByText("Stop using Let's Encrypt?")).toBeTruthy();
    expect(
      dialog().getByText(/TierPlan goes back to its own self-signed certificate right away/),
    ).toBeTruthy();
    expect(dialog().getByText(/Your Let's Encrypt account is kept/)).toBeTruthy();
    const keep = dialog().getByRole("button", { name: "Keep Let's Encrypt" });
    await waitFor(() => expect(document.activeElement).toBe(keep));
    expect(confirmButton().className).toContain("bg-destructive-fill");
    expect(confirmButton().className).toContain("text-white");
    expect(stops()).toHaveLength(0);
  });

  it("closes from 'Keep Let's Encrypt' and asks the server for nothing", () => {
    renderDialog();
    fireEvent.click(dialog().getByRole("button", { name: "Keep Let's Encrypt" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(closed).toBe(1);
    expect(stops()).toHaveLength(0);
  });

  it("closes on Escape while nothing is in flight", () => {
    renderDialog();
    fireEvent.keyDown(screen.getByRole("alertdialog"), { key: "Escape" });
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("is frozen while the request is in flight: 'Stopping...' with a spinner, nothing closes it", async () => {
    const release = holdStop();
    renderDialog();
    fireEvent.click(confirmButton());
    await waitFor(() => expect(stops()).toHaveLength(1));
    expect(stops()[0]?.method).toBe("POST");

    await waitFor(() => expect(confirmButton().textContent).toBe("Stopping..."));
    expect(confirmButton().getAttribute("aria-disabled")).toBe("true");
    expect(confirmButton().querySelector("[data-slot=spinner], svg")).toBeTruthy();
    expect(
      dialog().getByRole("button", { name: "Keep Let's Encrypt" }).hasAttribute("disabled"),
    ).toBe(true);

    // Escape, a second press of the confirm button and the cancel button do nothing.
    fireEvent.keyDown(screen.getByRole("alertdialog"), { key: "Escape" });
    fireEvent.click(confirmButton());
    fireEvent.click(dialog().getByRole("button", { name: "Keep Let's Encrypt" }));
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(closed).toBe(0);
    expect(stops()).toHaveLength(1);

    status = () => Response.json(httpsStatus());
    await act(async () => release());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("on success closes, reads the status again and toasts", async () => {
    renderDialog();
    status = () => Response.json(httpsLetsEncryptStatus());
    fireEvent.click(confirmButton());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(closed).toBe(1);
    expect(successToast).toHaveBeenCalledTimes(1);
    const content = successToast.mock.calls[0]?.[0] as { props: { children: string } };
    expect(content.props.children).toBe("Back on the self-signed certificate.");
    expect(stops()).toHaveLength(1);
  });

  it("on a failure stays open with the inline alert, and can be tried again", async () => {
    stop = () =>
      Response.json(
        { type: "about:blank", title: "x", status: 500, detail: "d" },
        { status: 500, headers: { "Content-Type": "application/problem+json" } },
      );
    renderDialog();
    fireEvent.click(confirmButton());
    const alert = await dialog().findByText("Something went wrong. Try again.");
    expect(alert.closest("[role=alert]")).toBeTruthy();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(closed).toBe(0);
    expect(successToast).not.toHaveBeenCalled();
    expect(confirmButton().textContent).toBe("Stop using Let's Encrypt");
    expect(confirmButton().getAttribute("aria-disabled")).toBe("false");

    // The server recovers; pressing again goes through and the dialog closes.
    stop = () => new Response(null, { status: 204 });
    fireEvent.click(confirmButton());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(stops()).toHaveLength(2);
  });

  it("a 409 (the page was out of date) also stays open and reads the status again", async () => {
    stop = () =>
      Response.json(
        { type: "acme_not_active", title: "x", status: 409, detail: "d" },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    renderDialog();
    await waitFor(() => expect(sent.some((s) => s.path === "/api/https/status")).toBe(true));
    const before = sent.filter((s) => s.path === "/api/https/status").length;
    fireEvent.click(confirmButton());
    await dialog().findByText("Something went wrong. Try again.");
    await waitFor(() =>
      expect(sent.filter((s) => s.path === "/api/https/status").length).toBeGreaterThan(before),
    );
  });
});
