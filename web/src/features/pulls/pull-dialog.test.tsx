// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { PullItem, PullJob } from "@/api/pulls";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { PullDialog } from "@/features/pulls/pull-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const REF = "docker.io/library/nginx:1.27";
const DIGEST = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;

const started = (nodes = 1) =>
  Response.json(
    {
      jobId: "j1",
      ref: REF,
      credential: null,
      nodes: Array.from({ length: nodes }, (_, i) => ({
        nodeId: `n${i + 1}`,
        name: `node-${i + 1}`,
      })),
    },
    { status: 202 },
  );
const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

const item = (over: Partial<PullItem> = {}): PullItem => ({
  nodeId: "n1",
  name: "node-1",
  state: "waiting",
  code: "",
  message: "",
  current: 0,
  total: 0,
  unit: "",
  done: 0,
  count: 0,
  layers: [],
  digest: "",
  upToDate: false,
  ...over,
});

const job = (items: PullItem[], state: PullJob["state"] = "running"): PullJob => ({
  id: "j1",
  envId: ENV,
  ref: REF,
  credential: null,
  state,
  createdAt: "2026-10-11T10:00:00Z",
  finishedAt: state === "running" ? "" : "2026-10-11T10:01:00Z",
  items,
});

beforeEach(() => {
  sent = [];
  answer = () => started();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        method: req.method,
        path: new URL(req.url).pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (entry.path === `/api/env/${ENV}/images/pull` && req.method === "POST")
        return answer(entry);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

const posts = () => sent.filter((s) => s.method === "POST");

function mount(props: Partial<Parameters<typeof PullDialog>[0]> = {}) {
  const onOpenChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <PullDialog
            envId={ENV}
            open
            onOpenChange={onOpenChange}
            hostName="Home lab"
            swarm={false}
            {...props}
          />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onOpenChange };
}

const source = () => FakeEventSource.instances[0] as FakeEventSource;

async function submit(ref: string) {
  const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
  fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: ref } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
  return dialog;
}

