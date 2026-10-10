// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { shellPreferenceKey } from "@/api/shells";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeWebSocket } from "@/features/terminal/fake-web-socket";
import { ShellPanel } from "@/features/terminal/shell-panel";
import type { TerminalProps } from "@/features/terminal/terminal";
import "@/i18n";
import { makeMe } from "@/test-me";

// The launcher (plan 04-24, D-26, D-27, UI-SPEC "Shell"): the five shells, a custom
// command, the collapsed Options, and the last choice for an image preselected from the
// server. xterm needs a real layout, so the terminal is a stand-in.

vi.mock("@/features/terminal/terminal", () => ({
  Terminal: (props: TerminalProps) => <section aria-label={`Terminal in ${props.name}`} />,
}));

const ENV = "e1";
const IMAGE = "nginx:1.27";

interface Sent {
  method: string;
  path: string;
  query: string;
  body?: Record<string, unknown>;
}
let sent: Sent[];
let preference: () => Response | Promise<Response>;

beforeEach(() => {
  sent = [];
  preference = () => Response.json({ code: "shell_preference_not_found" }, { status: 404 });
  FakeWebSocket.reset();
  setCsrfToken(null);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  const me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["containers.exec"] } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path: url.pathname, query: url.search, body });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/me/shell-preferences") return preference();
      if (url.pathname === `/api/env/${ENV}/containers/web-id/shell`) {
        return Response.json(
          {
            sessionId: "SID1",
            wsPath: `/api/env/${ENV}/shell-sessions/SID1/ws`,
            command: ["/bin/sh"],
            user: "",
            workingDir: "",
          },
          { status: 201 },
        );
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
});

function renderPanel(props: Partial<React.ComponentProps<typeof ShellPanel>> = {}) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <ShellPanel
          envId={ENV}
          containerId="web-id"
          nodeId="n1"
          name="web"
          image={IMAGE}
          running
          createSocket={(url) => new FakeWebSocket(url)}
          {...props}
        />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const posts = () => sent.filter((s) => s.method === "POST");
const shellSelect = () => screen.getByRole("combobox", { name: "Shell" });
const openSelect = () => fireEvent.keyDown(shellSelect(), { key: "Enter" });

async function permissionsLoaded() {
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
  await act(async () => {});
}

async function submit() {
  const button = await screen.findByRole("button", { name: "Open shell" });
  await permissionsLoaded();
  fireEvent.click(button);
}

describe("the shell picker", () => {
  it("offers Automatic, bash, sh, ash and Custom command, with Automatic first", async () => {
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    expect(shellSelect().textContent).toContain("Automatic (bash, then sh)");
    openSelect();
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["Automatic (bash, then sh)", "bash", "sh", "ash", "Custom command…"]);
  });

  it("shows the Command field only for a custom shell, with its helper, and focuses it", async () => {
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    expect(screen.queryByLabelText("Command")).toBeNull();
    openSelect();
    fireEvent.click(await screen.findByRole("option", { name: "Custom command…" }));
    const field = await screen.findByLabelText("Command");
    expect(screen.getByText("The command to run, for example /bin/zsh.")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(field));
    // Without a command there is nothing to run.
    expect(screen.getByRole("button", { name: "Open shell" }).hasAttribute("disabled")).toBe(true);
    fireEvent.change(field, { target: { value: "  /bin/zsh -l  " } });
    expect(screen.getByRole("button", { name: "Open shell" }).hasAttribute("disabled")).toBe(false);
  });

  it("opens a custom shell with the command split by the server, trimmed here, and never as sh -c", async () => {
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    openSelect();
    fireEvent.click(await screen.findByRole("option", { name: "Custom command…" }));
    fireEvent.change(await screen.findByLabelText("Command"), {
      target: { value: "  /bin/zsh -l  " },
    });
    await submit();
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      nodeId: "n1",
      shell: "custom",
      command: "/bin/zsh -l",
      cols: 80,
      rows: 24,
    });
  });
});

