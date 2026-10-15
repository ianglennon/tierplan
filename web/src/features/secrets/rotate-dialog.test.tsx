// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RotateDialog } from "@/features/secrets/rotate-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The two-step rotate dialog (plan 06-28, RES-08, D-26, UI-SPEC "Secrets and configs", E13): step 1
// asks for the new name, the value and whether to remove the old object; step 2 lists the services
// with a diff each and the stack-owned ones as skipped; the run follows the server through creating,
// updating and removing, and ends with the done, partial or failed copy. The value lives in state
// only and is cleared when the dialog closes and when the run succeeds.

type Preview = components["schemas"]["RotatePreviewBody"];
type Rotated = components["schemas"]["RotateBody"];

const ENV = "e1";
const VALUE = "s3cret-new-value-7c1d"; //nolint — a fixture value the tests look for everywhere

interface Sent {
  method: string;
  path: string;
  body: unknown;
}

let sent: Sent[];
let answerPreview: (body: { newName?: string }) => Response | Promise<Response>;
let answerRotate: (body: Record<string, unknown>) => Response | Promise<Response>;
let onClose: Mock<() => void>;
let client: QueryClient;

const problem = (
  status: number,
  type: string,
  errors: { location: string; message: string }[] = [],
) => Response.json({ type, title: "x", status, detail: "d", errors }, { status });

const specOf = (secret: string) => ({
  Name: "svc",
  TaskTemplate: {
    ContainerSpec: {
      Image: "app:1",
      Secrets: [
        { SecretName: secret, File: { Name: "db_password", UID: "0", GID: "0", Mode: 292 } },
      ],
    },
  },
});

function previewOf(name: string, extra: Partial<Preview> = {}): Preview {
  const user = (service: string, stack = "") => ({
    serviceId: `svc-${service}`,
    service,
    stack,
    skipped: stack !== "",
    file: "db_password",
    uid: "0",
    gid: "0",
    mode: "0444",
    ...(stack === "" ? { current: specOf("db_password"), next: specOf(name) } : {}),
  });
  return {
    kind: "secret",
    id: "sid-db",
    name: "db_password",
    newName: name,
    nameTaken: false,
    templating: "",
    labels: {},
    users: [user("api"), user("shop_web", "shop"), user("worker")],
    skipped: 1,
    ...extra,
  };
}

const rotated = (extra: Partial<Rotated> = {}): Rotated => ({
  id: "sid-new",
  name: "db_password_v2",
  oldName: "db_password",
  services: [
    {
      serviceId: "svc-api",
      service: "api",
      stack: "",
      state: "updated",
      image: "app:1",
      digest: "",
    },
    {
      serviceId: "svc-shop",
      service: "shop_web",
      stack: "shop",
      state: "skipped",
      image: "",
      digest: "",
    },
    {
      serviceId: "svc-worker",
      service: "worker",
      stack: "",
      state: "updated",
      image: "app:1",
      digest: "",
    },
  ],
  updated: 2,
  skipped: 1,
  oldRemoved: false,
  removeCode: "object_in_use",
  ...extra,
});