describe("PullDialog (the form)", () => {
  it("opens with the image field, its helper and where it pulls, and sends nothing", async () => {
    mount();
    const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
    expect(dialog.className).toContain("sm:max-w-160");
    expect(dialog.className).toContain("max-h-[90svh]");
    expect(dialog.textContent).toContain("No tag means latest.");
    expect(dialog.textContent).toContain("Pulls on Home lab.");
    const field = within(dialog).getByLabelText("Image") as HTMLInputElement;
    expect(field.className).toContain("font-mono");
    expect(field.getAttribute("placeholder")).toBe("nginx:1.27 or ghcr.io/acme/app:latest");
    expect(posts()).toEqual([]);
  });

  it("says it pulls on every node of a swarm", async () => {
    mount({ swarm: true, hostName: "Cluster" });
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain("Pulls on every node of Cluster.");
  });

  it("prefills the image from a previous try", async () => {
    mount({ initialRef: "redis:8" });
    const dialog = await screen.findByRole("dialog");
    expect((within(dialog).getByLabelText("Image") as HTMLInputElement).value).toBe("redis:8");
  });

  it("asks for an image and sends nothing when the field is empty", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    expect(
      await within(dialog).findByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
    ).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("shows Starting… with a spinner while the request is out, then swaps to the progress panel and focuses its heading", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((r) => (release = r));
    mount();
    const dialog = await submit("nginx:1.27");
    const button = await within(dialog).findByRole("button", { name: "Starting…" });
    expect(button.hasAttribute("disabled") || button.getAttribute("aria-disabled") === "true").toBe(
      true,
    );
    expect(within(dialog).getByRole("status", { name: "Loading" })).toBeTruthy();
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.body).toEqual({ ref: "nginx:1.27" });
    release(started());
    const heading = await within(dialog).findByRole("heading", { name: `Pulling ${REF}` });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(within(dialog).queryByLabelText("Image")).toBeNull();
    expect(within(dialog).getAllByText("Waiting")).toHaveLength(1);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(source().url).toContain("/api/pulls/j1/stream");
  });

  it("does not post twice on a double click", async () => {
    answer = () => new Promise<Response>(() => {});
    mount();
    const dialog = await submit("nginx");
    const button = await within(dialog).findByRole("button", { name: "Starting…" });
    fireEvent.click(button);
    expect(posts()).toHaveLength(1);
  });

  it("shows the field error for a reference the server did not accept, and keeps the form", async () => {
    answer = () => problem(422, "ref_invalid");
    mount();
    const dialog = await submit("Bad Ref!");
    expect(
      await within(dialog).findByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
    ).toBeTruthy();
    expect(within(dialog).getByLabelText("Image")).toBeTruthy();
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("shows an inline alert when the user already has five pulls running", async () => {
    answer = () => problem(429, "too_many_pulls");
    mount();
    const dialog = await submit("nginx");
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("You already have 5 pulls running. Wait for one to finish.");
  });

  it("words a lost permission, a read-only host and an unreachable node, and shows the agent notice for a policy refusal", async () => {
    for (const [status, type, text] of [
      [403, "forbidden", "You no longer have permission to do that."],
      [409, "host_read_only", "This host is read-only (set on the host)."],
      [503, "node_unavailable", "The node isn't connected to Bayplan right now."],
      [500, "oops", "Couldn't start the pull. Try again."],
    ] as const) {
      answer = () => problem(status, type);
      mount();
      const dialog = await submit("nginx");
      expect((await within(dialog).findByRole("alert")).textContent).toContain(text);
      cleanup();
    }
    answer = () => problem(502, "agent_policy_denied");
    mount();
    const dialog = await submit("nginx");
    expect(await within(dialog).findByText(/older than this Bayplan|agent/i)).toBeTruthy();
  });

  it("closes through Cancel without sending anything", async () => {
    const { onOpenChange } = mount();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(posts()).toEqual([]);
  });
});

describe("PullDialog (the progress)", () => {
  async function startedPull(nodes = 1) {
    answer = () => started(nodes);
    mount();
    const dialog = await submit("nginx:1.27");
    await within(dialog).findByRole("heading", { name: `Pulling ${REF}` });
    return dialog;
  }

  it("follows the stream: a snapshot, a moving bar and a Done row with its digest", async () => {
    const dialog = await startedPull();
    act(() => source().sendFrame("snapshot", job([item()])));
    act(() =>
      source().sendFrame(
        "item",
        item({ state: "running", current: 512, total: 1024, unit: "bytes", done: 0, count: 1 }),
        "2",
      ),
    );
    const bar = within(dialog).getByRole("progressbar", { name: "Pull progress on node-1" });
    expect(bar.getAttribute("aria-valuenow")).toBe("50");
    expect(dialog.textContent).toContain("512 B of 1 KiB");
    act(() =>
      source().sendFrame(
        "done",
        job(
          [item({ state: "done", digest: DIGEST, current: 1024, total: 1024, done: 1, count: 1 })],
          "done",
        ),
        "3",
      ),
    );
    expect(await within(dialog).findByRole("heading", { name: `Pulled ${REF}` })).toBeTruthy();
    expect(within(dialog).getByText("Done")).toBeTruthy();
    expect(within(dialog).getByText("Digest sha256:aaaaaaaaaaaa")).toBeTruthy();
    expect(source().closed).toBe(true);
  });

  it("toasts Pulled {ref}. when a one-node pull finishes, once", async () => {
    await startedPull();
    act(() => source().sendFrame("snapshot", job([item()])));
    act(() =>
      source().sendFrame("done", job([item({ state: "done", digest: DIGEST })], "done"), "3"),
    );
    expect(await screen.findByText(`Pulled ${REF}.`)).toBeTruthy();
    expect(screen.getAllByText(`Pulled ${REF}.`)).toHaveLength(1);
  });

  it("toasts the node count for a pull on several nodes", async () => {
    await startedPull(3);
    act(() =>
      source().sendFrame(
        "done",
        job(
          ["n1", "n2", "n3"].map((id, i) =>
            item({ nodeId: id, name: `node-${i + 1}`, state: "done", digest: DIGEST }),
          ),
          "done",
        ),
        "5",
      ),
    );
    expect(await screen.findByText(`Pulled ${REF} on 3 nodes.`)).toBeTruthy();
  });

  it("toasts a partial result with Details, and a total failure with the reason", async () => {
    await startedPull(2);
    act(() =>
      source().sendFrame(
        "done",
        job(
          [
            item({ state: "done", digest: DIGEST }),
            item({ nodeId: "n2", name: "node-2", state: "failed", code: "pull_denied" }),
          ],
          "partial",
        ),
        "5",
      ),
    );
    expect(await screen.findByText("Pulled on 1, 1 failed")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Details" })).toBeTruthy();
  });

  it("toasts the mapped reason when the only node failed, and offers Try again, which opens the form with the reference", async () => {
    const dialog = await startedPull();
    act(() =>
      source().sendFrame(
        "done",
        job([item({ state: "failed", code: "image_not_found" })], "failed"),
        "5",
      ),
    );
    expect(
      await screen.findAllByText(
        `Couldn't pull ${REF}. ${REF} wasn't found on docker.io. Check the name and the tag.`,
      ),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Try again" }));
    const field = (await within(dialog).findByLabelText("Image")) as HTMLInputElement;
    expect(field.value).toBe("nginx:1.27");
    expect(posts()).toHaveLength(1);
  });

  it("keeps following the pull when the dialog is closed", async () => {
    answer = () => started();
    const onOpenChange = vi.fn();
    const { rerender } = render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <PullDialog
              envId={ENV}
              open
              onOpenChange={onOpenChange}
              hostName="Home lab"
              swarm={false}
            />
            <Toaster />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    const dialog = await submit("nginx:1.27");
    await within(dialog).findByRole("heading", { name: `Pulling ${REF}` });
    rerender(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <PullDialog
              envId={ENV}
              open={false}
              onOpenChange={onOpenChange}
              hostName="Home lab"
              swarm={false}
            />
            <Toaster />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    expect(source().closed).toBe(false);
    act(() =>
      source().sendFrame("done", job([item({ state: "done", digest: DIGEST })], "done"), "3"),
    );
    expect(await screen.findByText(`Pulled ${REF}.`)).toBeTruthy();
  });
});
