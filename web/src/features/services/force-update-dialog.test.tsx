// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ServiceItem } from "@/api/services";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ForceUpdateDialog } from "@/features/services/force-update-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

const web: ServiceItem = {
  id: "id-web",
  name: "web",
  mode: "replicated",
  running: 2,
  desired: 3,
  updateState: "",
  image: "localhost:5000/private/web:1",
  stack: "",
  bayplanStackId: "",
  agent: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ports: [],
  updateMessage: "",
  hasPreviousSpec: true,
  createdAt: "2026-09-01T10:00:00Z",
};

let posted: unknown[];
let answer: () => Response;
// A test that wants the request held sets this and opens it when it is done looking.
let gate: Promise<void> | null = null;

beforeEach(() => {
  posted = [];
  answer = () =>
    Response.json({
      image: "localhost:5000/private/web:1@sha256:aaa",
      digest: "sha256:aaa",
      pinned: true,
      previousImage: "localhost:5000/private/web:1",
    });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === `/api/env/${ENV}/services/id-web/force-update`) {
        posted.push(await req.clone().json());
        if (gate) await gate;
        return answer();
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm one", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  // sonner replays the toasts still active to the next Toaster that mounts, so a toast raised
  // by one test (the started one, the lost-answer one) would be on the page of the next.
  toast.dismiss();
  cleanup();
  gate = null;
  vi.unstubAllGlobals();
});

function problem(status: number, type: string, extra: Record<string, unknown> = {}) {
  return Response.json({ status, type, title: "x", ...extra }, { status });
}

function mount(opts: { target?: ServiceItem; retry?: boolean } = {}) {
  const onOpenChange = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <ForceUpdateDialog
          envId={ENV}
          target={opts.target ?? web}
          open
          onOpenChange={onOpenChange}
          retry={opts.retry}
        />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onOpenChange };
}

const confirm = (name = "Force update") => screen.getByRole("button", { name });

describe("ForceUpdateDialog", () => {
  it("asks Force update {name}? and says what happens, with Cancel and Force update", () => {
    mount();
    const dialog = screen.getByRole("dialog", { name: "Force update web?" });
    expect(
      within(dialog).getByText(
        "Swarm replaces every task, a batch at a time, using the service's update settings. Bayplan looks up the image again, so a tag that has moved is picked up.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(confirm()).toBeTruthy();
    expect(dialog.className).toContain("sm:max-w-140");
  });

  it("sends the force update once, tells the person it started and closes", async () => {
    const { onOpenChange } = mount();
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({});
    expect(await screen.findByText("Started a forced update of web.")).toBeTruthy();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("is Retry the update on {name}? with Retry update in retry mode, and says it is a retry", async () => {
    const { onOpenChange } = mount({ retry: true });
    const dialog = screen.getByRole("dialog", { name: "Retry the update on web?" });
    expect(
      within(dialog).getByText("Swarm tries the update again with the service's current settings."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Force update" })).toBeNull();
    fireEvent.click(confirm("Retry update"));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ retry: true });
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("shows the spinner and the pending label, and does not send a second request on a second click", async () => {
    let open: () => void = () => {};
    gate = new Promise<void>((resolve) => {
      open = resolve;
    });
    mount();
    fireEvent.click(confirm());
    const pending = await screen.findByRole("button", { name: "Updating…" });
    expect(pending.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(pending);
    fireEvent.click(pending);
    open();
    await waitFor(() => expect(screen.queryByRole("button", { name: "Updating…" })).toBeNull());
    expect(posted).toHaveLength(1);
  });

  it("warns that the next stack deploy applies the compose file again for a stack service", () => {
    mount({ target: { ...web, stack: "shop" } });
    expect(
      screen.getByText(
        "web belongs to stack shop. The next stack deploy applies the compose file again.",
      ),
    ).toBeTruthy();
  });

  it("shows no stack warning for a service that belongs to no stack", () => {
    mount();
    expect(screen.queryByText(/belongs to stack/)).toBeNull();
  });

  it("names the image and the registry when the digest could not be looked up, and stays open", async () => {
    answer = () =>
      problem(422, "image_unresolved", {
        errors: [
          { location: "image.ref", message: "localhost:5000/private/web:1" },
          { location: "image.host", message: "localhost:5000" },
          { location: "reason", message: "pull_unauthorized" },
        ],
      });
    const { onOpenChange } = mount();
    fireEvent.click(confirm());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Couldn't look up localhost:5000/private/web:1 on localhost:5000 to pin it to a digest. Check the name, the tag and the credential, then try again.",
    );
    expect(alert.textContent).not.toContain("image_unresolved");
    expect(screen.getByRole("dialog", { name: "Force update web?" })).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalled();
    // The person can try again.
    expect(confirm().getAttribute("aria-disabled")).toBeNull();
  });

  it.each([
    [409, "service_changed", /was changed by someone else/],
    [404, "service_not_found", /This service is gone/],
    [503, "no_manager_connected", /No manager is connected for Swarm one/],
    [503, "no_quorum", /The swarm has lost its quorum/],
    [503, "registry_unresolved", /couldn't read the registry credential/],
    [409, "host_read_only", /This host is read-only/],
    [409, "agent_host_policy", /don't let Bayplan change services with host access/],
    [403, "forbidden", /You no longer have permission/],
    [502, "force_update_failed", /Something went wrong on the server/],
  ])("words a %i %s in the dialog and never shows the code", async (status, type, words) => {
    answer = () => problem(status, type);
    mount();
    fireEvent.click(confirm());
    // The environment's name arrives from its own request, so the words are waited for.
    const alert = await screen.findByText(words);
    expect(alert.textContent).not.toContain(type);
    expect(screen.getByRole("dialog", { name: "Force update web?" })).toBeTruthy();
  });

  it("shows the manager's own words when it refused the update", async () => {
    answer = () => problem(502, "force_update_failed", { detail: "rpc error: no suitable node" });
    mount();
    fireEvent.click(confirm());
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Docker rejected the update: rpc error: no suitable node",
    );
  });

  it("shows the agent notice when the agent refused by its policy", async () => {
    answer = () => problem(502, "agent_policy_denied");
    mount();
    fireEvent.click(confirm());
    expect(await screen.findByText("This host's agent needs an update")).toBeTruthy();
  });

  it("says the answer was lost when the server could not be reached", async () => {
    answer = () => {
      throw new TypeError("network down");
    };
    mount();
    fireEvent.click(confirm());
    expect((await screen.findByRole("alert")).textContent).toMatch(
      /The connection dropped before Bayplan answered/,
    );
  });
});