beforeEach(() => {
  sent = [];
  onClose = vi.fn<() => void>();
  answerPreview = (body) => Response.json(previewOf(body.newName || "db_password_v2"));
  answerRotate = () => Response.json(rotated());
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 1 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const text = req.method === "POST" ? await req.text() : "";
      const body = text ? JSON.parse(text) : undefined;
      sent.push({ method: req.method, path: url.pathname, body });
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (url.pathname === `/api/env/${ENV}/secrets/sid-db/rotate/preview`)
        return answerPreview(body ?? {});
      if (url.pathname === `/api/env/${ENV}/secrets/sid-db/rotate`) return answerRotate(body ?? {});
      if (url.pathname === `/api/env/${ENV}/configs/cid-app/rotate/preview`) {
        return Response.json(
          previewOf(body?.newName || "app.conf_v2", {
            kind: "config",
            id: "cid-app",
            name: "app.conf",
            templating: "golang",
          }),
        );
      }
      if (url.pathname === `/api/env/${ENV}/configs/cid-app/rotate`)
        return answerRotate(body ?? {});
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm", envType: "swarm" }] });
      }
      return problem(404, "service_not_found");
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(props: { kind?: "secret" | "config" } = {}) {
  const kind = props.kind ?? "secret";
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RotateDialog
            kind={kind}
            envId={ENV}
            object={
              kind === "config"
                ? { id: "cid-app", name: "app.conf" }
                : { id: "sid-db", name: "db_password" }
            }
            onClose={onClose}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const rotates = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/rotate"));
const previews = () => sent.filter((s) => s.path.endsWith("/rotate/preview"));

async function typeValue(value = VALUE) {
  fireEvent.change(await screen.findByLabelText("Value"), { target: { value } });
}

async function toReview(value = VALUE) {
  await screen.findByDisplayValue("db_password_v2");
  await typeValue(value);
  fireEvent.click(screen.getByRole("button", { name: "Review services" }));
}

describe("RotateDialog step 1", () => {
  it("says Step 1 of 2, prefills the next version of the name from the server and ticks Remove", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "New version of db_password" })).toBeTruthy();
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
    expect(
      screen.getByText(
        "Creates a new secret with the value you give, points the services that use db_password at it, then removes db_password once nothing uses it.",
      ),
    ).toBeTruthy();
    expect(await screen.findByDisplayValue("db_password_v2")).toBeTruthy();
    expect(
      screen.getByText(
        "Services keep their file names, owners and modes. Only the object changes.",
      ),
    ).toBeTruthy();
    const remove = screen.getByRole("checkbox", {
      name: "Remove db_password when nothing uses it",
    });
    expect(remove.getAttribute("aria-checked")).toBe("true");
    expect(previews()[0]?.body).toEqual({});
  });

  it("asks for a value before Review services, with the field errors of E13", async () => {
    mount();
    await screen.findByDisplayValue("db_password_v2");
    fireEvent.click(screen.getByRole("button", { name: "Review services" }));
    expect(await screen.findByText("Enter a value.")).toBeTruthy();
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("New name"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Review services" }));
    expect(await screen.findByText("Enter a name.")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("New name"), { target: { value: "-bad" } });
    fireEvent.click(screen.getByRole("button", { name: "Review services" }));
    expect(await screen.findByText("Use letters, numbers, dot, dash or underscore.")).toBeTruthy();
    expect(rotates()).toHaveLength(0);
  });

  it("uses a text area and a Templating field for a config, prefilled from the old one", async () => {
    mount({ kind: "config" });
    expect(await screen.findByDisplayValue("app.conf_v2")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "New version of app.conf" })).toBeTruthy();
    expect(screen.getByText(/Creates a new config with the value you give/)).toBeTruthy();
    expect(screen.getByRole("combobox", { name: /Templating/ }).textContent).toContain(
      "Go template",
    );
  });
});

