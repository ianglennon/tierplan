// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ServiceItem } from "@/api/services";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RollbackDialog } from "@/features/services/rollback-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

const web: ServiceItem = {
  id: "id-web",
  name: "web",
  mode: "replicated",
  running: 2,
  desired: 3,
  updateState: "",
  image: "acme/web:2",
  stack: "",
  bayplanStackId: "",
  agent: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ports: [],
  updateMessage: "",
  hasPreviousSpec: true,
  createdAt: "2026-09-01T10:00:00Z",
};

const specOf = (image: string, env: string[] = ["MODE=prod"]) => ({
  Name: "web",
  TaskTemplate: { ContainerSpec: { Image: image, Env: env } },
});

interface SpecAnswer {
  version: number;
  name: string;
  mode: string;
  stack: string;
  bayplanStackId: string;
  agent: boolean;
  masked: boolean;
  spec: Record<string, unknown>;
  previousSpec?: Record<string, unknown>;
}

const loaded = (extra: Partial<SpecAnswer> = {}): SpecAnswer => ({
  version: 9,
  name: "web",
  mode: "replicated",
  stack: "",
  bayplanStackId: "",
  agent: false,
  masked: false,
  spec: specOf("acme/web:2"),
  previousSpec: specOf("acme/web:1"),
  ...extra,
});

let posted: unknown[];
let specAnswer: () => Response | Promise<Response>;
let answer: () => Response;

beforeEach(() => {
  posted = [];
  specAnswer = () => Response.json(loaded());
  answer = () => Response.json({ image: "acme/web:1" }, { status: 202 });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === `/api/env/${ENV}/services/id-web/spec`) return specAnswer();
      if (path === `/api/env/${ENV}/services/id-web/rollback`) {
        posted.push(await req.clone().json());
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
  cleanup();
  vi.unstubAllGlobals();
});

function problem(status: number, type: string, extra: Record<string, unknown> = {}) {
  return Response.json({ status, type, title: "x", ...extra }, { status });
}

function mount(target: ServiceItem = web) {
  const onOpenChange = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RollbackDialog envId={ENV} target={target} open onOpenChange={onOpenChange} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onOpenChange };
}

const confirm = () => screen.getByRole("button", { name: "Roll back service" });
const diff = () => screen.findByRole("table", { name: "Changes to web" });

