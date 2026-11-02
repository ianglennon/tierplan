// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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

  it("shows a read-only host, a host-policy refusal, an unreachable node and a generic failure inside the dialog", async () => {
    for (const [status, type, text] of [
      [409, "host_read_only", "This host is read-only."],
      [409, "agent_host_policy", "/srv, /mnt or /media"],
      [409, "agent_host_policy", "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES"],
      [503, "node_unavailable", "isn't reachable right now"],
      [500, "", "Something went wrong on the server."],
    ] as const) {
      cleanup();
      answer = () => problem(status, type);
      const { onOpenChange } = mount();
      const dialog = await screen.findByRole("dialog");
      typeName(dialog, "data1");
      fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));
      expect((await within(dialog).findByRole("alert")).textContent).toContain(text);
      if (type === "agent_host_policy") {
        // A refusal the host's owner chose is not an out-of-date agent.
        expect(screen.queryByText(/agent needs an update/i)).toBeNull();
        expect(onOpenChange).not.toHaveBeenCalled();
      }
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
  const list = () => <VolumeList envId={ENV} search={{}} onSearchChange={() => {}} />;

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

// The presets, the generated options and the SMB password warning (plan 04-16 task 2).

async function chooseType(dialog: HTMLElement, label: string) {
  fireEvent.click(within(dialog).getByRole("combobox", { name: "Type" }));
  fireEvent.click(await screen.findByRole("option", { name: label }));
}

const fill = (dialog: HTMLElement, label: string, value: string) =>
  fireEvent.change(within(dialog).getByLabelText(label), { target: { value } });

const submit = (dialog: HTMLElement) =>
  fireEvent.click(within(dialog).getByRole("button", { name: "Create volume" }));

describe("CreateVolumeDialog (presets)", () => {
  it("offers None, NFS share, SMB (CIFS) share and Host folder", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Type" }));
    for (const label of ["None", "NFS share", "SMB (CIFS) share", "Host folder"]) {
      expect(await screen.findByRole("option", { name: label })).toBeTruthy();
    }
  });

  it("swaps the NFS fields in place, with the mount options defaulting to rw", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryByLabelText("Server address")).toBeNull();
    await chooseType(dialog, "NFS share");
    expect(within(dialog).getByLabelText("Server address")).toBeTruthy();
    const path = within(dialog).getByLabelText("Exported path") as HTMLInputElement;
    expect(path.placeholder).toBe("/srv/data");
    expect((within(dialog).getByLabelText("Mount options") as HTMLInputElement).value).toBe("rw");
    // The free-form editor gives way to the generated options.
    expect(within(dialog).queryByRole("button", { name: "Add option" })).toBeNull();
    expect(within(dialog).queryByText(/stores this password/)).toBeNull();
  });

  it("shows the generated options read-only as the NFS fields are typed", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await chooseType(dialog, "NFS share");
    fill(dialog, "Server address", "10.0.0.5");
    fill(dialog, "Exported path", "/srv/data");
    const generated = within(dialog).getByRole("group", {
      name: "Driver options that will be sent",
    });
    expect(within(generated).getByText("addr=10.0.0.5,rw")).toBeTruthy();
    expect(within(generated).getByText(":/srv/data")).toBeTruthy();
    expect(within(generated).getByText("nfs")).toBeTruthy();
    expect(generated.querySelector("input")).toBeNull();
  });

  it("posts the NFS options and the name", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "share1");
    await chooseType(dialog, "NFS share");
    fill(dialog, "Server address", "10.0.0.5");
    fill(dialog, "Exported path", "/srv/data");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "share1",
      driverOptions: { type: "nfs", o: "addr=10.0.0.5,rw", device: ":/srv/data" },
    });
  });

  it("asks for the server and a path that starts with a slash, sending nothing", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "share1");
    await chooseType(dialog, "NFS share");
    fill(dialog, "Exported path", "srv/data");
    submit(dialog);
    expect(await within(dialog).findByText("Enter a server address.")).toBeTruthy();
    expect(within(dialog).getByText("Enter a path that starts with /.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("shows the SMB fields and the password warning, and posts the cifs options", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "media");
    await chooseType(dialog, "SMB (CIFS) share");
    expect(dialog.textContent).toContain(
      "Docker stores this password in the volume's options, where anyone who can inspect the volume can read it. Use a credentials file if you can.",
    );
    const password = within(dialog).getByLabelText("Password") as HTMLInputElement;
    expect(password.type).toBe("password");
    fill(dialog, "Server address", "nas.lan");
    fill(dialog, "Share name", "media");
    fill(dialog, "Username", "bob");
    fill(dialog, "Password", "s3cret");
    fill(dialog, "Mount options", "vers=3.0");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "media",
      driverOptions: {
        type: "cifs",
        o: "addr=nas.lan,username=bob,password=s3cret,vers=3.0",
        device: "//nas.lan/media",
      },
    });
  });

  it("asks for a share name for SMB", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "media");
    await chooseType(dialog, "SMB (CIFS) share");
    fill(dialog, "Server address", "nas.lan");
    submit(dialog);
    expect(await within(dialog).findByText("Enter a share name.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("shows a typed SMB password as dots in the generated rows, never in the clear", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await chooseType(dialog, "SMB (CIFS) share");
    fill(dialog, "Server address", "nas.lan");
    fill(dialog, "Share name", "media");
    fill(dialog, "Username", "bob");
    fill(dialog, "Password", "s3cret");
    const generated = within(dialog).getByRole("group", {
      name: "Driver options that will be sent",
    });
    expect(generated.textContent).toContain("username=bob,password=");
    expect(generated.textContent).not.toContain("s3cret");
  });

  it("shows the host folder field with its helper and posts a bind", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "hostdata");
    await chooseType(dialog, "Host folder");
    expect(dialog.textContent).toContain("The folder must already exist on the host.");
    expect(
      (within(dialog).getByLabelText("Folder on the host") as HTMLInputElement).placeholder,
    ).toBe("/srv/data");
    fill(dialog, "Folder on the host", "relative");
    submit(dialog);
    expect(await within(dialog).findByText("Enter a path that starts with /.")).toBeTruthy();
    expect(posts()).toEqual([]);
    fill(dialog, "Folder on the host", "/srv/data");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "hostdata",
      driverOptions: { type: "none", o: "bind", device: "/srv/data" },
    });
  });

  it("names the chosen node in the host folder helper on a swarm", async () => {
    mount({
      swarm: true,
      networks: [
        { id: "b", name: "bridge", driver: "bridge", scope: "local", node: "node-2", nodeId: "n2" },
      ] as never[],
    });
    const dialog = await screen.findByRole("dialog");
    await chooseType(dialog, "Host folder");
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Create on node" }));
    fireEvent.click(await screen.findByRole("option", { name: "node-2" }));
    expect(dialog.textContent).toContain("The folder must already exist on node-2.");
  });

  it("goes back to None: the preset fields go, the free-form options come back, nothing is kept", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    typeName(dialog, "plain");
    await chooseType(dialog, "SMB (CIFS) share");
    fill(dialog, "Password", "s3cret");
    await chooseType(dialog, "None");
    expect(within(dialog).queryByLabelText("Password")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Add option" })).toBeTruthy();
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "plain" });
  });

  it("wraps long paths and option rows with break-all in mono", async () => {
    mount();
    const dialog = await screen.findByRole("dialog");
    await chooseType(dialog, "Host folder");
    fill(dialog, "Folder on the host", `/${"a".repeat(200)}`);
    const generated = within(dialog).getByRole("group", {
      name: "Driver options that will be sent",
    });
    const value = within(generated).getByText(`/${"a".repeat(200)}`);
    expect(value.className).toContain("break-all");
    expect(value.className).toContain("font-mono");
  });
});
