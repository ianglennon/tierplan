// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ACMEAccount, HTTPSACME, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { AcmeCard } from "@/features/https/acme-card";
import "@/i18n";
import { acmeDirectory, httpsACME, httpsLockedStatus, httpsStatus } from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

interface Sent {
  path: string;
  method: string;
  body?: Record<string, unknown>;
}

const REGISTERED_AT = "2026-11-10T08:00:00Z";
const account: ACMEAccount = {
  contact: "ops@example.com",
  registeredAt: REGISTERED_AT,
  termsOfServiceUrl: "",
};
const run = {
  challenge: "http-01",
  jobId: "j1",
  kind: "issue",
  outcome: "running",
  startedAt: REGISTERED_AT,
  trigger: "admin",
} as const;

let sent: Sent[];
let status: () => Response | Promise<Response>;
let me: () => Response;
let directory: () => Response | Promise<Response>;
let put: (body: Record<string, unknown>) => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const viewer = () => Response.json(makeMe({ allEnvironments: "viewer" }));

function withACME(over: Partial<HTTPSACME>, top: Partial<HTTPSStatus> = {}): HTTPSStatus {
  return httpsStatus({ acme: httpsACME({ account, ...over }), ...top });
}

function profiles(...names: string[]) {
  return names.map((name) => ({ name, description: `About ${name}` }));
}

