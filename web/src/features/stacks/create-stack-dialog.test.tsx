// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CreateStackDialog } from "@/features/stacks/create-stack-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let answer: () => Response;
let client: QueryClient;
// Who is signed in and which environments exist, for the tests of the All stacks list's dialog.
let me: ReturnType<typeof makeMe>;
let environments: { id: string; name: string; envType: string }[];
// The name check: what it answers, and the (environment, name) it was asked about.
let checkAnswer: () => Response | Promise<Response>;
let checks: { envId: string; name: string }[];

const made = () =>
  Response.json(
    {
      id: "stack-1",
      name: "web",
      target: "swarm",
      source: "inline",
      state: "not_deployed",
      lastDeployedAt: "",
      lastDeployedBy: "",
      hasDraft: false,
      envId: ENV,
      sourceRef: "",
      createdBy: "ada",
      createdAt: "2026-10-19T10:00:00Z",
      stopped: false,
    },
    { status: 201 },
  );
const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

beforeEach(() => {
  sent = [];
  answer = made;
  me = makeMe({ envs: { [ENV]: "admin" } });
  environments = [{ id: ENV, name: "Prod", envType: "swarm" }];
  checks = [];
  checkAnswer = () => Response.json({ status: "available", message: "" });
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
      if (entry.path === "/api/environments") return Response.json({ items: environments });
      const check = /^\/api\/env\/([^/]+)\/stacks\/name-check$/.exec(entry.path);
      if (check) {
        checks.push({
          envId: check[1] as string,
          name: new URL(req.url).searchParams.get("name") ?? "",
        });
        return checkAnswer();
      }
      if (/^\/api\/env\/[^/]+\/stacks$/.test(entry.path) && req.method === "POST") return answer();
      // The environment list names the type, so the summary is not what these tests are about.
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

const posts = () => sent.filter((s) => s.method === "POST");

// "env" is the dialog of an environment's own list; "global" is the All stacks list's, which asks for
// the environment.
function mount(scope: "env" | "global" = "env") {
  const envId = scope === "env" ? ENV : undefined;
  const onOpenChange = vi.fn();
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <CreateStackDialog envId={envId} open onOpenChange={onOpenChange} />
        <Toaster />
      </TooltipProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/unmanaged-stacks/$name" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/git-stacks/new" }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return { router, onOpenChange };
}

async function open() {
  return screen.findByRole("dialog", { name: "Create a stack" });
}

function typeName(dialog: HTMLElement, name: string) {
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: name } });
}

// The sentences of the stack-name rule of each target, and of the union that applies before a target is known (D-53).
const SWARM_SENTENCE =
  "Use only letters, numbers, dash and underscore, starting with a letter or number, up to 61 characters.";
const STANDALONE_SENTENCE =
  "Use only lowercase letters, numbers, dash and underscore, starting with a letter or number, up to 255 characters.";
const UNION_SENTENCE =
  "Use only letters, numbers, dash and underscore, starting with a letter or number.";

// The dialog of an environment's own list knows its target once the environment list has loaded.
async function ready(dialog: HTMLElement, kind: "Swarm" | "Standalone" = "Swarm") {
  await waitFor(() => expect(dialog.textContent).toContain(`(${kind})`));
}

function submit(dialog: HTMLElement) {
  fireEvent.click(within(dialog).getByRole("button", { name: "Create stack" }));
}

