// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { PullItem, PullJob } from "@/api/pulls";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { PREVIEW_DEBOUNCE_MS, PullDialog, PullImageButton } from "@/features/pulls/pull-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { settled, waitUnderFakeClock } from "@/test-settle";

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
let targetsAnswer: () => Response | Promise<Response>;
let cancelAnswer: () => Response | Promise<Response>;
let resolveAnswer: (req: Sent) => Response | Promise<Response>;
// The tag picker's reads: the tag list of an environment's route, and the details of its rows.
let tagsAnswer: (req: Sent) => Response | Promise<Response>;
// The reference each tag list was asked for.
let tagRefs: string[];
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

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
const target = (id: string, name: string, over: Record<string, unknown> = {}) => ({
  nodeId: id,
  name,
  role: "worker",
  arch: "amd64",
  readOnly: false,
  reachable: true,
  ...over,
});
const SWARM_TARGETS = () =>
  Response.json({
    swarm: true,
    nodes: [
      target("n1", "node-1", { role: "manager" }),
      target("n2", "node-2", { arch: "arm64" }),
      target("n3", "node-3", { reachable: false }),
    ],
  });
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
  tagRefs = [];
  me = makeMe({ envs: { [ENV]: "operator" } });
  answer = () => started();
  targetsAnswer = () => Response.json({}, { status: 404 });
  cancelAnswer = () =>
    Response.json(job([item({ state: "cancelled", code: "cancelled" })], "cancelled"));
  resolveAnswer = () => Response.json({}, { status: 404 });
  tagsAnswer = () =>
    Response.json({
      repo: "library/nginx",
      host: "docker.io",
      credential: null,
      tags: [{ name: "1.27" }, { name: "1.26" }, { name: "latest" }],
      hidden: 0,
      total: 3,
      more: false,
      page: 0,
      capped: false,
      limit: 10000,
    });
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
        body:
          req.method === "POST"
            ? await req
                .clone()
                .json()
                .catch(() => undefined)
            : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path === `/api/env/${ENV}/images/pull` && req.method === "POST")
        return answer(entry);
      if (entry.path === `/api/env/${ENV}/images/pull-targets`) return targetsAnswer();
      if (entry.path === "/api/pulls/j1/cancel" && req.method === "POST") return cancelAnswer();
      if (/^\/api\/env\/[^/]+\/registries\/resolve$/.test(entry.path) && req.method === "POST")
        return resolveAnswer(entry);
      if (entry.path === "/api/environments")
        return Response.json({
          items: [
            { id: "e1", name: "Lab host", envType: "standalone", enabled: true },
            { id: "e2", name: "Edge swarm", envType: "swarm", enabled: true },
            { id: "e3", name: "Read only", envType: "standalone", enabled: true },
          ],
          total: 3,
        });
      if (entry.path === "/api/env/e1/images/tags") {
        tagRefs.push(new URL(req.url).searchParams.get("ref") ?? "");
        return tagsAnswer(entry);
      }
      if (entry.path === "/api/env/e1/images/tags/details")
        return Response.json({
          items: ((entry.body?.tags as string[]) ?? []).map((tag) => ({
            tag,
            digest: DIGEST,
            // One date for every tag: the browser puts a batch newest first, and equal dates
            // keep the order the list gave.
            created: "2026-10-12T10:00:00Z",
            platforms: [{ os: "linux", arch: "amd64", variant: "", digest: DIGEST, size: 1024 }],
            code: "",
            runningIn: [],
            missing: [],
          })),
        });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

// The pulls started; the credential preview's checks are POSTs too, and not what is counted.
const posts = () =>
  sent.filter((s) => s.method === "POST" && !s.path.endsWith("/registries/resolve"));
const resolves = () => sent.filter((s) => s.path.endsWith("/registries/resolve"));
const pullPosts = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/images/pull"));

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

