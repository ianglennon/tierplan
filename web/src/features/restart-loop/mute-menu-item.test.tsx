// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { MuteMenuItem } from "@/features/restart-loop/mute-menu-item";
import "@/i18n";

// The mute item of a service's menus (plan 06-35, RES-13, D-16, UI-SPEC "Restart loop"): it toggles
// the alerts of one service, with a toast, and is unavailable with the reason below Operator.

interface Sent {
  method: string;
  path: string;
  search?: string;
}

let sent: Sent[];
let answer: () => Response;
let client: QueryClient;

beforeEach(() => {
  sent = [];
  answer = () => Response.json({ serviceId: "svc-web", muted: true, changed: true });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken("csrf-1");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      sent.push({
        method: req.method,
        path: url.pathname,
        ...(url.search && { search: url.search }),
      });
      return answer();
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  setCsrfToken(null);
});

const ALLOWED = { allowed: true };
const OPERATOR_NEEDED = { allowed: false, reason: "Needs Operator" };

async function mount(props: { muted: boolean; needs?: { allowed: boolean; reason?: string } }) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <DropdownMenu defaultOpen>
          <DropdownMenuTrigger>menu</DropdownMenuTrigger>
          <DropdownMenuContent>
            <MuteMenuItem
              envId="e1"
              serviceId="svc-web"
              name="web"
              muted={props.muted}
              needs={props.needs ?? ALLOWED}
            />
          </DropdownMenuContent>
        </DropdownMenu>
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await act(async () => {});
}

describe("MuteMenuItem", () => {
  it("mutes the service and toasts 'Muted restart-loop alerts for web.'", async () => {
    await mount({ muted: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" }));
    await waitFor(() =>
      expect(sent).toEqual([
        { method: "PUT", path: "/api/env/e1/services/svc-web/restart-loop-mute" },
      ]),
    );
    expect(await screen.findByText("Muted restart-loop alerts for web.")).toBeTruthy();
  });

  it("unmutes a muted service and toasts 'Unmuted restart-loop alerts for web.'", async () => {
    answer = () => Response.json({ serviceId: "svc-web", muted: false, changed: true });
    await mount({ muted: true });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Unmute restart-loop alerts" }));
    await waitFor(() =>
      expect(sent).toEqual([
        { method: "DELETE", path: "/api/env/e1/services/svc-web/restart-loop-mute" },
      ]),
    );
    expect(await screen.findByText("Unmuted restart-loop alerts for web.")).toBeTruthy();
  });

  it("is unavailable with 'Needs Operator' below Operator, and sends nothing", async () => {
    await mount({ muted: false, needs: OPERATOR_NEEDED });
    const item = await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(item);
    expect(sent).toEqual([]);
  });

  it("says what went wrong when the server refuses", async () => {
    answer = () => Response.json({ status: 404, type: "service_not_found" }, { status: 404 });
    await mount({ muted: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" }));
    expect(
      await screen.findByText("Couldn't change the restart-loop alerts for web. Try again."),
    ).toBeTruthy();
  });
});

// The container variant (plan 06-36, RES-13, D-16): the same item, for a standalone container,
// named by the node it is on.

async function mountContainer(props: {
  muted: boolean;
  needs?: { allowed: boolean; reason?: string };
}) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <DropdownMenu defaultOpen>
          <DropdownMenuTrigger>menu</DropdownMenuTrigger>
          <DropdownMenuContent>
            <MuteMenuItem
              kind="container"
              envId="e1"
              nodeId="n1"
              containerId="crashy-id"
              name="crashy"
              muted={props.muted}
              needs={props.needs ?? ALLOWED}
            />
          </DropdownMenuContent>
        </DropdownMenu>
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await act(async () => {});
}

describe("MuteMenuItem for a container", () => {
  it("mutes the container on its node and toasts 'Muted restart-loop alerts for crashy.'", async () => {
    answer = () => Response.json({ containerId: "crashy-id", muted: true, changed: true });
    await mountContainer({ muted: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" }));
    await waitFor(() =>
      expect(sent).toEqual([
        {
          method: "PUT",
          path: "/api/env/e1/containers/crashy-id/restart-loop-mute",
          search: "?nodeId=n1",
        },
      ]),
    );
    expect(await screen.findByText("Muted restart-loop alerts for crashy.")).toBeTruthy();
  });

  it("unmutes a muted container and toasts 'Unmuted restart-loop alerts for crashy.'", async () => {
    answer = () => Response.json({ containerId: "crashy-id", muted: false, changed: true });
    await mountContainer({ muted: true });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Unmute restart-loop alerts" }));
    await waitFor(() =>
      expect(sent).toEqual([
        {
          method: "DELETE",
          path: "/api/env/e1/containers/crashy-id/restart-loop-mute",
          search: "?nodeId=n1",
        },
      ]),
    );
    expect(await screen.findByText("Unmuted restart-loop alerts for crashy.")).toBeTruthy();
  });

  it("is unavailable with 'Needs Operator' below Operator, and sends nothing", async () => {
    await mountContainer({ muted: false, needs: OPERATOR_NEEDED });
    const item = await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(item);
    expect(sent).toEqual([]);
  });

  it("says what went wrong when the server refuses", async () => {
    answer = () => Response.json({ status: 404, type: "container_not_found" }, { status: 404 });
    await mountContainer({ muted: false });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" }));
    expect(
      await screen.findByText("Couldn't change the restart-loop alerts for crashy. Try again."),
    ).toBeTruthy();
  });
});
