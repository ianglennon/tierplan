// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { Grant } from "@/api/grants";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { GrantDialog } from "@/features/access/grant-dialog";
import "@/i18n";

// The grant dialog (plan 03-19, AUTH-06, AUTH-07, D-15 to D-17, D-27, UI-SPEC "GrantDialog").

function envBody(id: string, name: string) {
  return { id, name, kind: "agent", enabled: true, status: { state: "online" } };
}

function grant(over: Partial<Grant> = {}): Grant {
  return {
    id: "g1",
    subjectType: "user",
    subjectId: "u1",
    scope: "env",
    envId: "env-a",
    envName: "Alpha",
    role: "operator",
    extras: ["containers.exec"],
    createdAt: "2026-10-08T10:00:00Z",
    ...over,
  };
}

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let environments: () => Response | Promise<Response>;
let put: (body: Record<string, unknown>) => Response | Promise<Response>;
let preview: (body: Record<string, unknown>) => Response | Promise<Response>;
let candidates: () => Response | Promise<Response>;

function problem(status: number, type: string) {
  return Response.json({ status, type, title: type, detail: type }, { status });
}

beforeEach(() => {
  sent = [];
  setCsrfToken("csrf");
  environments = () =>
    Response.json({ items: [envBody("env-b", "Beta"), envBody("env-a", "Alpha")], total: 2 });
  put = () => Response.json(grant(), { status: 201 });
  preview = () => Response.json({ impact: [] });
  candidates = () =>
    Response.json({ users: [{ id: "u2", username: "bob", displayName: "Bob B" }], teams: [] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { method: req.method, path };
      if (req.method !== "GET") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/environments") return environments();
      if (path === "/api/users/u1/grants" && req.method === "PUT") return put(entry.body ?? {});
      if (path === "/api/users/u1/grants/g1/impact") return preview(entry.body ?? {});
      if (path === "/api/env/env-a/access/candidates") return candidates();
      if (path === "/api/env/env-a/access" && req.method === "PUT") return put(entry.body ?? {});
      if (path === "/api/env/env-a/access/g1/impact") return preview(entry.body ?? {});
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
});

const SUBJECT = { type: "user", id: "u1", name: "alice" } as const;

function Harness({
  existing = null,
  grants = [],
  onSaved = () => {},
}: {
  existing?: Grant | null;
  grants?: Grant[];
  onSaved?: () => void;
}) {
  const [open, setOpen] = useState(true);
  return (
    <QueryClientProvider client={queryClient}>
      <GrantDialog
        subject={SUBJECT}
        open={open}
        onOpenChange={setOpen}
        grant={existing}
        grants={grants}
        onSaved={onSaved}
      />
      <Toaster />
    </QueryClientProvider>
  );
}

async function chooseEnvironment(dialog: HTMLElement, name: string) {
  const trigger = await within(dialog).findByRole("combobox", { name: "Environment" });
  await waitFor(() => expect((trigger as HTMLButtonElement).disabled).toBe(false));
  fireEvent.keyDown(trigger, { key: "Enter" });
  fireEvent.keyDown(await screen.findByRole("option", { name }), { key: "Enter" });
}

function checkbox(dialog: HTMLElement, name: string) {
  return within(dialog).getByRole("checkbox", { name }) as HTMLButtonElement;
}

function radio(dialog: HTMLElement, name: string) {
  return within(dialog).getByRole("radio", { name }) as HTMLButtonElement;
}

describe("the role picker and the extra permissions", () => {
  it("clears and disables the extras on Viewer, ticks and disables them on Admin, and restores the selection on Operator", async () => {
    render(<Harness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });

    fireEvent.click(radio(dialog, "Operator"));
    fireEvent.click(checkbox(dialog, "Container shell"));
    fireEvent.click(checkbox(dialog, "Delete volumes"));
    expect(checkbox(dialog, "Container shell").getAttribute("aria-checked")).toBe("true");
    expect(checkbox(dialog, "Reveal secrets").getAttribute("aria-checked")).toBe("false");

    fireEvent.click(radio(dialog, "Viewer"));
    for (const name of [
      "Container shell",
      "Reveal secrets",
      "Delete volumes",
      "Change janitor policy",
    ]) {
      const box = checkbox(dialog, name);
      expect(box.getAttribute("aria-checked")).toBe("false");
      expect(box.disabled).toBe(true);
    }
    expect(
      within(dialog).getByText("Viewers are read only, so they can't hold extra permissions."),
    ).toBeTruthy();

    fireEvent.click(radio(dialog, "Admin"));
    for (const name of [
      "Container shell",
      "Reveal secrets",
      "Delete volumes",
      "Change janitor policy",
    ]) {
      const box = checkbox(dialog, name);
      expect(box.getAttribute("aria-checked")).toBe("true");
      expect(box.disabled).toBe(true);
    }
    expect(within(dialog).getByText("Included with Admin.")).toBeTruthy();

    // Back to Operator: what was ticked before is ticked again, and editable.
    fireEvent.click(radio(dialog, "Operator"));
    expect(checkbox(dialog, "Container shell").getAttribute("aria-checked")).toBe("true");
    expect(checkbox(dialog, "Delete volumes").getAttribute("aria-checked")).toBe("true");
    expect(checkbox(dialog, "Reveal secrets").getAttribute("aria-checked")).toBe("false");
    expect(checkbox(dialog, "Container shell").disabled).toBe(false);
  });

  it("describes each disabled extra with the reason and each radio with its role description", async () => {
    render(<Harness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    fireEvent.click(radio(dialog, "Viewer"));
    const shell = checkbox(dialog, "Container shell");
    const described = (shell.getAttribute("aria-describedby") ?? "")
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent);
    expect(described).toContain("Open a terminal inside containers.");
    expect(described).toContain("Viewers are read only, so they can't hold extra permissions.");

    const admin = radio(dialog, "Admin");
    const text = document.getElementById(admin.getAttribute("aria-describedby") ?? "")?.textContent;
    expect(text).toContain("Includes every extra permission.");
  });

  it("lets role descriptions wrap inside options at least 56px high and never truncates them", async () => {
    render(<Harness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    for (const name of ["Admin", "Operator", "Viewer"]) {
      const option = radio(dialog, name).closest("label") as HTMLElement;
      expect(option.className).toContain("min-h-14");
      const desc = document.getElementById(
        radio(dialog, name).getAttribute("aria-describedby") ?? "",
      ) as HTMLElement;
      expect(desc.className).not.toMatch(/truncate|line-clamp|whitespace-nowrap/);
    }
  });
});

describe("adding access", () => {
  it("shows one skeleton row and a disabled Save while the environments load", async () => {
    let release: (() => void) | undefined;
    environments = () =>
      new Promise((resolve) => {
        release = () => resolve(Response.json({ items: [envBody("env-a", "Alpha")], total: 1 }));
      });
    render(<Harness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    expect(within(dialog).getByText("Loading environments")).toBeTruthy();
    expect(within(dialog).queryByRole("combobox", { name: "Environment" })).toBeNull();
    expect(
      (within(dialog).getByRole("button", { name: "Add access" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    release?.();
    expect(await within(dialog).findByRole("combobox", { name: "Environment" })).toBeTruthy();
    // No environment is chosen yet, so Save stays off until one is.
    expect(
      (within(dialog).getByRole("button", { name: "Add access" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("offers All environments first for the subject and sends scope all without an environment", async () => {
    const onSaved = vi.fn();
    render(<Harness onSaved={onSaved} />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    await chooseEnvironment(dialog, "All environments");
    expect(
      within(dialog).getByText("Applies to every environment, including ones you add later."),
    ).toBeTruthy();
    fireEvent.click(radio(dialog, "Operator"));
    fireEvent.click(checkbox(dialog, "Reveal secrets"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Add access" }));

    expect(await screen.findByText("Access saved.")).toBeTruthy();
    const body = sent.find((s) => s.method === "PUT")?.body;
    expect(body).toEqual({ scope: "all", role: "operator", extras: ["secrets.reveal"] });
    expect(onSaved).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("says saving replaces an existing grant on the chosen environment and the button reads Save access", async () => {
    render(<Harness grants={[grant({ role: "viewer", extras: [] })]} />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    expect(within(dialog).queryByText(/already has/)).toBeNull();
    await chooseEnvironment(dialog, "Alpha");
    expect(
      within(dialog).getByText("alice already has Viewer here. Saving replaces it."),
    ).toBeTruthy();
    fireEvent.click(radio(dialog, "Operator"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save access" }));
    await screen.findByText("Access saved.");
    // The save carries the state the dialog saw, so a change made meanwhile is caught.
    expect(sent.find((s) => s.method === "PUT")?.body).toEqual({
      scope: "env",
      envId: "env-a",
      role: "operator",
      extras: [],
      expected: { role: "viewer", extras: [] },
    });
    // Another environment has no such grant, and the note goes.
  });
});

describe("a refused or failed save", () => {
  async function submit(fill?: (dialog: HTMLElement) => void) {
    render(<Harness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    await chooseEnvironment(dialog, "Alpha");
    fill?.(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Add access" }));
    return dialog;
  }

  it.each([
    ["Viewers can't have extra permissions.", () => problem(422, "viewer_cannot_have_extras")],
    ["You can't give that access.", () => problem(403, "forbidden")],
    ["Couldn't save access. Try again.", () => problem(500, "internal")],
    [
      "This changed while you were looking at it. Reload the page and try again.",
      () => problem(409, "stale_change"),
    ],
  ])("keeps the dialog open with an inline alert: %s", async (message, answer) => {
    put = answer;
    const dialog = await submit();
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe(message);
    // The dialog is still there, Save is available again, and no toast was raised.
    expect(screen.getByRole("dialog", { name: "Add access" })).toBeTruthy();
    await waitFor(() =>
      expect(
        (within(dialog).getByRole("button", { name: "Add access" }) as HTMLButtonElement).disabled,
      ).toBe(false),
    );
    expect(screen.queryByText("Access saved.")).toBeNull();
  });
});

describe("editing access", () => {
  it("shows who and where as read-only text, with no environment field, and starts from the grant", async () => {
    render(<Harness existing={grant()} grants={[grant()]} />);
    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    expect(within(dialog).queryByRole("combobox")).toBeNull();
    expect(within(dialog).getByText("alice")).toBeTruthy();
    expect(within(dialog).getByText("Alpha")).toBeTruthy();
    expect(radio(dialog, "Operator").getAttribute("aria-checked")).toBe("true");
    expect(checkbox(dialog, "Container shell").getAttribute("aria-checked")).toBe("true");
    expect(within(dialog).getByRole("button", { name: "Save access" })).toBeTruthy();
    // It is an edit, so the replace note does not apply.
    expect(within(dialog).queryByText(/already has/)).toBeNull();
  });

  it("shows what changes when the role is lowered and keeps Save off until the preview has loaded", async () => {
    let release: (() => void) | undefined;
    preview = () =>
      new Promise((resolve) => {
        release = () =>
          resolve(
            Response.json({
              impact: [
                {
                  userId: "u1",
                  username: "alice",
                  environmentId: "env-a",
                  environmentName: "Alpha",
                  before: "operator",
                  after: "viewer",
                },
              ],
            }),
          );
      });
    render(<Harness existing={grant()} grants={[grant()]} />);
    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    // Nothing is lowered yet, so there is nothing to preview.
    expect(within(dialog).queryByText("What changes")).toBeNull();
    expect(sent.some((s) => s.path.endsWith("/impact"))).toBe(false);

    fireEvent.click(radio(dialog, "Viewer"));
    expect(await within(dialog).findByText("What changes")).toBeTruthy();
    const save = within(dialog).getByRole("button", { name: "Save access" }) as HTMLButtonElement;
    expect(save.disabled || save.getAttribute("aria-disabled") === "true").toBe(true);
    fireEvent.click(save);
    expect(sent.some((s) => s.method === "PUT")).toBe(false);

    release?.();
    expect(await within(dialog).findByText("Operator → Viewer")).toBeTruthy();
    await waitFor(() => {
      const s = within(dialog).getByRole("button", { name: "Save access" }) as HTMLButtonElement;
      expect(s.disabled || s.getAttribute("aria-disabled") === "true").toBe(false);
    });
    expect(sent.find((s) => s.path.endsWith("/impact"))?.body).toEqual({
      op: "update",
      role: "viewer",
      extras: [],
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save access" }));
    await screen.findByText("Access saved.");
    expect(sent.find((s) => s.method === "PUT")?.body).toEqual({
      scope: "env",
      envId: "env-a",
      role: "viewer",
      extras: [],
      expected: { role: "operator", extras: ["containers.exec"] },
    });
  });

  it("previews when only an extra is removed, and not when access is raised", async () => {
    render(<Harness existing={grant()} grants={[grant()]} />);
    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    fireEvent.click(checkbox(dialog, "Container shell"));
    expect(await within(dialog).findByText("No one loses access.")).toBeTruthy();
    expect(sent.find((s) => s.path.endsWith("/impact"))?.body).toEqual({
      op: "update",
      role: "operator",
      extras: [],
    });
    // Ticking it again is back to what was saved: nothing is lowered.
    fireEvent.click(checkbox(dialog, "Container shell"));
    await waitFor(() => expect(within(dialog).queryByText("What changes")).toBeNull());
    fireEvent.click(radio(dialog, "Admin"));
    expect(within(dialog).queryByText("What changes")).toBeNull();
  });

  it("fails closed when the preview fails: Save stays off and Retry asks again", async () => {
    preview = () => problem(500, "internal");
    render(<Harness existing={grant()} grants={[grant()]} />);
    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    fireEvent.click(radio(dialog, "Viewer"));
    expect(
      await within(dialog).findByText("Couldn't work out what changes. Try again."),
    ).toBeTruthy();
    const save = within(dialog).getByRole("button", { name: "Save access" }) as HTMLButtonElement;
    expect(save.disabled || save.getAttribute("aria-disabled") === "true").toBe(true);
    preview = () => Response.json({ impact: [] });
    fireEvent.click(within(dialog).getByRole("button", { name: "Retry" }));
    expect(await within(dialog).findByText("No one loses access.")).toBeTruthy();
  });
});

// The dialog opened from an environment's Access tab (plan 03-20, D-12, UI-SPEC
// "Grants" and the delegating env Admin partial).
function EnvHarness({
  existing = null,
  onSaved = () => {},
}: {
  existing?: Grant | null;
  onSaved?: () => void;
}) {
  const [open, setOpen] = useState(true);
  return (
    <QueryClientProvider client={queryClient}>
      <GrantDialog
        mode="env"
        environment={{ id: "env-a", name: "Alpha" }}
        subject={existing ? SUBJECT : undefined}
        open={open}
        onOpenChange={setOpen}
        grant={existing}
        onSaved={onSaved}
      />
      <Toaster />
    </QueryClientProvider>
  );
}

describe("the dialog on an environment's Access tab", () => {
  it("has no environment field and no All environments option: the environment is a line of text", async () => {
    render(<EnvHarness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    expect(within(dialog).getByText("On Alpha")).toBeTruthy();
    expect(within(dialog).queryByRole("combobox", { name: "Environment" })).toBeNull();
    expect(within(dialog).queryByText("All environments")).toBeNull();
    expect(await within(dialog).findByRole("combobox", { name: "Who" })).toBeTruthy();
  });

  it("edits through the environment route with Who and the environment as read-only text and no Who field", async () => {
    render(<EnvHarness existing={grant()} />);
    const dialog = await screen.findByRole("dialog", { name: "Edit access" });
    expect(within(dialog).queryByRole("combobox", { name: "Who" })).toBeNull();
    expect(within(dialog).getByText("alice")).toBeTruthy();
    expect(within(dialog).getByText("Alpha")).toBeTruthy();
    fireEvent.click(radio(dialog, "Viewer"));
    expect(await within(dialog).findByText("No one loses access.")).toBeTruthy();
    expect(sent.find((s) => s.path.endsWith("/impact"))?.path).toBe(
      "/api/env/env-a/access/g1/impact",
    );
    fireEvent.click(within(dialog).getByRole("button", { name: "Save access" }));
    await screen.findByText("Access saved.");
    const saved = sent.find((s) => s.method === "PUT");
    expect(saved?.path).toBe("/api/env/env-a/access");
    expect(saved?.body).toEqual({
      subjectType: "user",
      subjectId: "u1",
      role: "viewer",
      extras: [],
      expected: { role: "operator", extras: ["containers.exec"] },
    });
  });

  it("keeps Add access off until someone is chosen in the Who list", async () => {
    render(<EnvHarness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    const add = within(dialog).getByRole("button", { name: "Add access" }) as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    fireEvent.click(await within(dialog).findByRole("combobox", { name: "Who" }));
    fireEvent.click(await screen.findByRole("option", { name: /bob/ }));
    await waitFor(() => expect(add.disabled).toBe(false));
    expect((await within(dialog).findByRole("combobox", { name: "Who" })).textContent).toContain(
      "bob",
    );
  });

  it("shows Everyone already has access here. when the list is empty", async () => {
    candidates = () => Response.json({ users: [], teams: [] });
    render(<EnvHarness />);
    const dialog = await screen.findByRole("dialog", { name: "Add access" });
    expect(await within(dialog).findByText("Everyone already has access here.")).toBeTruthy();
  });
});