describe("RotateDialog step 2", () => {
  it("lists the services in order with Will use, the stack ones as skipped and an info alert", async () => {
    mount();
    await toReview();
    expect(await screen.findByText("Step 2 of 2")).toBeTruthy();
    const table = await screen.findByRole("table", { name: "Services that use db_password" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.querySelector("td")?.textContent)).toEqual([
      "api",
      "shop_web",
      "worker",
    ]);
    expect(within(rows[0] as HTMLElement).getByText("Will use db_password_v2")).toBeTruthy();
    expect(
      within(rows[1] as HTMLElement).getByText("Skipped. Managed by stack shop."),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "1 service belongs to a stack, so Bayplan can't change it from here. db_password stays until it uses the new version.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Create new version" })).toBeTruthy();
  });

  it("is plural-aware about the services that belong to stacks", async () => {
    answerPreview = (body) =>
      Response.json({
        ...previewOf(body.newName || "db_password_v2"),
        users: [
          previewOf("x").users?.[1],
          { ...previewOf("x").users?.[1], serviceId: "svc-b", service: "b" },
        ],
        skipped: 2,
      });
    mount();
    await toReview();
    expect(
      await screen.findByText(
        "2 services belong to stacks, so Bayplan can't change them from here. db_password stays until they use the new version.",
      ),
    ).toBeTruthy();
  });

  it("opens the diff of a standalone service from its expander and not for a skipped one", async () => {
    mount();
    await toReview();
    const table = await screen.findByRole("table", { name: "Services that use db_password" });
    expect(within(table).queryByRole("button", { name: "Show changes to shop_web" })).toBeNull();
    const expander = within(table).getByRole("button", { name: "Show changes to api" });
    expect(expander.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(expander);
    expect(
      screen.getByRole("button", { name: "Hide changes to api" }).getAttribute("aria-expanded"),
    ).toBe("true");
    expect(await screen.findByRole("table", { name: "Changes to api" })).toBeTruthy();
    expect(screen.getAllByText(/db_password_v2/).length).toBeGreaterThan(1);
  });

  it("says only the new secret is created when nothing uses the old one", async () => {
    answerPreview = (body) =>
      Response.json(previewOf(body.newName || "db_password_v2", { users: [], skipped: 0 }));
    mount();
    await toReview();
    expect(
      await screen.findByText("Nothing uses db_password, so only the new secret is created."),
    ).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Services that use db_password" })).toBeNull();
  });

  it("reads the services again under the name that was typed, with a skeleton while they load", async () => {
    let release: (res: Response) => void = () => {};
    answerPreview = (body) =>
      body.newName === "db_password_2026"
        ? new Promise<Response>((resolve) => {
            release = resolve;
          })
        : Response.json(previewOf("db_password_v2"));
    mount();
    await screen.findByDisplayValue("db_password_v2");
    fireEvent.change(screen.getByLabelText("New name"), { target: { value: "db_password_2026" } });
    await typeValue();
    fireEvent.click(screen.getByRole("button", { name: "Review services" }));
    const skeleton = await screen.findByRole("status", { name: "Loading the services" });
    expect(skeleton.getAttribute("aria-busy")).toBe("true");
    release(Response.json(previewOf("db_password_2026")));
    expect(await screen.findAllByText("Will use db_password_2026")).toHaveLength(2);
    expect(previews().map((p) => (p.body as { newName?: string }).newName)).toEqual([
      undefined,
      "db_password_2026",
    ]);
  });

  it("refuses a taken name inline and does not send", async () => {
    answerPreview = (body) =>
      Response.json(previewOf(body.newName || "db_password_v2", { nameTaken: true }));
    mount();
    await toReview();
    expect(
      await screen.findByText(
        "A secret named db_password_v2 already exists. Go back and choose another name.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Create new version" }));
    expect(rotates()).toHaveLength(0);
  });

  it("goes back to step 1 with the typed value kept", async () => {
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Back to step 1" }));
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
    expect((screen.getByLabelText("Value") as HTMLInputElement).value).toBe(VALUE);
  });
});