beforeEach(() => {
  sent = [];
  status = () => Response.json(withACME({}));
  me = instanceAdmin;
  directory = () =>
    Response.json(acmeDirectory({ profiles: profiles("classic", "shortlived", "tlsserver") }));
  put = (body) => Response.json({ profile: body.profile });
  successToast = vi.spyOn(toast, "success");
  // A card with a run in progress follows the run's stream; this one never says anything.
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "PUT") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") return directory();
      if (path === "/api/https/acme/profile" && req.method === "PUT") return put(entry.body ?? {});
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
      <TooltipProvider>
        <TestRouter>
          <AcmeCard />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Let's Encrypt" }));

// The card, once the status and who is signed in have both arrived.
async function ready() {
  renderCard();
  await card().findByText("Names on the certificate");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

// The card with Advanced opened, and the profile select in it.
async function opened() {
  await ready();
  fireEvent.click(await card().findByRole("button", { name: "Advanced" }));
  return await card().findByRole("combobox", { name: "Certificate profile" });
}

const select = () => card().getByRole("combobox", { name: "Certificate profile" });
const saveButton = () => card().queryByRole("button", { name: /Save profile|Saving/ });
const puts = () => sent.filter((s) => s.method === "PUT");

async function choose(name: string | RegExp) {
  fireEvent.keyDown(select(), { key: "Enter" });
  fireEvent.keyDown(await screen.findByRole("option", { name }), { key: "Enter" });
  // The menu closes and the card is readable again.
  await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
}

describe("ProfileSection", () => {
  it("starts closed, with the Advanced trigger the only sign of it", async () => {
    await ready();
    const trigger = await card().findByRole("button", { name: "Advanced" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(card().queryByText("Certificate profile")).toBeNull();
    fireEvent.click(trigger);
    expect(await card().findByText("Certificate profile")).toBeTruthy();
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
  });

  it("populated: lists CA default first, then each profile the directory offers", async () => {
    const trigger = await opened();
    fireEvent.keyDown(trigger, { key: "Enter" });
    await screen.findAllByRole("option");
    const names = screen.getAllByRole("option").map((o) => o.textContent);
    expect(names).toEqual([
      "CA default",
      "classicAbout classic",
      "shortlivedAbout shortlived",
      "tlsserverAbout tlsserver",
    ]);
    expect(trigger.textContent).toContain("CA default");
    expect(screen.getByText(/The default is right for most installs/)).toBeTruthy();
  });

  it("empty: a directory with no profiles replaces the select with the line", async () => {
    directory = () => Response.json(acmeDirectory({ profiles: [] }));
    await ready();
    fireEvent.click(await card().findByRole("button", { name: "Advanced" }));
    await card().findByText("This directory offers no profiles. The CA default is used.");
    expect(card().queryByRole("combobox", { name: "Certificate profile" })).toBeNull();
  });

  it("loading: the select is disabled while the list is on its way", async () => {
    directory = () => new Promise<Response>(() => {});
    await ready();
    fireEvent.click(await card().findByRole("button", { name: "Advanced" }));
    const el = (await card().findByRole("combobox", {
      name: "Certificate profile",
    })) as HTMLElement;
    expect(el.matches(":disabled")).toBe(true);
    expect(saveButton()?.matches(":disabled")).toBe(true);
  });

  it("error: keeps the saved profile, says the list couldn't be read and leaves Save available", async () => {
    status = () => Response.json(withACME({ profile: "classic" }));
    directory = () => Response.json({ title: "x", status: 502 }, { status: 502 });
    await opened();
    await card().findByText(/list of profiles couldn't be read/);
    expect(select().textContent).toContain("classic");
    expect(select().textContent).not.toContain("not offered");
    expect(saveButton()?.matches(":disabled")).toBe(true);
    await choose("CA default");
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));
  });

  it("partial: a saved profile the CA no longer offers shows as not offered now", async () => {
    status = () => Response.json(withACME({ profile: "legacy" }));
    await opened();
    await waitFor(() => expect(select().textContent).toContain("legacy (not offered now)"));
    fireEvent.keyDown(select(), { key: "Enter" });
    await screen.findByRole("option", { name: "legacy (not offered now)" });
    expect(screen.getAllByRole("option")).toHaveLength(5);
  });

  it("partial: a saved profile with an empty directory keeps the select, so it can be put right", async () => {
    status = () => Response.json(withACME({ profile: "legacy" }));
    directory = () => Response.json(acmeDirectory({ profiles: [] }));
    await opened();
    await waitFor(() => expect(select().textContent).toContain("legacy (not offered now)"));
    await choose("CA default");
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));
  });

  it("overflow: every offered profile is listed and the menu scrolls inside its own height", async () => {
    const many = Array.from({ length: 40 }, (_, i) => `profile-${String(i).padStart(2, "0")}`);
    directory = () => Response.json(acmeDirectory({ profiles: profiles(...many) }));
    await opened();
    fireEvent.keyDown(select(), { key: "Enter" });
    await screen.findAllByRole("option");
    expect(screen.getAllByRole("option")).toHaveLength(41);
    const content = document.querySelector('[data-slot="select-content"]') as HTMLElement;
    expect(content.className).toContain("max-h-");
    expect(content.className).toContain("overflow-y-auto");
  });

  it("zero, one, many: no profiles shows the line, one or many show the select", async () => {
    directory = () => Response.json(acmeDirectory({ profiles: profiles("classic") }));
    const trigger = await opened();
    fireEvent.keyDown(trigger, { key: "Enter" });
    await screen.findAllByRole("option");
    expect(screen.getAllByRole("option")).toHaveLength(2);
    expect(screen.queryByText(/offers no profiles/)).toBeNull();
  });

  it("long text: the trigger truncates with the full name in title, the item wraps", async () => {
    const long = `${"averylongprofilename".repeat(8)}`;
    status = () => Response.json(withACME({ profile: long }));
    directory = () => Response.json(acmeDirectory({ profiles: profiles(long) }));
    await opened();
    await waitFor(() => expect(select().getAttribute("title")).toBe(long));
    const label = select().querySelector("span.truncate");
    expect(label?.textContent).toBe(long);
    fireEvent.keyDown(select(), { key: "Enter" });
    const option = await screen.findByRole("option", { name: new RegExp(long) });
    expect(option.querySelector("span.break-words")).not.toBeNull();
  });

  it("draws the CA's names and descriptions as text, never as markup", async () => {
    directory = () =>
      Response.json(
        acmeDirectory({
          profiles: [{ name: "classic", description: '<img src=x onerror="alert(1)">' }],
        }),
      );
    await opened();
    fireEvent.keyDown(select(), { key: "Enter" });
    await screen.findByRole("option", { name: /classic/ });
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByText('<img src=x onerror="alert(1)">')).toBeTruthy();
  });

  it("enables Save profile only when the choice differs, and saves the chosen name", async () => {
    await opened();
    await waitFor(() => expect(saveButton()).not.toBeNull());
    expect(saveButton()?.matches(":disabled")).toBe(true);
    await choose(/^shortlived/);
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));
    // Choosing the saved value again puts it back.
    await choose("CA default");
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(true));
    await choose(/^shortlived/);
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));

    // The status the page reads after the save carries the new profile.
    let release: (() => void) | undefined;
    put = (body) =>
      new Promise<Response>((resolve) => {
        release = () => resolve(Response.json({ profile: body.profile }));
      });
    status = () => Response.json(withACME({ profile: "shortlived" }));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(saveButton()?.textContent).toContain("Saving..."));
    expect(select().matches(":disabled")).toBe(true);
    release?.();
    await waitFor(() => expect(successToast).toHaveBeenCalled());
    expect(puts().map((p) => p.body)).toEqual([{ profile: "shortlived" }]);
    await waitFor(() => expect(saveButton()?.textContent).toBe("Save profile"));
    expect(saveButton()?.matches(":disabled")).toBe(true);
    expect(select().textContent).toContain("shortlived");
  });

  it("toasts Profile saved.", async () => {
    await opened();
    await choose(/^classic/);
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(successToast).toHaveBeenCalledTimes(1));
    const [node] = successToast.mock.calls[0] as [{ props: { children: string } }];
    expect(node.props.children).toBe("Profile saved.");
  });

  it("saves the CA default as an empty name", async () => {
    status = () => Response.json(withACME({ profile: "classic" }));
    await opened();
    await waitFor(() => expect(select().textContent).toContain("classic"));
    await choose("CA default");
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ profile: "" });
  });

  it("says so when the save is refused, and reads the status again after a 409", async () => {
    put = () =>
      Response.json(
        { type: "acme_run_active", title: "x", status: 409, detail: "d" },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    await opened();
    await choose(/^classic/);
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));
    const before = sent.filter((s) => s.path === "/api/https/status").length;
    fireEvent.click(saveButton() as HTMLElement);
    await card().findByText("Something went wrong. Try again.");
    await waitFor(() =>
      expect(sent.filter((s) => s.path === "/api/https/status").length).toBeGreaterThan(before),
    );
    expect(successToast).not.toHaveBeenCalled();
  });

  it("is not rendered in A (no account) or I (certificate files, nothing kept)", async () => {
    status = () => Response.json(httpsStatus());
    await ready();
    expect(card().queryByRole("button", { name: "Advanced" })).toBeNull();
    cleanup();
    queryClient.clear();
    status = () =>
      Response.json(
        httpsStatus({
          certFiles: true,
          acme: httpsACME({ pausedReason: "cert_files" }),
        }),
      );
    await ready();
    expect(card().queryByRole("button", { name: "Advanced" })).toBeNull();
  });

  it.each([
    ["C: a run is active", () => withACME({ activeRun: run }), true],
    [
      "H: paused after a restore",
      () => withACME({ pausedReason: "restore", hasState: true }),
      false,
    ],
    [
      "J: certificate files, state kept",
      () => withACME({ pausedReason: "cert_files", hasState: true }, { certFiles: true }),
      false,
    ],
  ])("is drawn disabled with no Save button in %s", async (_name, make, reads) => {
    status = () => Response.json(make());
    const el = await opened();
    expect(el.matches(":disabled")).toBe(true);
    expect(saveButton()).toBeNull();
    // The CA's directory is read only where the server answers it.
    expect(sent.some((s) => s.path === "/api/https/acme/directory")).toBe(reads);
  });

  it("is drawn disabled with no Save button without settings.write", async () => {
    me = viewer;
    const el = await opened();
    expect(el.matches(":disabled")).toBe(true);
    expect(saveButton()).toBeNull();
    expect(puts()).toHaveLength(0);
  });
});

