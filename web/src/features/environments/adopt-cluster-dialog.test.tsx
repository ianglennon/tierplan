// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { AdoptClusterDialog } from "@/features/environments/adopt-cluster-dialog";
import { meQueryOptions } from "@/hooks/use-me";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = { id: "e2", name: "edge swarm", pendingClusterId: "x3k2m9t0q4w5e6r7t8y9u0i1o" };

let posts: { path: string; csrf: string | null }[];
let answer: () => Response | Promise<Response>;
let me = makeMe({ instanceAdmin: true });

beforeEach(() => {
  posts = [];
  me = makeMe({ instanceAdmin: true });
  answer = () => Response.json({ clusterId: ENV.pendingClusterId });
  setCsrfToken("csrf-1");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (req.method === "POST") posts.push({ path, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/env/e2/adopt-cluster") return answer();
      return Response.json({ items: [] });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
  setCsrfToken(null);
});

function show(environment: Partial<typeof ENV> = {}, onOpenChange = vi.fn()) {
  // Who is signed in is read from the cache the app fills at sign-in.
  queryClient.setQueryData(meQueryOptions.queryKey, me);
  render(
    <QueryClientProvider client={queryClient}>
      <AdoptClusterDialog
        environment={{ ...ENV, ...environment }}
        open
        onOpenChange={onOpenChange}
      />
    </QueryClientProvider>,
  );
  return onOpenChange;
}

describe("AdoptClusterDialog", () => {
  it("asks Adopt the new cluster? and names the environment and the cluster ID, with Cancel and the primary confirm", () => {
    show();
    const dialog = screen.getByRole("dialog", { name: "Adopt the new cluster?" });
    expect(
      within(dialog).getByText(
        "edge swarm will be bound to the swarm that now reports cluster ID x3k2m9t0q4w5e6r7t8y9u0i1o. This is recorded in the audit log.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Adopt new cluster" })).toBeTruthy();
  });

  it("says so without a placeholder when the cluster ID is not known", () => {
    show({ pendingClusterId: undefined });
    const dialog = screen.getByRole("dialog", { name: "Adopt the new cluster?" });
    expect(dialog.textContent).toContain(
      "edge swarm will be bound to the swarm that now reports a new cluster ID.",
    );
    expect(dialog.textContent).not.toContain("{{");
  });

  it("posts once however often confirm is clicked, shows the spinner, and closes with the environments list read again", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    const onOpenChange = show();
    const confirm = screen.getByRole("button", { name: "Adopt new cluster" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    const pending = await screen.findByRole("button", { name: "Adopting…" });
    expect((pending as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(posts).toEqual([{ path: "/api/env/e2/adopt-cluster", csrf: "csrf-1" }]);

    await act(async () => release(Response.json({ clusterId: ENV.pendingClusterId })));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["environments"] });
    expect(posts).toHaveLength(1);
  });

  it("keeps the dialog open on a failure and says why in an alert, then lets the admin try again", async () => {
    answer = () => new Response(null, { status: 500 });
    const onOpenChange = show();
    fireEvent.click(screen.getByRole("button", { name: "Adopt new cluster" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Couldn't adopt the new cluster. Try again.");
    expect(onOpenChange).not.toHaveBeenCalled();
    const retry = screen.getByRole("button", { name: "Adopt new cluster" }) as HTMLButtonElement;
    expect(retry.disabled).toBe(false);

    answer = () => Response.json({ clusterId: ENV.pendingClusterId });
    fireEvent.click(retry);
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(posts).toHaveLength(2);
  });

  it("says when another admin adopted it first, and when another environment holds the cluster", async () => {
    answer = () =>
      Response.json(
        { type: "no_cluster_change", title: "x", status: 409 },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    show();
    fireEvent.click(screen.getByRole("button", { name: "Adopt new cluster" }));
    expect((await screen.findByRole("alert")).textContent).toContain("nothing left to adopt");

    answer = () =>
      Response.json(
        { type: "cluster_in_use", title: "x", status: 409 },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    fireEvent.click(screen.getByRole("button", { name: "Adopt new cluster" }));
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("already manages that cluster"),
    );
  });

  it("is offered to an env Admin of that environment, and to nobody who lacks environments.configure on it", () => {
    me = makeMe({ envs: { e2: "admin" } });
    show();
    expect(screen.getByRole("dialog", { name: "Adopt the new cluster?" })).toBeTruthy();
    cleanup();

    // A Viewer of it, and an Admin of some other environment, get nothing to confirm.
    me = makeMe({ envs: { e2: "viewer", e9: "admin" } });
    show();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(posts).toHaveLength(0);
  });

  it("closes on Cancel without posting", () => {
    const onOpenChange = show();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(posts).toHaveLength(0);
  });
});