describe("CreateStackDialog", () => {
  it("opens with the title, the explanation, the rule under Name and the starter example chosen", async () => {
    mount();
    const dialog = await open();
    expect(dialog.textContent).toContain("Nothing is deployed until you review and deploy it.");
    await ready(dialog);
    expect(dialog.textContent).toContain(
      "Letters, numbers, dash or underscore, up to 61 characters. It can't change later.",
    );
    expect(
      within(dialog).getByRole("radio", { name: "Starter example" }).getAttribute("aria-checked"),
    ).toBe("true");
    expect(
      within(dialog).getByRole("radio", { name: "Empty file" }).getAttribute("aria-checked"),
    ).toBe("false");
    expect(posts()).toEqual([]);
  });

  it("names the environment and its kind", async () => {
    mount();
    const dialog = await open();
    await waitFor(() => expect(dialog.textContent).toContain("On Prod (Swarm)"));
  });

  it.each(["Shop.Front", "-web", "my stack", "a/b", "x".repeat(62)])(
    "refuses the name %j on a swarm with the swarm rule, and sends nothing",
    async (name) => {
      mount();
      const dialog = await open();
      await ready(dialog);
      typeName(dialog, name);
      submit(dialog);
      expect(await within(dialog).findByText(SWARM_SENTENCE)).toBeTruthy();
      expect(posts()).toEqual([]);
    },
  );

  it.each(["MyApp", "my_app-2", "x".repeat(61)])(
    "takes the name %j on a swarm and sends it",
    async (name) => {
      mount();
      const dialog = await open();
      await ready(dialog);
      typeName(dialog, name);
      submit(dialog);
      await waitFor(() => expect(posts()).toHaveLength(1));
      expect(posts()[0]?.body).toEqual({ name, start: "example" });
    },
  );

  it("asks for a name when it is empty", async () => {
    mount();
    const dialog = await open();
    submit(dialog);
    expect(await within(dialog).findByText("Enter a name for the stack.")).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("sends the name and the starter example by default", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "web", start: "example" });
  });

  it("sends start empty when Empty file is chosen", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.click(within(dialog).getByRole("radio", { name: "Empty file" }));
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "web", start: "empty" });
  });

  it("goes to the new stack's page and says nothing is deployed yet", async () => {
    const { router } = mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1/stacks/stack-1"));
    expect(await screen.findByText("Created stack web. Nothing is deployed yet.")).toBeTruthy();
  });

  it("shows a taken name inside the dialog as an alert and stays open", async () => {
    answer = () => problem(409, "stack_name_taken");
    const { router } = mount();
    const dialog = await open();
    // The environment's name comes from the list the sidebar has already loaded.
    await waitFor(() => expect(dialog.textContent).toContain("On Prod (Swarm)"));
    typeName(dialog, "web");
    submit(dialog);
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("A stack named web already exists on Prod.");
    expect(router.state.location.pathname).toBe("/");
    expect(screen.queryByText(/Created stack/)).toBeNull();
  });

  it("shows the server's name rule when it refuses a name the form let through", async () => {
    answer = () => problem(422, "stack_name_invalid");
    mount();
    const dialog = await open();
    await ready(dialog);
    typeName(dialog, "web");
    submit(dialog);
    expect(await within(dialog).findByText(SWARM_SENTENCE)).toBeTruthy();
  });

  it("says it cannot create stacks when the server answers 403", async () => {
    answer = () => problem(403, "forbidden");
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "You can't create stacks on this environment.",
    );
  });

  it("closes without sending anything on Cancel", async () => {
    const { onOpenChange } = mount();
    const dialog = await open();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(posts()).toEqual([]);
  });
});