describe("RotateDialog run", () => {
  it("sends the value once as base64 with the name and the remove choice, and says Rotated…", async () => {
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    expect(
      await screen.findByText("Rotated db_password to db_password_v2. Updated 2 services."),
    ).toBeTruthy();
    expect(rotates()).toHaveLength(1);
    expect(rotates()[0]?.body).toEqual({
      name: "db_password_v2",
      data: btoa(VALUE),
      removeOld: true,
    });
    const steps = screen.getByRole("list", { name: "Steps of the new version" });
    expect(within(steps).getByText("Create db_password_v2")).toBeTruthy();
    expect(within(steps).getByText("Update api")).toBeTruthy();
    expect(within(steps).getByText("Update worker")).toBeTruthy();
    expect(within(steps).queryByText("Update shop_web")).toBeNull();
    expect(within(steps).getByText("Remove db_password")).toBeTruthy();
    expect(
      screen.getByText("db_password stays until the services of your stacks use db_password_v2."),
    ).toBeTruthy();
    // Nothing of the value is left on the page.
    expect(document.body.innerHTML).not.toContain(VALUE);
    expect(document.body.innerHTML).not.toContain(btoa(VALUE));
  });

  it("shows the first step working and the rest waiting while the request is out", async () => {
    answerRotate = () => new Promise<Response>(() => {});
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    const steps = await screen.findByRole("list", { name: "Steps of the new version" });
    const items = within(steps).getAllByRole("listitem");
    expect(items[0]?.textContent).toContain("Working");
    expect(items[1]?.textContent).toContain("Waiting");
    expect(items.at(-1)?.textContent).toContain("Waiting");
    // Nothing closes the dialog while the request is out.
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    expect(onClose).not.toHaveBeenCalled();
  });

  it("says Updated 1 of 2 services when the run stopped part-way, and which one failed", async () => {
    answerRotate = () =>
      problem(409, "service_changed", [
        { location: "reason", message: "d" },
        { location: "created", message: "db_password_v2" },
        { location: "updated", message: "api" },
        { location: "skipped", message: "shop_web" },
        { location: "failedAt", message: "worker" },
      ]);
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    expect(
      await screen.findByText(
        "Updated 1 of 2 services. db_password stays until the rest use db_password_v2.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText("worker was changed by someone else while this ran. Nothing was retried."),
    ).toBeTruthy();
    const steps = screen.getByRole("list", { name: "Steps of the new version" });
    const worker = within(steps).getByText("Update worker").closest("li") as HTMLElement;
    expect(worker.textContent).toContain("Failed");
    expect(
      (within(steps).getByText("Update api").closest("li") as HTMLElement).textContent,
    ).toContain("Done");
    // There is no way back to the form: the new secret exists.
    expect(screen.queryByRole("button", { name: "Back to step 1" })).toBeNull();
  });

  it("says Couldn't create… Nothing else changed. and goes back with the value kept", async () => {
    answerRotate = () => problem(409, "name_taken", [{ location: "reason", message: "x" }]);
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    expect(
      await screen.findByText(
        "Couldn't create db_password_v2. A secret named db_password_v2 already exists. Nothing else changed.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to step 1" }));
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
    expect((screen.getByLabelText("Value") as HTMLInputElement).value).toBe(VALUE);
  });

  it("words a value that was too big without ever showing the problem type", async () => {
    answerRotate = () => problem(422, "value_too_large");
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    expect(await screen.findByText(/Values can be at most 500 KB\./)).toBeTruthy();
    expect(screen.queryByText(/value_too_large/)).toBeNull();
  });

  it("says Created… when nothing used the old secret", async () => {
    answerPreview = (body) =>
      Response.json(previewOf(body.newName || "db_password_v2", { users: [], skipped: 0 }));
    answerRotate = () =>
      Response.json(
        rotated({ services: [], updated: 0, skipped: 0, oldRemoved: false, removeCode: "" }),
      );
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    expect(
      await screen.findByText(
        "Created db_password_v2. Nothing used db_password, so no service was changed.",
      ),
    ).toBeTruthy();
  });

  it("says Updated 1 service in the singular and marks the old secret as removed when it was", async () => {
    answerRotate = () =>
      Response.json(
        rotated({
          services: [
            {
              serviceId: "svc-api",
              service: "api",
              stack: "",
              state: "updated",
              image: "app:1",
              digest: "",
            },
          ],
          updated: 1,
          skipped: 0,
          oldRemoved: true,
          removeCode: "",
        }),
      );
    mount();
    await toReview();
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    expect(
      await screen.findByText("Rotated db_password to db_password_v2. Updated 1 service."),
    ).toBeTruthy();
    const remove = screen.getByText("Remove db_password").closest("li") as HTMLElement;
    expect(remove.textContent).toContain("Done");
  });

  it("sends a config's templating and says config in the words", async () => {
    answerRotate = () =>
      Response.json(
        rotated({ name: "app.conf_v2", oldName: "app.conf", oldRemoved: true, removeCode: "" }),
      );
    mount({ kind: "config" });
    await screen.findByDisplayValue("app.conf_v2");
    fireEvent.change(await screen.findByLabelText("Content"), { target: { value: "level=debug" } });
    fireEvent.click(screen.getByRole("button", { name: "Review services" }));
    fireEvent.click(await screen.findByRole("button", { name: "Create new version" }));
    await waitFor(() => expect(rotates()).toHaveLength(1));
    expect(rotates()[0]?.body).toEqual({
      name: "app.conf_v2",
      data: btoa("level=debug"),
      removeOld: true,
      templating: "golang",
    });
    expect(await screen.findByText(/Rotated app\.conf to app\.conf_v2\./)).toBeTruthy();
  });
});

describe("RotateDialog closing", () => {
  it("asks its owner to close on Cancel, and keeps nothing of the value for the next time", async () => {
    mount();
    await screen.findByDisplayValue("db_password_v2");
    await typeValue();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    cleanup();
    mount();
    await screen.findByDisplayValue("db_password_v2");
    expect((screen.getByLabelText("Value") as HTMLInputElement).value).toBe("");
  });
});
