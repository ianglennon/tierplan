// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CreateVolumeDialog } from "@/features/volumes/create-volume-dialog";
import { VolumeList } from "@/features/volumes/volume-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let volumes: unknown[];
let swarmEnv: boolean;

const created = () => Response.json({ name: "data1", nodeId: "node-1-id" }, { status: 201 });
const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

const volume = (name: string, node = "node-1", nodeId = "node-1-id") => ({
  name,
  driver: "local",
  node,
  nodeId,
  created: "2026-10-01T10:00:00Z",
});

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  answer = created;
  volumes = [volume("existing")];
  swarmEnv = false;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const entry: Sent = {
        method: req.method,
        path: new URL(req.url).pathname,
        body: req.method === "POST" ? await req.clone().json() : undefined,
      };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path === `/api/env/${ENV}/volumes` && req.method === "POST") return answer(entry);
      if (entry.path === `/api/env/${ENV}/volumes`) {
        return Response.json({
          items: volumes,
          total: volumes.length,
          truncated: false,
          nodes: ["node-1", "node-2"],
          notReporting: [],
        });
      }
      if (entry.path === `/api/env/${ENV}/networks`) {
        return Response.json({
          items: [
            {
              id: "b1",
              name: "bridge",
              driver: "bridge",
              scope: "local",
              node: "node-1",
              nodeId: "node-1-id",
            },
            {
              id: "b2",
              name: "bridge",
              driver: "bridge",
              scope: "local",
              node: "node-2",
              nodeId: "node-2-id",
            },
          ],
          total: 2,
          truncated: false,
          nodes: ["node-1", "node-2"],
          notReporting: [],
        });
      }
      if (entry.path === "/api/environments") {
        return Response.json({
          items: swarmEnv ? [{ id: ENV, name: "Swarm", envType: "swarm" }] : [],
        });
      }
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

const posts = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/volumes"));

function wrap(node: React.ReactNode) {
  return (
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          {node}
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>
  );
}

function mount(props: Partial<Parameters<typeof CreateVolumeDialog>[0]> = {}) {
  const onOpenChange = vi.fn();
  render(
    wrap(
      <CreateVolumeDialog
        envId={ENV}
        open
        onOpenChange={onOpenChange}
        swarm={false}
        networks={[]}
        volumes={[]}
        {...props}
      />,
    ),
  );
  return { onOpenChange };
}

function typeName(dialog: HTMLElement, name: string) {
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: name } });
}