// The stream is opened by an effect, which React may run just after the panel is on screen.
const streamOpen = () => waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

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

  it("says it pulls on every node of a swarm whose nodes could not be listed, and sends no choice", async () => {
    mount({ swarm: true, hostName: "Cluster" });
    const dialog = await screen.findByRole("dialog");
    await waitFor(() => expect(dialog.textContent).toContain("Pulls on every node of Cluster."));
    expect(within(dialog).queryByRole("radio")).toBeNull();
    fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    await within(dialog).findByRole("heading", { name: /Pulling/ });
    expect(posts()[0]?.body).toEqual({ ref: "nginx" });
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
    expect(dialog.querySelector('[data-slot="spinner"]')).toBeTruthy();
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.body).toEqual({ ref: "nginx:1.27" });
    release(started());
    const heading = await within(dialog).findByRole("heading", { name: `Pulling ${REF}` });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(within(dialog).queryByLabelText("Image")).toBeNull();
    expect(within(dialog).getAllByText("Waiting")).toHaveLength(1);
    await streamOpen();
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

  it("shows tag_invalid as an error on the Image field and keeps the form", async () => {
    answer = () => problem(422, "tag_invalid");
    mount();
    const dialog = await submit("nginx:1.27");
    expect(
      await within(dialog).findByText(/A tag is 1 to 128 letters, digits, underscores/),
    ).toBeTruthy();
    expect(within(dialog).getByLabelText("Image")).toBeTruthy();
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
      [503, "node_unavailable", "The node isn't connected to Coaming right now."],
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
    expect(await within(dialog).findByText(/older than this Coaming|agent/i)).toBeTruthy();
  });

  it("ignores Escape and Cancel while the request is out, and still tracks the pull when it answers (WR-C01)", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((r) => (release = r));
    const { onOpenChange } = mount();
    const dialog = await submit("nginx:1.27");
    await within(dialog).findByRole("button", { name: "Starting…" });
    fireEvent.keyDown(dialog, { key: "Escape" });
    const cancel = within(dialog).getByRole("button", { name: "Cancel" });
    expect((cancel as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(cancel);
    expect(onOpenChange).not.toHaveBeenCalled();
    release(started());
    await within(dialog).findByRole("heading", { name: `Pulling ${REF}` });
    await streamOpen();
    expect(source().url).toContain("/api/pulls/j1/stream");
    // Once the request is over the dialog closes as usual.
    fireEvent.keyDown(dialog, { key: "Escape" });
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
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
    await streamOpen();
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

  it("cancels a running pull once, with the CSRF header, and says how many nodes it stopped", async () => {
    setCsrfToken("csrf-1");
    cancelAnswer = () =>
      Response.json(
        job(
          [
            item({ state: "done", digest: DIGEST }),
            item({ nodeId: "n2", name: "node-2", state: "cancelled", code: "cancelled" }),
            item({ nodeId: "n3", name: "node-3", state: "cancelled", code: "cancelled" }),
          ],
          "partial",
        ),
      );
    const dialog = await startedPull();
    act(() => source().sendFrame("snapshot", job([item({ state: "running" })])));
    const cancel = await within(dialog).findByRole("button", { name: "Cancel pull" });
    fireEvent.click(cancel);
    fireEvent.click(cancel);
    expect(await screen.findByText("Cancelled the pull on 2 nodes.")).toBeTruthy();
    expect(posts().filter((p) => p.path === "/api/pulls/j1/cancel")).toHaveLength(1);
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
    await streamOpen();
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

describe("PullDialog (where it pulls)", () => {
  const radio = (dialog: HTMLElement, name: RegExp | string) =>
    within(dialog).getByRole("radio", { name });

  async function swarmDialog() {
    targetsAnswer = SWARM_TARGETS;
    mount({ swarm: true, hostName: "Cluster" });
    const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
    await within(dialog).findByRole("radio", { name: "All nodes (3)" });
    return dialog;
  }

  it("shows All nodes (n) checked on a swarm, with the chooser hidden, and sends no choice for it", async () => {
    const dialog = await swarmDialog();
    expect(radio(dialog, "All nodes (3)").getAttribute("aria-checked")).toBe("true");
    expect(radio(dialog, "Choose nodes").getAttribute("aria-checked")).toBe("false");
    expect(within(dialog).queryAllByRole("checkbox")).toHaveLength(0);
    fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    await within(dialog).findByRole("heading", { name: /Pulling/ });
    expect(posts()[0]?.body).toEqual({ ref: "nginx" });
  });

  it("reveals the chooser for Choose nodes, with each node's role and architecture and the count", async () => {
    const dialog = await swarmDialog();
    fireEvent.click(radio(dialog, "Choose nodes"));
    expect(within(dialog).getAllByRole("checkbox")).toHaveLength(3);
    expect(within(dialog).getByText("Manager")).toBeTruthy();
    expect(within(dialog).getByText("arm64")).toBeTruthy();
    expect(within(dialog).getByText("0 of 3 selected")).toBeTruthy();
    const down = within(dialog).getByRole("checkbox", { name: /node-3/ });
    expect(down.hasAttribute("disabled")).toBe(true);
    expect(dialog.textContent).toContain("node-3 isn't reachable right now.");
  });

  it("asks for at least one node and sends nothing when none is chosen", async () => {
    const dialog = await swarmDialog();
    fireEvent.click(radio(dialog, "Choose nodes"));
    fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    expect(await within(dialog).findByText("Choose at least one node.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("sends the chosen node IDs only when Choose nodes is on", async () => {
    const dialog = await swarmDialog();
    fireEvent.click(radio(dialog, "Choose nodes"));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /node-2/ }));
    expect(within(dialog).getByText("1 of 3 selected")).toBeTruthy();
    fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    await within(dialog).findByRole("heading", { name: /Pulling/ });
    expect(posts()[0]?.body).toEqual({ ref: "nginx", nodeIds: ["n2"] });
  });

  it("goes back to every node when All nodes is chosen again", async () => {
    const dialog = await swarmDialog();
    fireEvent.click(radio(dialog, "Choose nodes"));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /node-2/ }));
    fireEvent.click(radio(dialog, "All nodes (3)"));
    fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    await within(dialog).findByRole("heading", { name: /Pulling/ });
    expect(posts()[0]?.body).toEqual({ ref: "nginx" });
  });

  it("shows one line and no chooser on a standalone host", async () => {
    targetsAnswer = () => Response.json({ swarm: false, nodes: [target("e1", "Home lab")] });
    mount({ swarm: false });
    const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
    await waitFor(() => expect(dialog.textContent).toContain("Pulls on Home lab."));
    expect(within(dialog).queryByRole("radio")).toBeNull();
    expect(within(dialog).queryByRole("checkbox")).toBeNull();
  });

  it("words a node choice the server refused, and keeps the form", async () => {
    const dialog = await swarmDialog();
    for (const [type, text] of [
      ["nodes_unknown", "isn't part of this environment"],
      ["nodes_required", "Choose at least one node."],
    ] as const) {
      answer = () => problem(422, type);
      fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: "nginx" } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
      await waitFor(() => expect(within(dialog).getByRole("alert").textContent).toContain(text));
    }
    expect(within(dialog).getByLabelText("Image")).toBeTruthy();
  });

  it("toasts Pulled on 4, 1 failed with Details when one of five nodes failed", async () => {
    answer = () => started(5);
    mount();
    const dialog = await submit("nginx:1.27");
    await within(dialog).findByRole("heading", { name: `Pulling ${REF}` });
    await streamOpen();
    act(() =>
      source().sendFrame(
        "done",
        job(
          [1, 2, 3, 4, 5].map((n) =>
            item({
              nodeId: `n${n}`,
              name: `node-${n}`,
              state: n === 5 ? "failed" : "done",
              code: n === 5 ? "pull_denied" : "",
              digest: n === 5 ? "" : DIGEST,
            }),
          ),
          "partial",
        ),
        "9",
      ),
    );
    expect(await screen.findByText("Pulled on 4, 1 failed")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Details" })).toBeTruthy();
  });
});

describe("PullImageButton", () => {
  function mountButton(role: "viewer" | "operator") {
    me = makeMe({ envs: { [ENV]: role } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <PullImageButton envId={ENV} hostName="Home lab" swarm={false} />
            <Toaster />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  const pullButton = () => screen.getByRole("button", { name: "Pull image" });

  it("is the accent button for an Operator and opens the dialog", async () => {
    mountButton("operator");
    expect(
      (await screen.findByRole("button", { name: "Pull image" })).getAttribute("data-variant"),
    ).toBe("default");
    // The permission arrives with /api/me; a click before it does nothing.
    await waitFor(() => {
      fireEvent.click(pullButton());
      expect(screen.getByRole("dialog", { name: "Pull an image" })).toBeTruthy();
    });
    expect(pullButton().getAttribute("aria-disabled")).toBeNull();
  });

  it("is unavailable with Needs Operator for a Viewer, and opens nothing", async () => {
    mountButton("viewer");
    await waitFor(() => expect(pullButton().getAttribute("aria-disabled")).toBe("true"));
    expect(pullButton().getAttribute("aria-describedby")).toBeTruthy();
    expect(document.body.textContent).toContain("Needs Operator");
    fireEvent.click(pullButton());
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(await screen.findAllByText("Needs Operator")).toBeTruthy();
  });

  it("is unavailable and says the host is read-only when every node is read-only", async () => {
    targetsAnswer = () =>
      Response.json({
        swarm: true,
        nodes: [
          target("n1", "node-1", { readOnly: true }),
          target("n2", "node-2", { readOnly: true }),
        ],
      });
    mountButton("operator");
    await waitFor(() => expect(pullButton().getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("This host is read-only (set on the host)");
    fireEvent.click(pullButton());
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("stays available when only some nodes are read-only", async () => {
    targetsAnswer = () =>
      Response.json({
        swarm: true,
        nodes: [target("n1", "node-1", { readOnly: true }), target("n2", "node-2")],
      });
    mountButton("operator");
    await screen.findByRole("button", { name: "Pull image" });
    await waitFor(() => {
      fireEvent.click(pullButton());
      expect(screen.getByRole("dialog", { name: "Pull an image" })).toBeTruthy();
    });
  });
});

// What the pull will use, before it starts (plan 05-08, IMG-02, D-10): the same resolver
// the Registries page tester asks, 400 ms after the last keystroke.
describe("PullDialog (the credential preview)", () => {
  const resolved = (over: Record<string, unknown> = {}) =>
    Response.json({
      input: "nginx",
      normalized: "docker.io/library/nginx:latest",
      credential: null,
      matchedOn: "",
      considered: [],
      ...over,
    });
  const hub = {
    id: "h",
    name: "hub-all",
    scope: { kind: "instance", envId: null, envName: null },
    host: "docker.io",
    prefix: "",
  };
  const openDialog = () => screen.findByRole("dialog", { name: "Pull an image" });
  const typeInto = (dialog: HTMLElement, ref: string) =>
    fireEvent.change(within(dialog).getByLabelText("Image"), { target: { value: ref } });
  const type = async (ref: string) => {
    const dialog = await openDialog();
    typeInto(dialog, ref);
    return dialog;
  };
  // The preview is debounced by a timer of the component's own. A test about "nothing more is
  // asked" runs it on a fully fake clock: real time passing between two keystrokes must not be
  // able to fire the debounce, or "asked once, for what was typed last" would hold only on a
  // quiet machine. The dialog is found before the clock is faked, the keystrokes are synchronous,
  // and the test moves the clock past the debounce when it means to. Under it the test waits with
  // vi.waitFor, because Testing Library's waitFor and findBy poll on the faked timers and so never
  // poll.
  const debounceClock = () => vi.useFakeTimers();
  // settled(client) for a fake clock: no query is fetching and no mutation is running.
  const noneRunning = () =>
    waitUnderFakeClock(() => expect(client.isFetching() + client.isMutating()).toBe(0));

  it("shows the reference as it will be pulled and says the pull is anonymous", async () => {
    resolveAnswer = () => resolved();
    mount();
    const dialog = await type("nginx");
    expect(resolves()).toHaveLength(0); // not before the 400 ms are up
    await waitFor(() =>
      expect(dialog.textContent).toContain("Pulls docker.io/library/nginx:latest"),
    );
    expect(dialog.textContent).toContain("Credential: anonymous. No stored credential matches.");
    expect(resolves()).toEqual([
      { method: "POST", path: `/api/env/${ENV}/registries/resolve`, body: { ref: "nginx" } },
    ]);
    expect(posts()).toEqual([]);
  });

  it("names the credential the pull will use and where it applies", async () => {
    resolveAnswer = () => resolved({ credential: hub, matchedOn: "hub_fallback" });
    mount();
    const dialog = await type("nginx");
    await waitFor(() => expect(dialog.textContent).toContain("Credential: “hub-all” (Instance)"));
    resolveAnswer = () =>
      resolved({
        credential: {
          ...hub,
          name: "team",
          scope: { kind: "environment", envId: ENV, envName: "Lab" },
        },
        matchedOn: "prefix",
      });
    await type("nginx2");
    await waitFor(() => expect(dialog.textContent).toContain("Credential: “team” (Lab)"));
  });

  it("asks once, for what was typed last, after the typing stops", async () => {
    resolveAnswer = () => resolved();
    mount();
    const dialog = await openDialog();
    debounceClock();
    typeInto(dialog, "n");
    typeInto(dialog, "ng");
    typeInto(dialog, "nginx:1.27");
    await vi.advanceTimersByTimeAsync(PREVIEW_DEBOUNCE_MS);
    await waitUnderFakeClock(() => expect(resolves()).toHaveLength(1));
    expect(resolves()[0]?.body).toEqual({ ref: "nginx:1.27" });
    // Nothing more comes after it, however long the debounce runs on.
    await vi.advanceTimersByTimeAsync(PREVIEW_DEBOUNCE_MS * 2);
    await noneRunning();
    expect(resolves()).toHaveLength(1);
    expect(dialog).toBeTruthy();
  });

  it("checks a prefilled reference without waiting for a keystroke", async () => {
    resolveAnswer = () => resolved({ input: "redis:8", normalized: "docker.io/library/redis:8" });
    mount({ initialRef: "redis:8" });
    const dialog = await screen.findByRole("dialog");
    await waitFor(() => expect(dialog.textContent).toContain("Pulls docker.io/library/redis:8"));
  });

  it("shows neither line for a reference the server does not understand, nor for an empty field", async () => {
    resolveAnswer = () =>
      Response.json({ type: "ref_invalid", title: "x", status: 422, detail: "d" }, { status: 422 });
    mount();
    const dialog = await openDialog();
    debounceClock();
    typeInto(dialog, "Bad Ref!");
    await vi.advanceTimersByTimeAsync(PREVIEW_DEBOUNCE_MS);
    await waitUnderFakeClock(() => expect(resolves()).toHaveLength(1));
    // The check has been answered (with the 422) before the lines are looked for.
    await noneRunning();
    expect(dialog.textContent).not.toContain("Credential:");
    expect(dialog.textContent).not.toContain("Pulls docker.io");
    // Clearing the field asks nothing.
    typeInto(dialog, "");
    await vi.advanceTimersByTimeAsync(PREVIEW_DEBOUNCE_MS * 2);
    await noneRunning();
    expect(resolves()).toHaveLength(1);
  });

  it("shows nothing when the check fails, and the pull can still be started", async () => {
    resolveAnswer = () => Response.json({ title: "boom", status: 500 }, { status: 500 });
    mount();
    const dialog = await type("nginx");
    await waitFor(() => expect(resolves()).toHaveLength(1));
    // The check has been answered (with the 500) before the line is looked for.
    await settled(client);
    expect(dialog.textContent).not.toContain("Credential:");
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    await within(dialog).findByRole("heading", { name: /Pulling/ });
    expect(posts()).toHaveLength(1);
  });
});

// Browse tags: the picker inside the form (plan 05-20, IMG-03, D-19).
describe("PullDialog (Browse tags)", () => {
  const field = async () => {
    const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
    return { dialog, input: within(dialog).getByLabelText("Image") as HTMLInputElement };
  };

  it("offers Browse tags only once the field holds a repository", async () => {
    mount();
    const { dialog, input } = await field();
    expect(within(dialog).queryByRole("button", { name: "Browse tags" })).toBeNull();
    fireEvent.change(input, { target: { value: "nginx" } });
    expect(within(dialog).getByRole("button", { name: "Browse tags" })).toBeTruthy();
    fireEvent.change(input, { target: { value: "  " } });
    expect(within(dialog).queryByRole("button", { name: "Browse tags" })).toBeNull();
  });

  it("opens the picker for the repository in the field, whatever tag it carries", async () => {
    mount();
    const { dialog, input } = await field();
    fireEvent.change(input, { target: { value: "nginx:1.27" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Browse tags" }));
    expect(await within(dialog).findByText("1.26")).toBeTruthy();
    expect(sent.some((s) => s.path === "/api/env/e1/images/tags" && s.method === "GET")).toBe(true);
    expect(
      within(dialog)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Tag", "Created", "Platforms"]);
    expect(within(dialog).getByRole("grid").closest(".max-h-80")).toBeTruthy();
  });

  it("writes the chosen tag into the field and updates the credential preview", async () => {
    resolveAnswer = (req) =>
      Response.json({
        input: String(req.body?.ref),
        normalized: `docker.io/library/${String(req.body?.ref)}`,
        credential: null,
        matchedOn: "",
        considered: [],
      });
    mount();
    const { dialog, input } = await field();
    fireEvent.change(input, { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Browse tags" }));
    fireEvent.click(await within(dialog).findByText("1.26"));
    expect(input.value).toBe("nginx:1.26");
    await waitFor(() => expect(dialog.textContent).toContain("Pulls docker.io/library/nginx:1.26"));
    expect(resolves().at(-1)?.body?.ref).toBe("nginx:1.26");
  });

  it("chooses with the keyboard: arrows move, Enter writes the tag", async () => {
    mount();
    const { dialog, input } = await field();
    fireEvent.change(input, { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Browse tags" }));
    await within(dialog).findByText("latest");
    // The rows settle once the details have come: they are put newest first when they do.
    await waitFor(() => expect(within(dialog).queryAllByText("Loading…")).toHaveLength(0));
    const rows = within(dialog).getAllByRole("row").slice(1);
    rows[0]?.focus();
    fireEvent.keyDown(rows[0] as HTMLElement, { key: "ArrowDown" });
    expect(document.activeElement).toBe(rows[1]);
    fireEvent.keyDown(rows[1] as HTMLElement, { key: "Enter" });
    expect(input.value).toBe("nginx:1.26");
    expect(rows[1]?.getAttribute("aria-selected")).toBe("true");
  });

  it("does not ask the registry about the previous repository's tags under the new one (WR-C03)", async () => {
    let release: (r: Response) => void = () => {};
    tagsAnswer = () =>
      tagRefs.at(-1) === "redis"
        ? new Promise<Response>((r) => (release = r))
        : Response.json({
            repo: "library/nginx",
            host: "docker.io",
            credential: null,
            tags: [{ name: "old-a" }, { name: "old-b" }],
            hidden: 0,
            total: 2,
            more: false,
            page: 0,
            capped: false,
            limit: 10000,
          });
    mount();
    const { dialog, input } = await field();
    fireEvent.change(input, { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Browse tags" }));
    await within(dialog).findByText("old-a");
    await waitFor(() => expect(within(dialog).queryAllByText("Loading…")).toHaveLength(0));
    const detailsFor = (repo: string) =>
      posts().filter((p) => p.path.endsWith("/tags/details") && p.body?.ref === repo);
    expect(detailsFor("nginx").length).toBeGreaterThan(0);

    fireEvent.change(input, { target: { value: "redis" } });
    // The picker follows the field once the typing stops; the new list is then in flight.
    await waitFor(() => expect(tagRefs).toContain("redis"));
    expect(within(dialog).queryByText("old-a")).toBeNull();
    expect(detailsFor("redis")).toEqual([]);

    release(
      Response.json({
        repo: "library/redis",
        host: "docker.io",
        credential: null,
        tags: [{ name: "7.4" }],
        hidden: 0,
        total: 1,
        more: false,
        page: 0,
        capped: false,
        limit: 10000,
      }),
    );
    await within(dialog).findByText("7.4");
    expect(
      detailsFor("redis").flatMap((p) => (p.body?.tags as string[] | undefined) ?? []),
    ).toEqual(["7.4"]);
  });

  it("starts the pull with the tag that was chosen", async () => {
    mount();
    const { dialog, input } = await field();
    fireEvent.change(input, { target: { value: "nginx" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Browse tags" }));
    fireEvent.click(await within(dialog).findByText("latest"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    await waitFor(() => expect(pullPosts()).toHaveLength(1));
    expect(pullPosts()[0]?.body).toEqual({ ref: "nginx:latest" });
  });
});

// Opened from the Registries page there is no environment yet (plan 05-20, D-24).
describe("PullDialog (Registries context)", () => {
  const environments = [
    { id: "e1", name: "Lab host", swarm: false },
    { id: "e2", name: "Edge swarm", swarm: true },
  ];
  const open = async () => {
    mount({ envId: undefined, environments, initialRef: "ghcr.io/acme/app:1.0" });
    const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
    return dialog;
  };

  it("has an Environment select above the image, with the reference prefilled", async () => {
    const dialog = await open();
    expect(within(dialog).getByRole("combobox", { name: "Environment" })).toBeTruthy();
    expect((within(dialog).getByLabelText("Image") as HTMLInputElement).value).toBe(
      "ghcr.io/acme/app:1.0",
    );
  });

  it("asks to choose an environment and sends nothing when none is chosen", async () => {
    const dialog = await open();
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    expect(await within(dialog).findByText("Choose an environment.")).toBeTruthy();
    expect(pullPosts()).toHaveLength(0);
  });

  it("pulls on the environment that was chosen", async () => {
    const dialog = await open();
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Environment" }));
    fireEvent.click(await screen.findByRole("option", { name: "Lab host" }));
    expect(await within(dialog).findByText("Pulls on Lab host.")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Pull image" }));
    await waitFor(() => expect(pullPosts()).toHaveLength(1));
    expect(pullPosts()[0]?.path).toBe("/api/env/e1/images/pull");
    expect(pullPosts()[0]?.body).toEqual({ ref: "ghcr.io/acme/app:1.0" });
  });
});
