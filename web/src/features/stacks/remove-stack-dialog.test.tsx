// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RemoveStackDialog } from "@/features/stacks/remove-stack-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";

interface Preview {
  services: string[];
  containers: string[];
  networks: string[];
  secrets: string[];
  configs: string[];
  volumes: Array<{ name: string; nodeId: string; nodeName: string; usedOutside: number }>;
  externals: string[];
  notReporting: string[];
  hasDraft: boolean;
}

const emptyPreview = (): Preview => ({
  services: [],
  containers: [],
  networks: [],
  secrets: [],
  configs: [],
  volumes: [],
  externals: [],
  notReporting: [],
  hasDraft: false,
});

let preview: Preview;
let previewStatus: number;
// When set, the preview is not answered until it resolves.
let previewGate: Promise<void> | null;
let removeAnswer: { status: number; body: unknown };
let job: { state: string; items: Array<Record<string, unknown>> };
let bodies: unknown[];
let client: QueryClient;

beforeEach(() => {
  preview = {
    ...emptyPreview(),
    services: ["tracer_api", "tracer_web"],
    networks: ["tracer_default"],
    secrets: ["tracer_tok_abc"],
  };
  previewStatus = 200;
  previewGate = null;
  removeAnswer = { status: 202, body: { jobId: "j1" } };
  job = { state: "done", items: [] };
  bodies = [];
  toasts.success.mockReset();
  toasts.error.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === `/api/env/${ENV}/stacks/s1/removal-preview`) {
        if (previewGate) await previewGate;
        if (previewStatus !== 200)
          return Response.json({ status: previewStatus }, { status: previewStatus });
        return Response.json(preview);
      }
      if (req.method === "POST" && path === `/api/env/${ENV}/stacks/s1/remove`) {
        bodies.push(await req.json());
        return Response.json(removeAnswer.body, { status: removeAnswer.status });
      }
      if (path === `/api/env/${ENV}/stacks/s1/deploys/j1`) {
        return Response.json({ id: "j1", stackId: "s1", mode: "remove", ...job });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(target: "swarm" | "standalone" = "swarm", name = "tracer") {
  const onOpenChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RemoveStackDialog
            stack={{ id: "s1", envId: ENV, name, target }}
            open
            onOpenChange={onOpenChange}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return onOpenChange;
}

// The footer's Close: the dialog's own corner X has the same name and comes first.
const closeButton = () => screen.getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement;

const confirmButton = () => screen.getByRole("button", { name: /^Remove stack/ });

async function typeName(name = "tracer") {
  fireEvent.change(await screen.findByLabelText(`Type ${name} to confirm`), {
    target: { value: name },
  });
}

describe("RemoveStackDialog: the first phase", () => {
  it("says what the removal does and lists what it deletes, counts first, then the definition", async () => {
    mount();

    expect(await screen.findByRole("heading", { name: "Remove tracer?" })).toBeTruthy();
    expect(
      screen.getByText(
        "Bayplan removes the stack's services, its own networks, and its own secrets and configs. External networks, secrets, configs and volumes are never touched.",
      ),
    ).toBeTruthy();
    expect(await screen.findByText("2 services, 1 network, 1 secret, 0 configs")).toBeTruthy();
    const table = screen.getByRole("table", { name: "What removing tracer deletes" });
    const rows = within(table)
      .getAllByRole("row")
      .slice(1)
      .map((r) =>
        within(r)
          .getAllByRole("cell")
          .map((c) => c.textContent),
      );
    expect(rows).toEqual([
      ["Service", "tracer_api"],
      ["Service", "tracer_web"],
      ["Network", "tracer_default"],
      ["Secret", "tracer_tok_abc"],
      ["Definition", "tracer"],
    ]);
  });

  it("counts a standalone stack's containers and says containers in the body", async () => {
    preview = { ...emptyPreview(), containers: ["tracer-web-1"], networks: ["tracer_default"] };
    mount("standalone");

    expect(
      await screen.findByText(
        "Bayplan removes the stack's containers, its own networks, and its own secrets and configs. External networks, secrets, configs and volumes are never touched.",
      ),
    ).toBeTruthy();
    expect(await screen.findByText("1 container, 1 network, 0 secrets, 0 configs")).toBeTruthy();
  });

  it("lists only the definition for a stack with nothing on the engine", async () => {
    preview = emptyPreview();
    mount();

    expect(await screen.findByText("0 services, 0 networks, 0 secrets, 0 configs")).toBeTruthy();
    const table = screen.getByRole("table");
    expect(within(table).getAllByRole("row")).toHaveLength(2);
  });

  it("shows a spinner row and keeps the confirm off until the list has loaded", async () => {
    let release: () => void = () => {};
    previewGate = new Promise<void>((resolve) => {
      release = resolve;
    });
    mount();
    expect((await screen.findByRole("status")).textContent).toContain(
      "Looking at what tracer owns",
    );
    expect(confirmButton().getAttribute("aria-disabled")).toBe("true");
    await typeName();
    expect(confirmButton().getAttribute("aria-disabled")).toBe("true");
    release();
    await screen.findByRole("table");
    await waitFor(() => expect(confirmButton().getAttribute("aria-disabled")).toBe("false"));
  });

  it("keeps the confirm aria-disabled until the name is typed exactly", async () => {
    mount();
    await screen.findByRole("table");
    expect(confirmButton().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirmButton());
    expect(bodies).toEqual([]);

    fireEvent.change(screen.getByLabelText("Type tracer to confirm"), {
      target: { value: "Tracer" },
    });
    expect(confirmButton().getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(screen.getByLabelText("Type tracer to confirm"), {
      target: { value: "tracer" },
    });
    expect(confirmButton().getAttribute("aria-disabled")).toBe("false");
    expect(bodies).toEqual([]);
  });

  it("says it could not read what the stack owns, with a retry, and keeps the confirm off", async () => {
    previewStatus = 500;
    mount();

    expect(
      await screen.findByText(
        "Couldn't read what tracer owns. Check the connection and try again.",
      ),
    ).toBeTruthy();
    await typeName();
    expect(confirmButton().getAttribute("aria-disabled")).toBe("true");
    previewStatus = 200;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table")).toBeTruthy();
  });

  it("wraps a long stack name in the title and the typed confirmation", async () => {
    const name = "a-very-long-stack-name-that-would-push-the-dialog-wider-than-the-screen";
    mount("swarm", name);

    expect(await screen.findByRole("heading", { name: `Remove ${name}?` })).toBeTruthy();
    expect(screen.getByRole("heading", { name: `Remove ${name}?` }).className).toContain(
      "break-all",
    );
    expect(screen.getByText(`Type ${name} to confirm`).className).toContain("break-words");
  });
});

describe("RemoveStackDialog: the second phase", () => {
  async function submit() {
    await screen.findByRole("table");
    await typeName();
    fireEvent.click(confirmButton());
  }

  it("sends the name and nothing ticked, then swaps to the panel in the same dialog and toasts when done", async () => {
    job = {
      state: "done",
      items: [
        { id: "check", label: "Check the stack", state: "done", code: "", message: "", detail: "" },
        {
          id: "services",
          label: "Remove services",
          state: "done",
          code: "",
          message: "",
          detail: "",
        },
        {
          id: "definition",
          label: "Delete the definition",
          state: "done",
          code: "",
          message: "",
          detail: "",
        },
      ],
    };
    const onOpenChange = mount();

    await submit();

    await waitFor(() =>
      expect(bodies).toEqual([{ confirmName: "tracer", volumes: [], keepDefinition: false }]),
    );
    const heading = await screen.findByRole("heading", { name: "Removing tracer" });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    expect(await screen.findByText("Delete the definition")).toBeTruthy();
    expect((await screen.findByRole("status")).textContent).toBe("Removed tracer.");
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Removed tracer."));
    expect(onOpenChange).not.toHaveBeenCalled();
    fireEvent.click(closeButton());
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("shows how many tasks are left while it waits for them to stop", async () => {
    job = {
      state: "running",
      items: [
        {
          id: "services",
          label: "Remove services",
          state: "done",
          code: "",
          message: "",
          detail: "",
        },
        {
          id: "wait",
          label: "Wait for tasks to stop",
          state: "running",
          code: "",
          message: "",
          detail: "",
          result: { left: "3" },
        },
        {
          id: "remove-networks",
          label: "Remove networks",
          state: "waiting",
          code: "",
          message: "",
          detail: "",
        },
      ],
    };
    mount();

    await submit();

    expect(await screen.findByText("3 left")).toBeTruthy();
    expect(screen.getByText("Wait for tasks to stop")).toBeTruthy();
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("keeps what failed in view, names the object and says what stays removed", async () => {
    job = {
      state: "partial",
      items: [
        {
          id: "services",
          label: "Remove services",
          state: "done",
          code: "",
          message: "",
          detail: "",
        },
        {
          id: "remove-networks",
          label: "Remove networks",
          state: "failed",
          code: "in_use",
          message: "Something outside this stack still uses it, so it was left in place.",
          detail: "network tracer_back is in use by service other_web",
          result: { name: "tracer_back" },
        },
        {
          id: "definition",
          label: "Delete the definition",
          state: "skipped",
          code: "not_run",
          message: "Not run because an earlier step failed.",
          detail: "",
        },
      ],
    };
    mount();

    await submit();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Couldn't remove tracer: Something outside this stack still uses it, so it was left in place. What was already removed stays removed. Remove the rest from this page.",
    );
    expect(screen.getByText("tracer_back")).toBeTruthy();
    expect(screen.getByText("network tracer_back is in use by service other_web")).toBeTruthy();
    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith(
        "Couldn't remove tracer: Something outside this stack still uses it, so it was left in place. What was already removed stays removed. Remove the rest from this page.",
      ),
    );
    expect(closeButton()).toBeTruthy();
  });

  it("keeps the dialog open and says why when the server refuses, with no toast over it", async () => {
    removeAnswer = {
      status: 409,
      body: { type: "operation_locked", title: "Conflict", status: 409, detail: "busy" },
    };
    const onOpenChange = mount();

    await submit();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Couldn't remove tracer: Another stack operation is running on this environment. Try again when it finishes. What was already removed stays removed. Remove the rest from this page.",
    );
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
    expect(toasts.error).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: "Remove tracer?" })).toBeTruthy();
    await waitFor(() => expect(confirmButton().getAttribute("aria-disabled")).toBe("false"));
  });
});

const volume = (name: string, nodeName = "node-a", usedOutside = 0) => ({
  name,
  nodeId: `id-${nodeName}`,
  nodeName,
  usedOutside,
});

describe("RemoveStackDialog: volumes", () => {
  it("lists each volume of a swarm stack on each node, unticked, with the node in its accessible name", async () => {
    preview = { ...emptyPreview(), volumes: [volume("data", "node-a"), volume("data", "node-b")] };
    mount();

    const first = await screen.findByRole("checkbox", { name: "Remove volume data on node-a" });
    const second = screen.getByRole("checkbox", { name: "Remove volume data on node-b" });
    expect(first.getAttribute("aria-checked")).toBe("false");
    expect(second.getAttribute("aria-checked")).toBe("false");
    expect(screen.getAllByText("on node-a")).toHaveLength(1);
    expect(
      screen.getByText(
        "Volumes hold your data. Tick only the ones you're sure you don't need. Unticked volumes stay on the host.",
      ),
    ).toBeTruthy();
  });

  it("lists a standalone stack's volume once with no node", async () => {
    preview = { ...emptyPreview(), volumes: [volume("data", "host")] };
    mount("standalone");

    expect(await screen.findByRole("checkbox", { name: "Remove volume data" })).toBeTruthy();
    expect(screen.queryByText("on host")).toBeNull();
  });

  it("says a stack has no volumes of its own in place of the section", async () => {
    preview = emptyPreview();
    mount();

    expect(await screen.findByText("This stack has no volumes of its own.")).toBeTruthy();
    expect(screen.queryByRole("checkbox", { name: /Remove volume/ })).toBeNull();
    expect(screen.queryByText(/Volumes hold your data/)).toBeNull();
  });

  it("switches the confirm to Remove stack and {n} volumes, plural-aware, and back", async () => {
    preview = { ...emptyPreview(), volumes: [volume("a"), volume("b")] };
    mount();
    expect((await screen.findByRole("button", { name: "Remove stack" })).textContent).toBe(
      "Remove stack",
    );

    fireEvent.click(screen.getByRole("checkbox", { name: "Remove volume a on node-a" }));
    expect(screen.getByRole("button", { name: "Remove stack and 1 volume" })).toBeTruthy();
    fireEvent.click(screen.getByRole("checkbox", { name: "Remove volume b on node-a" }));
    expect(screen.getByRole("button", { name: "Remove stack and 2 volumes" })).toBeTruthy();
    fireEvent.click(screen.getByRole("checkbox", { name: "Remove volume a on node-a" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Remove volume b on node-a" }));
    expect(screen.getByRole("button", { name: "Remove stack" })).toBeTruthy();
  });

  it("sends exactly the volumes that were ticked, each with its node", async () => {
    preview = {
      ...emptyPreview(),
      volumes: [volume("data", "node-a"), volume("data", "node-b"), volume("cache", "node-a")],
    };
    mount();
    await screen.findByRole("checkbox", { name: "Remove volume cache on node-a" });

    fireEvent.click(screen.getByRole("checkbox", { name: "Remove volume data on node-b" }));
    await typeName();
    fireEvent.click(screen.getByRole("button", { name: "Remove stack and 1 volume" }));

    await waitFor(() =>
      expect(bodies).toEqual([
        {
          confirmName: "tracer",
          volumes: [{ name: "data", nodeId: "id-node-b" }],
          keepDefinition: false,
        },
      ]),
    );
  });

  it("warns on the row of a volume that containers outside the stack use, plural-aware", async () => {
    preview = {
      ...emptyPreview(),
      volumes: [
        volume("data", "node-a", 2),
        volume("cache", "node-a", 1),
        volume("logs", "node-a", 0),
      ],
    };
    mount();

    expect(
      await screen.findByText(
        "2 containers outside this stack use its volume data. Removing the volume will fail while they run.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "1 container outside this stack uses its volume cache. Removing the volume will fail while it runs.",
      ),
    ).toBeTruthy();
    expect(screen.getAllByText(/outside this stack/)).toHaveLength(2);
  });

  it("puts the volume checkboxes before the typed field in tab order", async () => {
    preview = { ...emptyPreview(), volumes: [volume("data")] };
    mount();

    const box = await screen.findByRole("checkbox", { name: "Remove volume data on node-a" });
    const typed = screen.getByLabelText("Type tracer to confirm");
    expect(box.compareDocumentPosition(typed) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("scrolls a long list inside a 240px box and still lists every volume", async () => {
    preview = {
      ...emptyPreview(),
      volumes: Array.from({ length: 60 }, (_, i) =>
        volume(`vol-${i % 10}`, `node-${Math.floor(i / 10)}`),
      ),
    };
    mount();

    await screen.findAllByRole("checkbox", { name: /Remove volume vol-0 on/ });
    expect(screen.getAllByRole("checkbox", { name: /^Remove volume / })).toHaveLength(60);
    const list = screen.getByTestId("volume-list");
    expect(list.className).toContain("max-h-60");
    expect(list.className).toContain("overflow-y-auto");
  });

  it("names the nodes that did not answer, whose volumes are not listed", async () => {
    preview = { ...emptyPreview(), notReporting: ["node-z"] };
    mount();

    expect(
      await screen.findByText("node-z didn't answer, so its volumes aren't listed."),
    ).toBeTruthy();
  });

  it("starts every reopening with nothing ticked", async () => {
    preview = { ...emptyPreview(), volumes: [volume("data")] };
    const first = mount();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Remove volume data on node-a" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(first).toHaveBeenCalledWith(false);
    cleanup();
    client.clear();

    mount();

    const box = await screen.findByRole("checkbox", { name: "Remove volume data on node-a" });
    expect(box.getAttribute("aria-checked")).toBe("false");
  });
});

describe("RemoveStackDialog: keeping the definition and the draft", () => {
  it("offers Keep the definition, unticked, with its helper", async () => {
    mount();

    const keep = await screen.findByRole("checkbox", { name: "Keep the definition" });
    expect(keep.getAttribute("aria-checked")).toBe("false");
    expect(
      screen.getByText(
        "Keeps the compose file, files and variables so you can deploy it again. The stack shows as Not deployed.",
      ),
    ).toBeTruthy();
  });

  it("sends keepDefinition when it is ticked and ends on Open stack", async () => {
    job = {
      state: "done",
      items: [
        {
          id: "services",
          label: "Remove services",
          state: "done",
          code: "",
          message: "",
          detail: "",
        },
      ],
    };
    mount();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Keep the definition" }));
    await typeName();
    fireEvent.click(confirmButton());

    await waitFor(() =>
      expect(bodies).toEqual([{ confirmName: "tracer", volumes: [], keepDefinition: true }]),
    );
    expect(await screen.findByRole("link", { name: "Open stack" })).toBeTruthy();
    expect((await screen.findByRole("status")).textContent).toBe(
      "Removed tracer. Its definition is kept, so you can deploy it again.",
    );
  });

  it("says a saved draft is deleted too, until the definition is kept", async () => {
    preview = { ...emptyPreview(), hasDraft: true };
    mount();

    expect(
      await screen.findByText("This stack has a saved draft. Removing the stack deletes it too."),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("checkbox", { name: "Keep the definition" }));
    expect(
      screen.queryByText("This stack has a saved draft. Removing the stack deletes it too."),
    ).toBeNull();
  });

  it("does not mention a draft when there is none", async () => {
    mount();

    await screen.findByRole("table");
    expect(screen.queryByText(/saved draft/)).toBeNull();
  });
});