describe("CreateStackDialog: a stack from git", () => {
  it("offers A git repository with its helper, and the footer says Choose repository", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    const radio = within(dialog).getByRole("radio", { name: "A git repository" });
    expect(radio.getAttribute("aria-describedby")).toBeTruthy();
    fireEvent.click(radio);
    expect(dialog.textContent).toContain(
      "Keep web in sync with a folder in a repository. You choose the repository on the next page.",
    );
    expect(within(dialog).getByRole("button", { name: "Choose repository" })).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: "Create stack" })).toBeNull();
  });

  it("opens the From git page with the name and creates nothing", async () => {
    const { router } = mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.click(within(dialog).getByRole("radio", { name: "A git repository" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Choose repository" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/e1/git-stacks/new"));
    expect(router.state.location.search).toEqual({ name: "web" });
    expect(posts()).toEqual([]);
  });

  it("still holds the name to the rule of the target", async () => {
    const { router } = mount();
    const dialog = await open();
    await ready(dialog);
    typeName(dialog, "my stack");
    fireEvent.click(within(dialog).getByRole("radio", { name: "A git repository" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Choose repository" }));
    expect(await within(dialog).findByText(SWARM_SENTENCE)).toBeTruthy();
    expect(router.state.location.pathname).toBe("/");
  });
});

const longFile = (name: string, body: BlobPart = "services: {}\n") => new File([body], name);

// chooseFile picks a file for the Upload row, as a person does with the system's file chooser.
function chooseFile(dialog: HTMLElement, file: File) {
  fireEvent.click(within(dialog).getByRole("radio", { name: "Upload a compose file" }));
  fireEvent.change(within(dialog).getByLabelText("Compose file"), { target: { files: [file] } });
}

describe("CreateStackDialog: the name check", () => {
  it("asks the server about the name when the field loses focus, and says nothing for a free one", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    expect(checks).toEqual([]);
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    await waitFor(() => expect(checks).toEqual([{ envId: ENV, name: "web" }]));
    await waitFor(() => expect(within(dialog).queryByRole("status")).toBeNull());
    expect(within(dialog).queryByRole("alert")).toBeNull();
    expect(posts()).toEqual([]);
  });

  it("shows a spinner in the field while the check is on its way", async () => {
    let release: (r: Response) => void = () => {};
    checkAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    expect(await within(dialog).findByRole("status", { name: "Checking the name" })).toBeTruthy();
    release(Response.json({ status: "available", message: "" }));
    await waitFor(() => expect(within(dialog).queryByRole("status")).toBeNull());
  });

  it("does not ask about an empty name or one that breaks the rule", async () => {
    mount();
    const dialog = await open();
    await ready(dialog);
    const name = within(dialog).getByLabelText("Name");
    fireEvent.blur(name);
    typeName(dialog, "Not Valid");
    fireEvent.blur(name);
    expect(await within(dialog).findByText(SWARM_SENTENCE)).toBeTruthy();
    expect(checks).toEqual([]);
  });

  it("shows a taken name inline as an alert at once, and sends nothing on submit", async () => {
    checkAnswer = () =>
      Response.json({ status: "taken", message: "A stack named web already exists on Prod." });
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("A stack named web already exists on Prod.");
    submit(dialog);
    await waitFor(() => expect(checks).toHaveLength(2));
    expect(posts()).toEqual([]);
  });

  it("shows a name a foreign stack holds with the second line of the spec", async () => {
    checkAnswer = () =>
      Response.json({
        status: "clash",
        message:
          "A stack named legacy already exists on this environment, not managed by TierPlan.",
      });
    mount();
    const dialog = await open();
    typeName(dialog, "legacy");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain(
      "A stack named legacy already exists on this environment, not managed by TierPlan.",
    );
    expect(alert.textContent).toContain("Pick another name, or adopt the existing stack.");
    expect(alert.textContent).not.toContain("later version");
    // The way to the stack that is there: its read-only page.
    const toStack = within(alert).getByRole("link", { name: "Open legacy" });
    expect(toStack.getAttribute("href")).toBe("/env/e1/unmanaged-stacks/legacy");
  });

  it("offers no way to the stack that is there to a user who cannot read stacks", async () => {
    me = makeMe({
      permissions: { global: [], environments: { [ENV]: ["stacks.create", "environments.read"] } },
    });
    checkAnswer = () =>
      Response.json({
        status: "clash",
        message:
          "A stack named legacy already exists on this environment, not managed by TierPlan.",
      });
    mount();
    const dialog = await open();
    typeName(dialog, "legacy");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("Pick another name, or adopt the existing stack.");
    expect(within(alert).queryByRole("link")).toBeNull();
  });

  it("takes the alert away when the name is changed", async () => {
    checkAnswer = () =>
      Response.json({ status: "taken", message: "A stack named web already exists on Prod." });
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    await within(dialog).findByRole("alert");
    typeName(dialog, "web2");
    await waitFor(() => expect(within(dialog).queryByRole("alert")).toBeNull());
  });

  it("shows the rule when the server calls the name invalid", async () => {
    checkAnswer = () =>
      Response.json({
        status: "invalid",
        message: SWARM_SENTENCE,
      });
    mount();
    const dialog = await open();
    await ready(dialog);
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    expect(await within(dialog).findByText(SWARM_SENTENCE)).toBeTruthy();
  });

  it("checks again on submit, and creates nothing when the name has been taken since", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    await waitFor(() => expect(checks).toHaveLength(1));
    checkAnswer = () =>
      Response.json({ status: "taken", message: "A stack named web already exists on Prod." });
    submit(dialog);
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "A stack named web already exists on Prod.",
    );
    expect(checks).toHaveLength(2);
    expect(posts()).toEqual([]);
  });

  it("goes on to create when the check itself cannot be answered, since the server decides anyway", async () => {
    checkAnswer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
  });

  it("shows Creating… on the button while the name is checked and the stack made", async () => {
    let release: (r: Response) => void = () => {};
    checkAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    expect(await within(dialog).findByRole("button", { name: "Creating…" })).toBeTruthy();
    release(Response.json({ status: "available", message: "" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
  });

  it("shows a name a foreign stack took in the meantime when the create is refused with a clash", async () => {
    answer = () => problem(409, "stack_name_clash");
    mount();
    const dialog = await open();
    typeName(dialog, "legacy");
    submit(dialog);
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain(
      "A stack named legacy already exists on this environment, not managed by TierPlan.",
    );
    expect(alert.textContent).toContain("Pick another name, or adopt the existing stack.");
    expect(within(alert).getByRole("link", { name: "Open legacy" })).toBeTruthy();
  });

  it("stops the name field at 61 characters on a swarm and 255 on a standalone host", async () => {
    mount();
    const dialog = await open();
    await ready(dialog);
    expect(within(dialog).getByLabelText("Name").getAttribute("maxlength")).toBe("61");
    cleanup();
    environments = [{ id: ENV, name: "Prod", envType: "standalone" }];
    mount();
    const standalone = await open();
    await ready(standalone, "Standalone");
    expect(within(standalone).getByLabelText("Name").getAttribute("maxlength")).toBe("255");
  });
});

describe("CreateStackDialog: a standalone host", () => {
  beforeEach(() => {
    environments = [{ id: ENV, name: "Prod", envType: "standalone" }];
  });

  it("says the standalone rule under Name", async () => {
    mount();
    const dialog = await open();
    await ready(dialog, "Standalone");
    expect(dialog.textContent).toContain(
      "Lowercase letters, numbers, dash or underscore, up to 255 characters. It can't change later.",
    );
  });

  it.each(["Web", "MyApp", "a.b", "-web"])(
    "refuses the name %j with the standalone rule, and sends nothing",
    async (name) => {
      mount();
      const dialog = await open();
      await ready(dialog, "Standalone");
      typeName(dialog, name);
      submit(dialog);
      expect(await within(dialog).findByText(STANDALONE_SENTENCE)).toBeTruthy();
      expect(posts()).toEqual([]);
    },
  );

  it("takes shop-front and sends it", async () => {
    mount();
    const dialog = await open();
    await ready(dialog, "Standalone");
    typeName(dialog, "shop-front");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "shop-front", start: "example" });
  });

  it("shows the standalone rule when the server calls the name invalid", async () => {
    answer = () => problem(422, "stack_name_invalid");
    mount();
    const dialog = await open();
    await ready(dialog, "Standalone");
    typeName(dialog, "web");
    submit(dialog);
    expect(await within(dialog).findByText(STANDALONE_SENTENCE)).toBeTruthy();
  });
});