describe("RollbackDialog", () => {
  it("shows a skeleton in place of the diff until the earlier version loads, with Roll back service unavailable", async () => {
    let open: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      open = resolve;
    });
    specAnswer = async () => {
      await held;
      return Response.json(loaded());
    };
    mount();
    const dialog = screen.getByRole("dialog", { name: "Roll back web?" });
    expect(within(dialog).getByTestId("rollback-diff-skeleton")).toBeTruthy();
    expect(confirm().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm());
    expect(posted).toEqual([]);
    open();
    await diff();
    expect(screen.queryByTestId("rollback-diff-skeleton")).toBeNull();
    expect(confirm().getAttribute("aria-disabled")).toBeNull();
  });

  it("is titled Roll back {name}? and says what the diff is, with Now and After rollback", async () => {
    mount();
    const dialog = screen.getByRole("dialog", { name: "Roll back web?" });
    expect(
      within(dialog).getByText(
        "The service goes back to the settings it had before its last update. Swarm rolls tasks back a batch at a time.",
      ),
    ).toBeTruthy();
    const table = await diff();
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Change", "Setting", "Now", "After rollback"]);
    expect(within(table).getByText("acme/web:2")).toBeTruthy();
    expect(within(table).getByText("acme/web:1")).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(dialog.className).toContain("sm:max-w-200");
  });

  it("rolls back at the version it showed, tells the person and closes", async () => {
    const { onOpenChange } = mount();
    await diff();
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ version: 9 });
    expect(await screen.findByText("Rolling back web.")).toBeTruthy();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("shows the pending label and sends one request however often Roll back service is clicked", async () => {
    let open: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      open = resolve;
    });
    answer = () => Response.json({ image: "acme/web:1" }, { status: 202 });
    const base = globalThis.fetch;
    vi.stubGlobal("fetch", async (req: Request) => {
      if (new URL(req.url).pathname.endsWith("/rollback")) await held;
      return base(req);
    });
    mount();
    await diff();
    fireEvent.click(confirm());
    const pending = await screen.findByRole("button", { name: "Rolling back…" });
    expect(pending.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(pending);
    open();
    await waitFor(() => expect(screen.queryByRole("button", { name: "Rolling back…" })).toBeNull());
    expect(posted).toHaveLength(1);
  });

  it("says there is no earlier version, and cannot be confirmed, when the service has no previous spec", async () => {
    specAnswer = () => Response.json(loaded({ previousSpec: undefined }));
    mount({ ...web, hasPreviousSpec: false });
    expect(await screen.findByText("There's no earlier version to roll back to.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
    expect(confirm().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm());
    expect(posted).toEqual([]);
  });

  it("warns that the next stack deploy applies the compose file again for a stack service", async () => {
    specAnswer = () => Response.json(loaded({ stack: "shop" }));
    mount({ ...web, stack: "shop" });
    expect(screen.getByText("The next stack deploy applies the compose file again.")).toBeTruthy();
    await diff();
  });

  it("shows no stack warning for a service that belongs to no stack", async () => {
    mount();
    await diff();
    expect(screen.queryByText(/stack deploy/)).toBeNull();
  });

  // The rollback warning names no stack, so the link beside it is Open stack.
  it("offers Open stack beside the warning when Bayplan manages the stack, the text unchanged", async () => {
    specAnswer = () => Response.json(loaded({ stack: "shop", bayplanStackId: "stk-shop" }));
    mount({ ...web, stack: "shop", bayplanStackId: "stk-shop" });
    const link = screen.getByRole("link", { name: "Open stack" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/stk-shop");
    expect(link.closest('[role="status"]')?.textContent).toContain(
      "The next stack deploy applies the compose file again.",
    );
    await diff();
  });

  it("offers no Open stack for a stack Bayplan does not manage", async () => {
    specAnswer = () => Response.json(loaded({ stack: "shop" }));
    mount({ ...web, stack: "shop" });
    expect(screen.getByText("The next stack deploy applies the compose file again.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Open stack" })).toBeNull();
    await diff();
  });

  it("prints withheld values as bullets and never lists an unchanged one", async () => {
    specAnswer = () =>
      Response.json(
        loaded({
          masked: true,
          spec: specOf("acme/web:2", ["TOKEN=<hidden>", "MODE=prod"]),
          previousSpec: specOf("acme/web:1", ["TOKEN=<hidden>", "MODE=dev"]),
        }),
      );
    mount();
    const table = await diff();
    expect(within(table).queryByText(/TOKEN/)).toBeNull();
    expect(table.textContent).not.toContain("<hidden>");
  });

  it("names a secret that is gone and keeps the dialog open", async () => {
    answer = () =>
      problem(409, "rollback_object_missing", {
        detail: "secret not found: db_password",
        errors: [
          { location: "object.kind", message: "secret" },
          { location: "object.name", message: "db_password" },
        ],
      });
    const { onOpenChange } = mount();
    await diff();
    fireEvent.click(confirm());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Can't roll back: secret db_password no longer exists. Create it again or edit the service instead.",
    );
    expect(alert.textContent).not.toContain("rollback_object_missing");
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("words a privileged earlier version as needing Admin, naming each setting, and keeps the dialog open", async () => {
    answer = () =>
      problem(403, "privileged_service", {
        errors: [
          {
            location: "TaskTemplate.ContainerSpec.Mounts[0].Source",
            message: "/var/run/docker.sock",
          },
        ],
      });
    const { onOpenChange } = mount();
    await diff();
    fireEvent.click(confirm());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Can't roll back: the earlier version uses settings that need Admin (TaskTemplate.ContainerSpec.Mounts[0].Source: /var/run/docker.sock). Ask an administrator to roll it back.",
    );
    expect(alert.textContent).not.toContain("You no longer have permission");
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("words a missing object the manager did not name without a name", async () => {
    answer = () => problem(409, "rollback_object_missing", { detail: "something odd not found" });
    mount();
    await diff();
    fireEvent.click(confirm());
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Can't roll back: something the earlier version uses no longer exists. Create it again or edit the service instead.",
    );
  });

  it.each([
    [409, "service_changed", /was changed by someone else after you looked/],
    [409, "no_previous_spec", /There's no earlier version to roll back to/],
    [404, "service_not_found", /This service is gone/],
    [503, "no_manager_connected", /No manager is connected for Swarm one/],
    [503, "no_quorum", /The swarm has lost its quorum/],
    [503, "registry_unresolved", /couldn't read the registry credential/],
    [409, "host_read_only", /This host is read-only/],
    [403, "forbidden", /You no longer have permission/],
    [502, "rollback_failed", /Something went wrong on the server/],
  ])("words a %i %s in the dialog and never shows the code", async (status, type, words) => {
    answer = () => problem(status, type);
    mount();
    await diff();
    fireEvent.click(confirm());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(words);
    expect(alert.textContent).not.toContain(type);
    expect(screen.getByRole("dialog", { name: "Roll back web?" })).toBeTruthy();
  });

  it("shows the manager's words when it refused the rollback for another reason", async () => {
    answer = () => problem(422, "rollback_refused", { detail: "update out of sequence" });
    mount();
    await diff();
    fireEvent.click(confirm());
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Docker rejected the rollback: update out of sequence",
    );
  });

  it("says the earlier version could not be loaded, and cannot be confirmed", async () => {
    specAnswer = () => problem(502, "offline");
    mount();
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Couldn't load the earlier version.",
    );
    expect(confirm().getAttribute("aria-disabled")).toBe("true");
  });

  it("says the service is gone when it was removed while the dialog was open", async () => {
    specAnswer = () => problem(404, "service_not_found");
    mount();
    expect((await screen.findByRole("alert")).textContent).toMatch(/This service is gone/);
    expect(confirm().getAttribute("aria-disabled")).toBe("true");
  });
});
