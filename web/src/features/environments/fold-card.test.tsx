// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { FoldCard } from "@/features/environments/fold-card";
import "@/i18n";

function env(extra: Record<string, unknown>): EnvironmentItem {
  return {
    id: "e2",
    name: "edge swarm",
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state: "online" },
    envType: "swarm",
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    foldOffered: true,
    ...extra,
  } as EnvironmentItem;
}

const SWARM = env({});
const LOCAL = env({
  id: "e1",
  name: "lab-1",
  kind: "local",
  isSelfHost: true,
  isLocalSocket: true,
  envType: "swarm",
});

interface Sent {
  method: string;
  path: string;
  csrf: string | null;
}

let sent: Sent[];
let answer: () => Response | Promise<Response>;
// What GET fold-offer answers.
let offerAnswer: () => Response | Promise<Response>;
// What GET /api/environments answers, so a refetch keeps the list the test set.
let listed: EnvironmentItem[];
const OFFERED = { offered: true, source: "local_environment", role: "manager" };
// The counts of a Local environment that holds nothing the cascade would delete.
const NO_REMOVALS = {
  stacks: 0,
  gitRepos: 0,
  registries: 0,
  gitCredentials: 0,
  imageUpdateIgnores: 0,
  janitorKeeps: 0,
  runs: 0,
  janitor: false,
};

// tick answers the confirmation every fold of a Local environment asks for.
function tick(dialog: HTMLElement) {
  fireEvent.click(within(dialog).getByRole("checkbox"));
}

// failureAlert is the alert that reports a failed fold: the last one in the dialog, after the
// warning that lists what the fold removes.
function failureAlert(dialog: HTMLElement) {
  return waitFor(() => {
    const alerts = within(dialog).getAllByRole("alert");
    const last = alerts[alerts.length - 1];
    if (alerts.length < 2 || !last) throw new Error("no failure alert yet");
    return last;
  });
}