describe("CreateStackDialog: upload", () => {
  it("offers Upload a compose file as a third way to start", async () => {
    mount();
    const dialog = await open();
    // Starter example, Empty file, Upload a compose file, and A git repository.
    expect(within(dialog).getAllByRole("radio")).toHaveLength(4);
    expect(within(dialog).getByRole("radio", { name: "Upload a compose file" })).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: "Choose file" })).toBeNull();
  });

  it("disables Create stack with the reason Choose a file. until a file is chosen", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    fireEvent.click(within(dialog).getByRole("radio", { name: "Upload a compose file" }));
    const create = within(dialog).getByRole("button", { name: "Create stack" });
    expect(create.getAttribute("aria-disabled")).toBe("true");
    expect(dialog.textContent).toContain("Choose a file.");
    fireEvent.click(create);
    await waitFor(() => expect(checks).toEqual([]));
    expect(posts()).toEqual([]);
  });

  it("shows the file as its name and size, with Remove file", async () => {
    mount();
    const dialog = await open();
    chooseFile(dialog, longFile("compose.prod.yaml", "x".repeat(1536)));
    expect(await within(dialog).findByText("compose.prod.yaml")).toBeTruthy();
    expect(dialog.textContent).toContain("(1.5 KiB)");
    expect(within(dialog).getByRole("button", { name: "Remove file" })).toBeTruthy();
    expect(
      within(dialog).getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled"),
    ).toBeNull();
  });

  it("sends the file's text with start upload", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    chooseFile(dialog, longFile("compose.yaml", "services:\n  api:\n    image: x:1\n"));
    await within(dialog).findByText("compose.yaml");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      name: "web",
      start: "upload",
      compose: "services:\n  api:\n    image: x:1\n",
    });
  });

  it("takes a file that does not parse as the draft: the editor shows its problems", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    chooseFile(dialog, longFile("broken.yaml", "services: [oops"));
    await within(dialog).findByText("broken.yaml");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toMatchObject({ start: "upload", compose: "services: [oops" });
  });

  it("removes the file and disables Create stack again", async () => {
    mount();
    const dialog = await open();
    chooseFile(dialog, longFile("compose.yaml"));
    await within(dialog).findByText("compose.yaml");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove file" }));
    await waitFor(() => expect(within(dialog).queryByText("compose.yaml")).toBeNull());
    expect(
      within(dialog).getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(within(dialog).getByRole("button", { name: "Choose file" })).toBeTruthy();
  });

  it("refuses a file over 1 MiB inline and keeps nothing of it", async () => {
    mount();
    const dialog = await open();
    chooseFile(dialog, longFile("big.yaml", new Uint8Array(1024 * 1024 + 1)));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("That file is over 1 MiB. Choose a smaller one.");
    expect(within(dialog).queryByText("big.yaml")).toBeNull();
    expect(
      within(dialog).getByRole("button", { name: "Create stack" }).getAttribute("aria-disabled"),
    ).toBe("true");
  });

  it("refuses a file that cannot be read as text inline", async () => {
    mount();
    const dialog = await open();
    chooseFile(dialog, longFile("image.bin", new Uint8Array([0xff, 0xfe, 0xfd, 0x00])));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("Couldn't read that file. Choose another.");
    expect(within(dialog).queryByText("image.bin")).toBeNull();
  });

  it("takes the refusal away when another file is chosen", async () => {
    mount();
    const dialog = await open();
    chooseFile(dialog, longFile("big.yaml", new Uint8Array(1024 * 1024 + 1)));
    await within(dialog).findByRole("alert");
    chooseFile(dialog, longFile("ok.yaml"));
    await within(dialog).findByText("ok.yaml");
    expect(within(dialog).queryByRole("alert")).toBeNull();
  });

  it("cuts a long file name and keeps it whole in the text", async () => {
    mount();
    const dialog = await open();
    const name = `${"very-long-".repeat(12)}compose.yaml`;
    chooseFile(dialog, longFile(name));
    const shown = await within(dialog).findByText(name);
    expect(shown.className).toContain("truncate");
  });

  it("sends the empty file's text as it is, since an empty file was chosen", async () => {
    mount();
    const dialog = await open();
    typeName(dialog, "web");
    chooseFile(dialog, longFile("empty.yaml", ""));
    await within(dialog).findByText("empty.yaml");
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ name: "web", start: "upload", compose: "" });
  });
});