describe("a profile fixed by TIERPLAN_ACME_PROFILE (D-24)", () => {
  const locked = (stored?: string) =>
    httpsLockedStatus(
      { profile: { var: "TIERPLAN_ACME_PROFILE", ...(stored ? { stored } : {}) } },
      { profile: "tlsserver" },
    );

  it("shows the effective profile in a disabled select, with no Save, and the variable and stored value beneath", async () => {
    status = () => Response.json(locked("classic"));
    const el = await opened();
    expect(el.matches(":disabled")).toBe(true);
    expect(el.textContent).toContain("tlsserver");
    expect(saveButton()).toBeNull();
    const note = document.getElementById("acme-profile-lock") as HTMLElement;
    expect(note.textContent).toBe(
      "Set by the TIERPLAN_ACME_PROFILE environment variable.Stored value: classic, used if TIERPLAN_ACME_PROFILE is removed.",
    );
    // The select is described by the lock lines.
    expect(el.getAttribute("aria-describedby")).toContain("acme-profile-lock");
    expect(within(note).getByRole("img", { name: "Locked" })).toBeTruthy();
    expect(puts()).toHaveLength(0);
  });

  it("reads 'none' beneath when nothing is stored", async () => {
    status = () => Response.json(locked());
    await opened();
    expect(document.getElementById("acme-profile-lock")?.textContent).toContain(
      "Stored value: none, used if TIERPLAN_ACME_PROFILE is removed.",
    );
  });

  it("names the variable the API gives, not a fixed one", async () => {
    status = () =>
      Response.json(
        httpsLockedStatus({ profile: { var: "TIERPLAN_OTHER", stored: "x" } }, { profile: "" }),
      );
    const el = await opened();
    expect(el.textContent).toContain("CA default");
    expect(document.getElementById("acme-profile-lock")?.textContent).toContain(
      "Set by the TIERPLAN_OTHER environment variable.",
    );
  });

  it("keeps the lock note for a viewer, who sees no Save either", async () => {
    me = viewer;
    status = () => Response.json(locked("classic"));
    await opened();
    expect(document.getElementById("acme-profile-lock")).not.toBeNull();
    expect(saveButton()).toBeNull();
  });

  it("redraws as locked on a 409 acme_profile_locked, with no failure line and no toast", async () => {
    // The variable is added after the page loaded: only the status read after the refusal has it.
    let refused = false;
    status = () => Response.json(refused ? locked("classic") : withACME({ profile: "classic" }));
    put = () => {
      refused = true;
      return Response.json(
        { type: "acme_profile_locked", title: "x", status: 409, detail: "d" },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    };
    await opened();
    await choose(/^shortlived/);
    await waitFor(() => expect(saveButton()?.matches(":disabled")).toBe(false));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(document.getElementById("acme-profile-lock")).not.toBeNull());
    expect(card().queryByText("Something went wrong. Try again.")).toBeNull();
    expect(successToast).not.toHaveBeenCalled();
    expect(select().matches(":disabled")).toBe(true);
    // The unsent draft is gone: the select shows what the environment fixes.
    expect(select().textContent).toContain("tlsserver");
    expect(saveButton()).toBeNull();
  });
});