beforeEach(() => {
  sent = [];
  answer = () => Response.json({ ...SWARM, foldOffered: undefined });
  offerAnswer = () => Response.json(OFFERED);
  listed = [LOCAL, SWARM];
  setCsrfToken("csrf-1");
  queryClient.setQueryData(environmentsQuery.queryKey, { items: [LOCAL, SWARM] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ method: req.method, path, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/environments/e2/fold-local") return answer();
      if (path === "/api/environments/e2/fold-offer") return offerAnswer();
      if (path === "/api/environments") return Response.json({ items: listed });
      return Response.json({ items: [] });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
  setCsrfToken(null);
});

function show(target: EnvironmentItem = SWARM, onFolded = vi.fn()) {
  render(
    <QueryClientProvider client={queryClient}>
      <FoldCard env={target} onFolded={onFolded} />
      <Toaster />
    </QueryClientProvider>,
  );
  return onFolded;
}

const posts = () => sent.filter((s) => s.method === "POST");

describe("FoldCard", () => {
  it("says this host is a node in the swarm and offers Fold into {swarm} under the Local host title", async () => {
    show();
    const card = await screen.findByRole("region", { name: "Local host" });
    expect(
      within(card).getByText(
        "This TierPlan host is a node in edge swarm. TierPlan can reach it through the Docker socket, so it needs no separate environment.",
      ),
    ).toBeTruthy();
    expect(within(card).getByRole("button", { name: "Fold into edge swarm" })).toBeTruthy();
  });

  it("renders nothing while the offer is loading, after it fails, or when it is not offered", async () => {
    // Loading: the read never answers.
    offerAnswer = () => new Promise<Response>(() => {});
    show();
    expect(screen.queryByRole("region", { name: "Local host" })).toBeNull();
    cleanup();
    queryClient.removeQueries({ queryKey: ["fold-offer"] });

    // Failed.
    offerAnswer = () => Response.json({ title: "Internal", status: 500 }, { status: 500 });
    show();
    await waitFor(() =>
      expect(sent.filter((s) => s.path.endsWith("/fold-offer")).length).toBeGreaterThan(1),
    );
    expect(screen.queryByRole("region", { name: "Local host" })).toBeNull();
    cleanup();
    queryClient.removeQueries({ queryKey: ["fold-offer"] });

    // Not offered, no refusal.
    offerAnswer = () => Response.json({ offered: false });
    sent.length = 0;
    show();
    await waitFor(() => expect(sent.some((s) => s.path.endsWith("/fold-offer"))).toBe(true));
    await act(async () => {});
    expect(screen.queryByRole("region", { name: "Local host" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Fold into/ })).toBeNull();
  });

  it("offers the fold of a socket-only host from its own read and posts once", async () => {
    offerAnswer = () => Response.json({ offered: true, source: "socket", role: "worker" });
    // The list carries no offer for a socket-only install.
    show(env({ foldOffered: false }));
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog", { name: "Fold this host into edge swarm?" });
    expect(dialog.textContent).toContain(
      "The agent on this node keeps running, but TierPlan won't use it. It stays as a standby.",
    );
    expect(dialog.textContent).toContain(
      "Nothing changes on the host. This is recorded in the audit log.",
    );
    expect(dialog.textContent).not.toContain("The separate Local environment is removed.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]).toEqual({
      method: "POST",
      path: "/api/environments/e2/fold-local",
      csrf: "csrf-1",
    });
  });

  async function openDialog() {
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge swarm" }));
    return screen.findByRole("dialog", { name: "Fold this host into edge swarm?" });
  }

  it("lists only non-zero removals and needs the checkbox before a destructive confirm", async () => {
    offerAnswer = () =>
      Response.json({
        ...OFFERED,
        removes: { ...NO_REMOVALS, stacks: 2, gitRepos: 1, runs: 3, janitor: true },
      });
    const dialog = await openDialog();
    expect(
      within(dialog).getByText(
        "Removing the Local environment also removes everything else stored under it, for example:",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByText("2 stacks")).toBeTruthy();
    expect(within(dialog).getByText("1 Git repository")).toBeTruthy();
    expect(within(dialog).getByText("3 janitor or image update runs in the history")).toBeTruthy();
    expect(within(dialog).getByText("Janitor settings")).toBeTruthy();
    expect(within(dialog).queryByText(/registry login/)).toBeNull();
    expect(within(dialog).queryByText(/Git credential/)).toBeNull();
    expect(within(dialog).getByText("Running containers are not touched.")).toBeTruthy();

    const confirm = within(dialog).getByRole("button", {
      name: "Fold into edge swarm",
    }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    expect(confirm.getAttribute("data-variant")).toBe("destructive");
    fireEvent.click(confirm);
    expect(posts()).toEqual([]);

    fireEvent.click(
      within(dialog).getByRole("checkbox", {
        name: "I understand this removes the items above and everything else stored under that environment.",
      }),
    );
    await waitFor(() => expect(confirm.disabled).toBe(false));
    fireEvent.click(confirm);
    await waitFor(() => expect(posts()).toHaveLength(1));
  });

  it("needs the checkbox for a Local environment with nothing counted, and not for a socket fold", async () => {
    offerAnswer = () => Response.json({ ...OFFERED, removes: NO_REMOVALS });
    let dialog = await openDialog();
    // The row delete is irreversible whatever the counts, so the tick is still needed.
    expect(
      within(dialog).getByText(
        "Removing the Local environment also removes anything else stored under it. Nothing is listed here, and it can't be undone.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByRole("checkbox")).toBeTruthy();
    expect(within(dialog).getByText("The separate Local environment is removed.")).toBeTruthy();
    let confirm = within(dialog).getByRole("button", { name: "Fold into edge swarm" });
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
    expect(confirm.getAttribute("data-variant")).toBe("destructive");
    cleanup();
    queryClient.removeQueries({ queryKey: ["fold-offer"] });

    offerAnswer = () => Response.json({ offered: true, source: "socket", role: "worker" });
    dialog = await openDialog();
    expect(within(dialog).queryByRole("checkbox")).toBeNull();
    confirm = within(dialog).getByRole("button", { name: "Fold into edge swarm" });
    expect((confirm as HTMLButtonElement).disabled).toBe(false);
    expect(confirm.getAttribute("data-variant")).toBe("default");
  });

  it("uses the singular for one stack and names the other stored items", async () => {
    offerAnswer = () =>
      Response.json({
        ...OFFERED,
        removes: {
          ...NO_REMOVALS,
          stacks: 1,
          registries: 1,
          gitCredentials: 1,
          imageUpdateIgnores: 1,
          janitorKeeps: 1,
          runs: 1,
        },
      });
    const dialog = await openDialog();
    expect(within(dialog).getByText("1 stack")).toBeTruthy();
    expect(within(dialog).getByText("1 registry login")).toBeTruthy();
    expect(within(dialog).getByText("1 Git credential")).toBeTruthy();
    expect(within(dialog).getByText("1 image update ignore")).toBeTruthy();
    expect(within(dialog).getByText("1 item kept from the janitor")).toBeTruthy();
    expect(within(dialog).getByText("1 janitor or image update run in the history")).toBeTruthy();
  });

  it("lists plural counts of the other stored items", async () => {
    offerAnswer = () =>
      Response.json({
        ...OFFERED,
        removes: { ...NO_REMOVALS, gitCredentials: 2, imageUpdateIgnores: 4, janitorKeeps: 5 },
      });
    const dialog = await openDialog();
    expect(within(dialog).getByText("2 Git credentials")).toBeTruthy();
    expect(within(dialog).getByText("4 image update ignores")).toBeTruthy();
    expect(within(dialog).getByText("5 items kept from the janitor")).toBeTruthy();
    expect(within(dialog).queryByText(/stack/)).toBeNull();
  });

  const DOCS_HREF =
    "https://github.com/ianglennon/tierplan/blob/main/docs/install-in-swarm.md#fold-a-host-that-has-ingress-set-up";

  it("refuses for ingress with what exists, the order, the docs link and no fold button", async () => {
    offerAnswer = () =>
      Response.json({
        offered: false,
        refusal: "fold_ingress_active",
        ingress: { domains: 2, reserved: 1, hostnames: 0, routes: 0, enabled: false },
      });
    show();
    const card = await screen.findByRole("region", { name: "Local host" });
    expect(card.textContent).toContain(
      "Ingress is set up on the Local environment (2 domains, 1 reserved name), and folding would delete it. To fold this host, first remove that environment's domains and reserved names. Then fold, and register them on edge swarm afterwards. The install-in-swarm guide in the docs shows how.",
    );
    const link = within(card).getByRole("link", { name: "install-in-swarm guide" });
    expect(link.getAttribute("href")).toBe(DOCS_HREF);
    expect(screen.queryByRole("button", { name: /Fold into/ })).toBeNull();
  });

  it("adds turning ingress off when it is on, and never lists a version counter", async () => {
    offerAnswer = () =>
      Response.json({
        offered: false,
        refusal: "fold_ingress_active",
        ingress: { domains: 1, reserved: 0, hostnames: 0, routes: 0, enabled: true },
      });
    show();
    const card = await screen.findByRole("region", { name: "Local host" });
    expect(card.textContent).toContain("(1 domain, ingress turned on)");
    expect(card.textContent).toContain("reserved names, and turn ingress off. Then fold");
    expect(card.textContent).not.toMatch(/version/i);
  });

  it("refuses when the swarm does not list this host, with no fold button", async () => {
    offerAnswer = () => Response.json({ offered: false, refusal: "fold_node_not_listed" });
    show();
    const card = await screen.findByRole("region", { name: "Local host" });
    expect(card.textContent).toContain(
      "edge swarm doesn't list this host as one of its nodes, or its managers don't match this host's.",
    );
    expect(screen.queryByRole("button", { name: /Fold into/ })).toBeNull();
  });

  it("keeps the dialog open with the node-not-listed alert and the confirm usable again", async () => {
    answer = () =>
      Response.json(
        { title: "Conflict", status: 409, type: "fold_node_not_listed" },
        { status: 409 },
      );
    const dialog = await openDialog();
    tick(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    const alert = await failureAlert(dialog);
    expect(alert.textContent).toContain(
      "edge swarm doesn't list this host as one of its nodes, or its managers don't match this host's. Check that the swarm's agents are connected, then try again. Nothing was changed.",
    );
    expect(
      (within(dialog).getByRole("button", { name: "Fold into edge swarm" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });

  it("keeps the dialog open on fold_ingress_active, hides the confirm and refreshes the offer", async () => {
    answer = () =>
      Response.json(
        { title: "Conflict", status: 409, type: "fold_ingress_active" },
        { status: 409 },
      );
    const dialog = await openDialog();
    const reads = () => sent.filter((s) => s.path.endsWith("/fold-offer")).length;
    const before = reads();
    // The refreshed offer carries what was found.
    offerAnswer = () =>
      Response.json({
        offered: false,
        refusal: "fold_ingress_active",
        ingress: { domains: 2, reserved: 1, hostnames: 0, routes: 0, enabled: false },
      });
    tick(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    const alert = await failureAlert(dialog);
    await waitFor(() => expect(reads()).toBeGreaterThan(before));
    await waitFor(() => expect(alert.textContent).toContain("(2 domains, 1 reserved name)"));
    expect(alert.textContent).toContain("Nothing was changed.");
    expect(within(alert).getByRole("link", { name: "install-in-swarm guide" })).toBeTruthy();
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: "Fold into edge swarm" })).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep this host as it is" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    // The card now shows the same refusal.
    expect(
      within(screen.getByRole("region", { name: "Local host" })).getByRole("link", {
        name: "install-in-swarm guide",
      }),
    ).toBeTruthy();
  });

  it("is not rendered when the offer is on a local environment and no swarm environment offers one", async () => {
    listed = [LOCAL];
    queryClient.setQueryData(environmentsQuery.queryKey, { items: [LOCAL] });
    show(LOCAL);
    await act(async () => {});
    expect(screen.queryByRole("button", { name: /Fold into/ })).toBeNull();
  });

  it("opens Fold this host into {swarm}? with the UI-SPEC body, the keep button and the primary confirm, and posts nothing yet", async () => {
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog", { name: "Fold this host into edge swarm?" });
    expect(dialog.textContent).toContain(
      "TierPlan will reach this node through the Docker socket from now on.",
    );
    expect(dialog.textContent).toContain(
      "The agent on this node keeps running, but TierPlan won't use it. It stays as a standby.",
    );
    expect(dialog.textContent).toContain("The separate Local environment is removed.");
    expect(dialog.textContent).toContain(
      "Nothing changes on the host. This is recorded in the audit log.",
    );
    expect(within(dialog).getByRole("button", { name: "Keep this host as it is" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Fold into edge swarm" })).toBeTruthy();
    expect(posts()).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep this host as it is" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posts()).toEqual([]);
  });

  it("posts once however often confirm is clicked, shows the spinner, reads the environments and the nodes again, closes and says it is done", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    const onFolded = show();
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog", { name: "Fold this host into edge swarm?" });
    tick(dialog);
    const confirm = within(dialog).getByRole("button", { name: "Fold into edge swarm" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() =>
      expect(within(dialog).getByRole("button", { name: "Folding…" })).toBeTruthy(),
    );
    expect(
      (within(dialog).getByRole("button", { name: "Keep this host as it is" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    expect(posts()).toEqual([
      { method: "POST", path: "/api/environments/e2/fold-local", csrf: "csrf-1" },
    ]);

    await act(async () => release(Response.json({})));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const keys = invalidate.mock.calls.map((c) => JSON.stringify(c[0]?.queryKey));
    expect(keys).toContain(JSON.stringify(["environments"]));
    expect(keys).toContain(JSON.stringify(["agents", "e2"]));
    expect(keys).toContain(JSON.stringify(["fold-offer", "e2"]));
    expect(keys).toContain(JSON.stringify(["local-host"]));
    expect(
      await screen.findByText("edge swarm now reaches this host through the Docker socket."),
    ).toBeTruthy();
    expect(onFolded).toHaveBeenCalledWith("e2");
    expect(posts()).toHaveLength(1);
  });

  it("from the local host's own page names the swarm and posts to the swarm environment", async () => {
    show(LOCAL);
    expect(
      await screen.findByText(
        "This TierPlan host is a node in edge swarm. TierPlan can reach it through the Docker socket, so it needs no separate environment.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog");
    tick(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe("/api/environments/e2/fold-local");
  });

  it("keeps the dialog open with an inline alert when the fold is no longer offered, and reads the environments again", async () => {
    answer = () =>
      Response.json(
        { title: "Conflict", status: 409, type: "fold_not_offered", detail: "no longer offered" },
        { status: 409 },
      );
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog");
    tick(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    const alert = await failureAlert(dialog);
    expect(alert.textContent).toContain(
      "This host is no longer a node of that swarm, or it was folded a moment ago. Nothing was changed.",
    );
    expect(screen.getByRole("dialog")).toBeTruthy();
    await waitFor(() =>
      expect(invalidate.mock.calls.map((c) => JSON.stringify(c[0]?.queryKey))).toContain(
        JSON.stringify(["environments"]),
      ),
    );
    // The confirm is usable again, for another try.
    expect(
      (within(dialog).getByRole("button", { name: "Fold into edge swarm" }) as HTMLButtonElement)
        .disabled,
    ).toBe(false);
  });

  it("says Couldn't fold this host for any other failure", async () => {
    answer = () => Response.json({ title: "Internal", status: 500 }, { status: 500 });
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Fold into edge swarm" }));
    const dialog = await screen.findByRole("dialog");
    tick(dialog);
    fireEvent.click(within(dialog).getByRole("button", { name: "Fold into edge swarm" }));
    const alert = await failureAlert(dialog);
    expect(alert.textContent).toContain("Couldn't fold this host. Try again.");
  });
});