describe("CreateStackDialog on the All stacks list", () => {
  beforeEach(() => {
    me = makeMe({ envs: { e1: "admin", e2: "operator", e3: "admin" } });
    environments = [
      { id: "e1", name: "Prod", envType: "swarm" },
      { id: "e2", name: "Staging", envType: "standalone" },
      { id: "e3", name: "Lab", envType: "standalone" },
    ];
  });

  const pick = async (dialog: HTMLElement, name: string) => {
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Environment" }));
    fireEvent.click(await screen.findByRole("option", { name }));
  };

  it("asks for the environment, and offers only those where the user may create stacks", async () => {
    mount("global");
    const dialog = await open();
    const select = await within(dialog).findByRole("combobox", { name: "Environment" });
    expect(select.textContent).toContain("Choose an environment");
    fireEvent.click(select);
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "Prod",
      "Lab",
    ]);
    expect(dialog.textContent).not.toContain("On Prod");
  });

  it("asks for an environment before anything is sent", async () => {
    mount("global");
    const dialog = await open();
    typeName(dialog, "web");
    submit(dialog);
    expect(await within(dialog).findByText("Choose an environment.")).toBeTruthy();
    expect(posts()).toEqual([]);
    expect(checks).toEqual([]);
  });

  it("checks and creates on the environment that was chosen", async () => {
    mount("global");
    const dialog = await open();
    await pick(dialog, "Lab");
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    await waitFor(() => expect(checks).toEqual([{ envId: "e3", name: "web" }]));
    submit(dialog);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.path).toBe("/api/env/e3/stacks");
    expect(posts()[0]?.body).toEqual({ name: "web", start: "example" });
  });

  it("asks about the name again when the environment changes", async () => {
    mount("global");
    const dialog = await open();
    await pick(dialog, "Prod");
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    await waitFor(() => expect(checks).toHaveLength(1));
    await pick(dialog, "Lab");
    await waitFor(() =>
      expect(checks).toEqual([
        { envId: "e1", name: "web" },
        { envId: "e3", name: "web" },
      ]),
    );
  });

  it("names the environment in a taken name and goes to the new stack on its environment", async () => {
    checkAnswer = () =>
      Response.json({ status: "taken", message: "A stack named web already exists on Lab." });
    mount("global");
    const dialog = await open();
    await pick(dialog, "Lab");
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "A stack named web already exists on Lab.",
    );
  });

  it("uses the union rule until an environment is chosen, then the rule of its target", async () => {
    mount("global");
    const dialog = await open();
    const name = within(dialog).getByLabelText("Name");
    expect(name.getAttribute("maxlength")).toBe("255");
    expect(dialog.textContent).toContain(
      "Letters, numbers, dash or underscore. It can't change later.",
    );
    // No environment yet: a name no target takes is refused, one that some target takes is not.
    typeName(dialog, "Shop.Front");
    fireEvent.blur(name);
    expect(await within(dialog).findByText(UNION_SENTENCE)).toBeTruthy();
    typeName(dialog, "MyApp");
    fireEvent.blur(name);
    await waitFor(() => expect(within(dialog).queryByText(UNION_SENTENCE)).toBeNull());
    // A standalone host does not take MyApp: the filled name is judged again at once, with that rule.
    await pick(dialog, "Lab");
    expect(await within(dialog).findByText(STANDALONE_SENTENCE)).toBeTruthy();
    expect(name.getAttribute("maxlength")).toBe("255");
    // A swarm does.
    await pick(dialog, "Prod");
    await waitFor(() => expect(within(dialog).queryByText(STANDALONE_SENTENCE)).toBeNull());
    expect(name.getAttribute("maxlength")).toBe("61");
    expect(dialog.textContent).toContain("up to 61 characters. It can't change later.");
    expect(posts()).toEqual([]);
  });

  it("shows the sentence of the chosen environment's target when the server calls the name invalid", async () => {
    checkAnswer = () => Response.json({ status: "invalid", message: STANDALONE_SENTENCE });
    mount("global");
    const dialog = await open();
    await pick(dialog, "Lab");
    typeName(dialog, "web");
    fireEvent.blur(within(dialog).getByLabelText("Name"));
    expect(await within(dialog).findByText(STANDALONE_SENTENCE)).toBeTruthy();
  });

  it("preselects the environment when the user may create on only one", async () => {
    me = makeMe({ envs: { e1: "admin", e2: "operator", e3: "viewer" } });
    mount("global");
    const dialog = await open();
    await waitFor(() =>
      expect(within(dialog).getByRole("combobox", { name: "Environment" }).textContent).toContain(
        "Prod",
      ),
    );
  });
});