describe("CreateVolumeDialog (common fields)", () => {
  it("opens with the title, the explanation, local as the driver, Type None and nothing sent", async () => {
    mount();
    const dialog = await screen.findByRole("dialog", { name: "Create volume" });
    expect(dialog.textContent).toContain("A volume keeps data after its container is gone.");
    expect(dialog.textContent).toContain("Letters, numbers, dot, dash or underscore.");
    expect((within(dialog).getByLabelText("Driver") as HTMLInputElement).value).toBe("local");
    expect(within(dialog).getByRole("combobox", { name: "Type" }).textContent).toContain("None");
    expect(within(dialog).queryByLabelText("Create on node")).toBeNull();
    expect(posts()).toEqual([]);
  });

  it("asks for a name and refuses one with a space, sending nothing", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    expect(await within(dialog).findByText("Enter a name.")).toBeTruthy();
    typeName(dialog, "my vol");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    expect(
      await within(dialog).findByText("Use letters, numbers, dot, dash or underscore."),
    ).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("posts the name and labels, toasts, and closes", async () => {
    const { onOpenChange } = mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "data1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Add label" }));
    fireEvent.change(within(dialog).getByLabelText("Label name"), { target: { value: "app" } });
    fireEvent.change(within(dialog).getByLabelText("Label value"), { target: { value: "x" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "data1", labels: { app: "x" } });
    expect(await screen.findByText("Created volume data1.")).toBeTruthy();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("sends another driver and driver options when they are given", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "data1");
    fireEvent.change(within(dialog).getByLabelText("Driver"), { target: { value: "other" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add option" }));
    fireEvent.change(within(dialog).getByLabelText("Option name"), { target: { value: "size" } });
    fireEvent.change(within(dialog).getByLabelText("Option value"), { target: { value: "1g" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "data1",
      driver: "other",
      driverOptions: { size: "1g" },
    });
  });

  it("sends one request when Create is pressed twice, and shows the pending label", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "data1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    const pending = await within(dialog).findByRole("button", { name: "Creating…" });
    expect(pending.hasAttribute("disabled")).toBe(true);
    // The fields are in a disabled fieldset, so the input is disabled by it.
    expect(within(dialog).getByLabelText("Name").matches(":disabled")).toBe(true);
    fireEvent.click(pending);
    expect(posts()).toHaveLength(1);
    release(created());
  });

  it("says the name is taken under the Name field and keeps the dialog open", async () => {
    answer = () => problem(409, "name_taken");
    const { onOpenChange } = mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "data1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    expect(await within(dialog).findByText("A volume named data1 already exists.")).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("shows a read-only host, an unreachable node and a generic failure inside the dialog", async () => {
    for (const [status, type, text] of [
      [409, "host_read_only", "This host is read-only."],
      [503, "node_unavailable", "isn't reachable right now"],
      [500, "", "Something went wrong on the server."],
    ] as const) {
      cleanup();
      answer = () => problem(status, type);
      mount();
      const dialog = await screen.findByRole("dialog");
      typeName(dialog, "data1");
      fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
      expect((await within(dialog).findByRole("alert")).textContent).toContain(text);
    }
  });

  it("shows markup in a name as the validation message, never as markup", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "<img src=x onerror=alert(1)>");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    await within(dialog).findByText("Use letters, numbers, dot, dash or underscore.");
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("CreateVolumeDialog (swarm)", () => {
  const nodes = [
    {
      id: "b1",
      name: "bridge",
      driver: "bridge",
      scope: "local",
      node: "node-1",
      nodeId: "node-1-id",
    },
    {
      id: "b2",
      name: "bridge",
      driver: "bridge",
      scope: "local",
      node: "node-2",
      nodeId: "node-2-id",
    },
  ] as never[];

  it("requires a node, with its helper, and sends nothing without one", async () => {
    mount({ swarm: true, networks: nodes });
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain("Volumes live on one node.");
    typeName(dialog, "data1");
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    expect(await within(dialog).findByText("Choose a node.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("sends the node that was chosen, by its ID", async () => {
    mount({ swarm: true, networks: nodes });
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "data1");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Create on node" }));
    fireEvent.click(await screen.findByRole("option", { name: "node-2" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "data1", nodeId: "node-2-id" });
  });
});

describe("VolumeList create button", () => {
  const list = () => <VolumeList envId={ENV} onNodeChange={() => {}} />;

  it("shows an env Admin an enabled Create volume button that opens the dialog", async () => {
    render(wrap(list()));
    const button = await screen.findByRole("button", { name: "Create volume" });
    expect(button.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(button);
    expect(await screen.findByRole("dialog", { name: "Create volume" })).toBeTruthy();
  });

  it("shows an Operator the button disabled with Needs Admin, and a click opens nothing", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    render(wrap(list()));
    const button = await screen.findByRole("button", { name: "Create volume" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(button.getAttribute("aria-describedby")).toBeTruthy();
    expect(document.body.textContent).toContain("Needs Admin");
    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows a Viewer the same disabled button", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    render(wrap(list()));
    const button = await screen.findByRole("button", { name: "Create volume" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
  });

  it("shows the empty state with a Create volume button when there are no volumes", async () => {
    volumes = [];
    render(wrap(list()));
    expect(await screen.findByText("No volumes")).toBeTruthy();
    expect(
      screen.getByText(
        "Volumes keep data after a container is gone. Create one, or deploy a stack that uses them.",
      ),
    ).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Create volume" }).length).toBeGreaterThan(0);
  });

  it("lists the volumes the server sent", async () => {
    render(wrap(list()));
    expect(await screen.findByText("existing")).toBeTruthy();
  });
});
