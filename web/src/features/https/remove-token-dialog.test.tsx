// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { httpsStatusQuery } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { RemoveTokenDialog } from "@/features/https/remove-token-dialog";
import "@/i18n";
import { httpsStatus } from "@/test-https";

// "Remove the Cloudflare token?" (plan 15-10, D-05, D-18, UI-SPEC E9): a confirmation with nothing to
// type, focus on Keep token, frozen while the request is in flight.

let sent: { path: string; method: string; csrf: string | null }[];
let remove: () => Response | Promise<Response>;
let status: () => Response;
let successToast: ReturnType<typeof vi.spyOn>;
let closed: number;

beforeEach(() => {
  sent = [];
  closed = 0;
  status = () => Response.json(httpsStatus());
  remove = () => new Response(null, { status: 204 });
  successToast = vi.spyOn(toast, "success");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/dns/token" && req.method === "DELETE") return remove();
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

function Harness({ leftOverCount }: { leftOverCount: number }) {
  const [open, setOpen] = useState(true);
  // The card reads the status; the dialog's mutation reads it again through this observer.
  useQuery(httpsStatusQuery);
  return (
    <RemoveTokenDialog
      open={open}
      leftOverCount={leftOverCount}
      onOpenChange={(next) => {
        if (!next) closed += 1;
        setOpen(next);
      }}
    />
  );
}

function renderDialog(leftOverCount = 0) {
  render(
    <QueryClientProvider client={queryClient}>
      <Harness leftOverCount={leftOverCount} />
    </QueryClientProvider>,
  );
}

const dialog = () => within(screen.getByRole("alertdialog"));
const confirmButton = () => dialog().getByRole("button", { name: /^(Remove token|Removing)/ });
const removes = () => sent.filter((s) => s.path === "/api/https/acme/dns/token");

function holdRemove() {
  let release: () => void = () => {};
  remove = () =>
    new Promise<Response>((resolve) => {
      release = () => resolve(new Response(null, { status: 204 }));
    });
  return () => release();
}

describe("Remove the Cloudflare token? dialog", () => {
  it("opens with the UI-SPEC copy and focus on 'Keep token'", async () => {
    renderDialog();
    expect(dialog().getByText("Remove the Cloudflare token?")).toBeTruthy();
    expect(
      dialog().getByText(
        "TierPlan deletes the saved token. You'll need to enter a token again to use DNS-01. Certificates already issued are not affected.",
      ),
    ).toBeTruthy();
    const keep = dialog().getByRole("button", { name: "Keep token" });
    await waitFor(() => expect(document.activeElement).toBe(keep));
    expect(confirmButton().className).toContain("bg-destructive-fill");
    expect(confirmButton().className).toContain("text-white");
    expect(removes()).toHaveLength(0);
  });

  it("adds no left-over paragraph when no record is left over", () => {
    renderDialog(0);
    expect(dialog().queryByText(/TierPlan needs the token to delete/)).toBeNull();
  });

  it("adds the left-over paragraph naming 2 TXT records", () => {
    renderDialog(2);
    expect(
      dialog().getByText(
        "TierPlan needs the token to delete the 2 TXT records it left in Cloudflare. Remove it now and they stay there until you delete them yourself.",
      ),
    ).toBeTruthy();
  });

  it("words one left-over record in the singular", () => {
    renderDialog(1);
    expect(
      dialog().getByText(
        "TierPlan needs the token to delete the 1 TXT record it left in Cloudflare. Remove it now and it stays there until you delete it yourself.",
      ),
    ).toBeTruthy();
  });

  it("closes from 'Keep token' and asks the server for nothing", () => {
    renderDialog();
    fireEvent.click(dialog().getByRole("button", { name: "Keep token" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(closed).toBe(1);
    expect(removes()).toHaveLength(0);
  });

  it("closes on Escape while nothing is in flight", () => {
    renderDialog();
    fireEvent.keyDown(screen.getByRole("alertdialog"), { key: "Escape" });
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("is frozen while the request is in flight: 'Removing...' with a spinner, nothing closes it", async () => {
    const release = holdRemove();
    renderDialog();
    fireEvent.click(confirmButton());
    await waitFor(() => expect(removes()).toHaveLength(1));
    expect(removes()[0]?.method).toBe("DELETE");

    await waitFor(() => expect(confirmButton().textContent).toBe("Removing..."));
    expect(confirmButton().getAttribute("aria-disabled")).toBe("true");
    expect(confirmButton().querySelector("[data-slot=spinner], svg")).toBeTruthy();
    expect(dialog().getByRole("button", { name: "Keep token" }).hasAttribute("disabled")).toBe(
      true,
    );

    // Escape, a second press of the confirm button and the cancel button do nothing.
    fireEvent.keyDown(screen.getByRole("alertdialog"), { key: "Escape" });
    fireEvent.click(confirmButton());
    fireEvent.click(dialog().getByRole("button", { name: "Keep token" }));
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(closed).toBe(0);
    expect(removes()).toHaveLength(1);

    await act(async () => release());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("on success closes, reads the status again and toasts 'Token removed.'", async () => {
    renderDialog();
    await waitFor(() => expect(sent.some((s) => s.path === "/api/https/status")).toBe(true));
    const before = sent.filter((s) => s.path === "/api/https/status").length;
    fireEvent.click(confirmButton());
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(closed).toBe(1);
    expect(successToast).toHaveBeenCalledTimes(1);
    const content = successToast.mock.calls[0]?.[0] as { props: { children: string } };
    expect(content.props.children).toBe("Token removed.");
    expect(removes()).toHaveLength(1);
    expect(removes()[0]?.csrf).not.toBeUndefined();
    await waitFor(() =>
      expect(sent.filter((s) => s.path === "/api/https/status").length).toBeGreaterThan(before),
    );
  });

  it.each([
    [409, "acme_dns_token_in_use"],
    [500, "about:blank"],
  ])("a %i keeps the dialog open with the inline alert", async (code, type) => {
    remove = () =>
      Response.json(
        { type, title: "x", status: code, detail: "d" },
        { status: code, headers: { "Content-Type": "application/problem+json" } },
      );
    renderDialog();
    fireEvent.click(confirmButton());
    const alert = await dialog().findByText("Something went wrong. Try again.");
    expect(alert.closest("[role=alert]")).toBeTruthy();
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(closed).toBe(0);
    expect(successToast).not.toHaveBeenCalled();
    expect(confirmButton().textContent).toBe("Remove token");
    expect(confirmButton().getAttribute("aria-disabled")).toBe("false");
  });
});
