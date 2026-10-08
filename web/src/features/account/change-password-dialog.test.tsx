// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { ChangePasswordDialog } from "@/features/account/change-password-dialog";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";
import { makeMe } from "@/test-me";

const ME = makeMe({
  instanceAdmin: true,
  csrfToken: "csrf-old",
  permissions: { global: [], environments: {} },
});

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

let sent: Sent[] = [];
let answer: () => Response | Promise<Response>;

beforeEach(() => {
  sent = [];
  setCsrfToken("csrf-old");
  queryClient.setQueryData(meQueryOptions.queryKey, ME);
  answer = () => Response.json({ csrfToken: "csrf-new" });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method, csrf: req.headers.get("X-CSRF-Token") };
      if (req.method === "POST") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/me") return Response.json({ ...ME, csrfToken: "csrf-new" });
      if (path === "/api/me/password") return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

function renderDialog(onOpenChange = vi.fn()) {
  render(
    <QueryClientProvider client={queryClient}>
      <ChangePasswordDialog open onOpenChange={onOpenChange} />
      <Toaster />
    </QueryClientProvider>,
  );
  return onOpenChange;
}

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement;

function fill(current: string, next: string, confirm = next) {
  fireEvent.change(field("Current password"), { target: { value: current } });
  fireEvent.change(field("New password"), { target: { value: next } });
  fireEvent.change(field("Confirm new password"), { target: { value: confirm } });
}

const submit = () => fireEvent.click(screen.getByRole("button", { name: "Change password" }));
const posts = () => sent.filter((s) => s.path === "/api/me/password");

const GOOD = "another long passphrase 42";

describe("ChangePasswordDialog", () => {
  it("shows the three fields with the right autocomplete values and the side-effect helper", () => {
    renderDialog();
    expect(screen.getByRole("dialog", { name: "Change password" })).toBeTruthy();
    expect(field("Current password").autocomplete).toBe("current-password");
    expect(field("New password").autocomplete).toBe("new-password");
    expect(field("Confirm new password").autocomplete).toBe("new-password");
    expect(screen.getByText("Other devices will be signed out.")).toBeTruthy();
  });

  it("toggles each field between hidden and shown", () => {
    renderDialog();
    const toggles = screen.getAllByRole("button", { name: "Show password" });
    expect(toggles).toHaveLength(3);
    fireEvent.click(toggles[1] as HTMLElement);
    expect(field("New password").type).toBe("text");
    expect(field("Current password").type).toBe("password");
  });

  it("sends the passwords, swaps the CSRF token, closes and toasts on success", async () => {
    const onOpenChange = renderDialog();
    fill("old password here", GOOD);
    submit();

    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.body).toEqual({ currentPassword: "old password here", newPassword: GOOD });
    expect(posts()[0]?.csrf).toBe("csrf-old");
    // The cached /api/me answer carries the new session's token straight away.
    expect(queryClient.getQueryData<typeof ME>(meQueryOptions.queryKey)?.csrfToken).toBe(
      "csrf-new",
    );
    expect(
      await screen.findByText("Password changed. Your other sessions were signed out."),
    ).toBeTruthy();
  });

  it("shows the wrong current password inline on that field", async () => {
    answer = () =>
      Response.json(
        {
          status: 422,
          type: "validation_failed",
          errors: [{ location: "body.currentPassword", message: "current_password_incorrect" }],
        },
        { status: 422 },
      );
    const onOpenChange = renderDialog();
    fill("not the password", GOOD);
    submit();

    const message = await screen.findByText("Your current password is incorrect.");
    const input = field("Current password");
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(input.getAttribute("aria-describedby")).toContain(message.id);
    await waitFor(() => expect(document.activeElement).toBe(input));
    expect(onOpenChange).not.toHaveBeenCalled();
    // The typed values stay, so the person only fixes the one field.
    expect(field("New password").value).toBe(GOOD);
  });

  it("maps the server's policy codes to the setup copy", async () => {
    answer = () =>
      Response.json(
        {
          status: 422,
          type: "validation_failed",
          errors: [{ location: "body.newPassword", message: "password_common" }],
        },
        { status: 422 },
      );
    renderDialog();
    fill("old password here", "password1234");
    submit();
    expect(
      await screen.findByText("That password is too common. Choose something harder to guess."),
    ).toBeTruthy();
    expect(field("New password").getAttribute("aria-invalid")).toBe("true");
  });

  it("checks length, a missing current password and a mismatch before sending anything", async () => {
    renderDialog();
    fill("", "short");
    submit();
    expect(await screen.findByText("Enter your current password.")).toBeTruthy();
    expect(screen.getByText("Use at least 12 characters.")).toBeTruthy();

    fill("old password here", GOOD, `${GOOD}x`);
    submit();
    expect(await screen.findByText("The passwords don't match.")).toBeTruthy();
    expect(field("Confirm new password").getAttribute("aria-invalid")).toBe("true");
    expect(posts()).toHaveLength(0);
  });

  it("shows a role=alert form error when the server fails", async () => {
    answer = () => Response.json({ status: 500 }, { status: 500 });
    const onOpenChange = renderDialog();
    fill("old password here", GOOD);
    submit();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Something went wrong. Try again.");
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("says to wait when the server throttles the attempts", async () => {
    answer = () =>
      Response.json({ status: 429, type: "rate_limited", detail: "x" }, { status: 429 });
    const onOpenChange = renderDialog();
    fill("old password here", GOOD);
    submit();
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Too many attempts. Wait a minute and try again.",
    );
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("shows the unreachable message when the request itself fails", async () => {
    answer = () => {
      throw new TypeError("network");
    };
    renderDialog();
    fill("old password here", GOOD);
    submit();
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Can't reach Bayplan. Check your connection and try again.",
    );
  });

  it("disables the form and shows the spinner while pending, and sends only once", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    const onOpenChange = renderDialog();
    fill("old password here", GOOD);
    submit();

    const pendingButton = await screen.findByRole("button", { name: "Changing password…" });
    // A fieldset disables its controls without an attribute on each one.
    expect(pendingButton.matches(":disabled")).toBe(true);
    expect(field("Current password").matches(":disabled")).toBe(true);
    expect(field("New password").matches(":disabled")).toBe(true);
    fireEvent.click(pendingButton);
    fireEvent.submit(pendingButton.closest("form") as HTMLFormElement);
    // Escape does not close a dialog whose request is in flight.
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onOpenChange).not.toHaveBeenCalled();

    await act(async () => release(Response.json({ csrfToken: "csrf-new" })));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(posts()).toHaveLength(1);
  });
});