describe("the options", () => {
  it("are collapsed, and hold only User and Working directory", async () => {
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    expect(screen.queryByLabelText("User")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Options" }));
    const user = await screen.findByLabelText("User");
    expect(user.getAttribute("placeholder")).toBe("Container default");
    expect(screen.getByText("For example root, 0 or www-data.")).toBeTruthy();
    expect(screen.getByLabelText("Working directory").getAttribute("placeholder")).toBe(
      "Container default",
    );
    // No way to ask for environment variables or a privileged exec (D-27).
    expect(screen.queryByLabelText(/environment/i)).toBeNull();
    expect(screen.queryByLabelText(/privileged/i)).toBeNull();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("send the user and the working directory with the open, and leave out what is empty", async () => {
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    openSelect();
    fireEvent.click(await screen.findByRole("option", { name: "bash" }));
    fireEvent.click(screen.getByRole("button", { name: "Options" }));
    fireEvent.change(await screen.findByLabelText("User"), { target: { value: " root " } });
    fireEvent.change(screen.getByLabelText("Working directory"), { target: { value: "/var/www" } });
    await submit();
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({
      nodeId: "n1",
      shell: "bash",
      user: "root",
      workingDir: "/var/www",
      cols: 80,
      rows: 24,
    });
  });
});

describe("the remembered choice (D-26, UI-SPEC Defaults 19)", () => {
  it("asks the server for the container's image and preselects the answer, with Options open", async () => {
    preference = () =>
      Response.json({ shell: "bash", command: "", user: "root", workingDir: "/srv" });
    renderPanel();
    expect(((await screen.findByLabelText("User")) as HTMLInputElement).value).toBe("root");
    expect(shellSelect().textContent).toBe("bash");
    expect((screen.getByLabelText("Working directory") as HTMLInputElement).value).toBe("/srv");
    const ask = sent.find((s) => s.path === "/api/me/shell-preferences");
    expect(new URLSearchParams(ask?.query).get("image")).toBe(IMAGE);
  });

  it("preselects a custom command, and the open sends it again", async () => {
    preference = () =>
      Response.json({ shell: "custom", command: "/opt/bin/zsh", user: "", workingDir: "" });
    renderPanel();
    expect(((await screen.findByLabelText("Command")) as HTMLInputElement).value).toBe(
      "/opt/bin/zsh",
    );
    expect(shellSelect().textContent).toContain("Custom command…");
    await submit();
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toMatchObject({ shell: "custom", command: "/opt/bin/zsh" });
  });

  it("starts from Automatic when nothing is remembered for the image (404)", async () => {
    renderPanel();
    await waitFor(() =>
      expect(sent.some((s) => s.path === "/api/me/shell-preferences")).toBe(true),
    );
    await act(async () => {});
    expect(shellSelect().textContent).toContain("Automatic (bash, then sh)");
    expect(screen.queryByLabelText("User")).toBeNull();
  });

  it("starts from Automatic, and still opens, when the preference cannot be read", async () => {
    preference = () => Response.json({}, { status: 500 });
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    await act(async () => {});
    expect(shellSelect().textContent).toContain("Automatic (bash, then sh)");
    await submit();
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toMatchObject({ shell: "auto" });
  });

  it("does not ask without an image", async () => {
    renderPanel({ image: undefined });
    await screen.findByRole("button", { name: "Open shell" });
    await act(async () => {});
    expect(sent.some((s) => s.path === "/api/me/shell-preferences")).toBe(false);
  });

  it("does not overwrite what the person has already changed when the answer arrives late", async () => {
    let release: (r: Response) => void = () => {};
    preference = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    openSelect();
    fireEvent.click(await screen.findByRole("option", { name: "ash" }));
    await act(async () => {
      release(Response.json({ shell: "bash", command: "", user: "root", workingDir: "" }));
    });
    expect(shellSelect().textContent).toContain("ash");
    expect(screen.queryByLabelText("User")).toBeNull();
  });

  it("is updated in the page at once by a successful open, so the next form starts from it", async () => {
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    openSelect();
    fireEvent.click(await screen.findByRole("option", { name: "sh" }));
    fireEvent.click(screen.getByRole("button", { name: "Options" }));
    fireEvent.change(await screen.findByLabelText("User"), { target: { value: "www-data" } });
    await submit();
    await waitFor(() => expect(queryClient.getQueryData(shellPreferenceKey(IMAGE))).toBeTruthy());
    expect(queryClient.getQueryData(shellPreferenceKey(IMAGE))).toEqual({
      shell: "sh",
      command: "",
      user: "www-data",
      workingDir: "",
    });
  });

  it("is used again by Start a new session", async () => {
    renderPanel();
    await screen.findByRole("button", { name: "Open shell" });
    openSelect();
    fireEvent.click(await screen.findByRole("option", { name: "ash" }));
    await submit();
    await waitFor(() => expect(FakeWebSocket.instances).toHaveLength(1));
    const socket = FakeWebSocket.instances[0] as FakeWebSocket;
    act(() => socket.open());
    act(() => socket.control({ type: "exit", code: 0 }));
    const again = await screen.findByRole("button", { name: "Start a new session" });
    fireEvent.click(again);
    await waitFor(() => expect(posts()).toHaveLength(2));
    expect(posts()[1]?.body).toMatchObject({ shell: "ash" });
    expect(within(document.body).queryByRole("combobox", { name: "Shell" })).toBeNull();
  });
});
