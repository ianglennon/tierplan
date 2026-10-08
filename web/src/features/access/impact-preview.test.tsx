// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ImpactLoad, ImpactRow } from "@/api/teams";
import { ImpactPreview } from "@/features/access/impact-preview";
import "@/i18n";

afterEach(() => cleanup());

function row(over: Partial<ImpactRow> = {}): ImpactRow {
  return {
    userId: "u1",
    username: "alice",
    environmentId: "e1",
    environmentName: "Production",
    before: "operator",
    after: "viewer",
    ...over,
  };
}

// A load that settles when the test says so.
function deferred() {
  let resolve: (v: ImpactLoad) => void = () => {};
  const promise = new Promise<ImpactLoad>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

describe("ImpactPreview", () => {
  // WR-C-06: a confirm kept in the parent must not stay enabled once the preview is gone.
  it("says it is no longer ready when it goes away", async () => {
    const onReadyChange = vi.fn();
    const { unmount } = render(
      <ImpactPreview load={async () => ({ kind: "ok", rows: [] })} onReadyChange={onReadyChange} />,
    );
    await waitFor(() => expect(onReadyChange).toHaveBeenLastCalledWith(true));
    unmount();
    expect(onReadyChange).toHaveBeenLastCalledWith(false);
  });

  it("shows skeleton rows and is not ready until the preview has loaded", async () => {
    const d = deferred();
    const onReadyChange = vi.fn();
    render(<ImpactPreview load={() => d.promise} onReadyChange={onReadyChange} />);

    expect(screen.getByRole("heading", { name: "What changes" })).toBeTruthy();
    const busy = screen.getByText("Working out what changes").closest("[aria-busy]");
    expect(busy?.getAttribute("aria-busy")).toBe("true");
    expect(onReadyChange).not.toHaveBeenCalledWith(true);

    d.resolve({ kind: "ok", rows: [row()] });
    expect(await screen.findByText("alice")).toBeTruthy();
    expect(onReadyChange).toHaveBeenLastCalledWith(true);
  });

  it("reads username, environment and Before → After, with No access when it ends", async () => {
    render(
      <ImpactPreview
        load={async () => ({
          kind: "ok",
          rows: [
            row(),
            row({
              username: "bob",
              userId: "u2",
              environmentId: "*",
              environmentName: "All environments",
              before: "admin",
              after: "none",
            }),
          ],
        })}
      />,
    );
    const alice = (await screen.findByText("alice")).closest("li") as HTMLElement;
    expect(within(alice).getByText("Production")).toBeTruthy();
    expect(within(alice).getByText("Operator → Viewer")).toBeTruthy();
    const bob = screen.getByText("bob").closest("li") as HTMLElement;
    expect(within(bob).getByText("All environments")).toBeTruthy();
    expect(within(bob).getByText("Admin → No access")).toBeTruthy();
    expect(
      screen.getByText("Recorded as one entry in the audit log, with this list attached."),
    ).toBeTruthy();
  });

  it("announces how many people lose access in a polite live region", async () => {
    render(
      <ImpactPreview
        load={async () => ({
          kind: "ok",
          rows: [
            row(),
            row({ environmentId: "e2", environmentName: "Staging" }),
            row({ userId: "u2", username: "bob" }),
          ],
        })}
      />,
    );
    await screen.findByText("bob");
    const live = document.querySelector('[aria-live="polite"]') as HTMLElement;
    // Two people, though three rows.
    expect(live.textContent).toBe("2 people lose access");
    expect(live.className).toContain("sr-only");
  });

  it("says No one loses access and is ready when the change affects no one", async () => {
    const onReadyChange = vi.fn();
    render(
      <ImpactPreview load={async () => ({ kind: "ok", rows: [] })} onReadyChange={onReadyChange} />,
    );
    expect(await screen.findAllByText("No one loses access.")).toBeTruthy();
    expect(onReadyChange).toHaveBeenLastCalledWith(true);
    expect(document.querySelector('[aria-live="polite"]')?.textContent).toBe("No one loses access");
  });

  it("fails closed: an inline alert with Retry, not ready, and Retry loads again", async () => {
    const onReadyChange = vi.fn();
    const load = vi
      .fn<() => Promise<ImpactLoad>>()
      .mockResolvedValueOnce({ kind: "error" })
      .mockResolvedValueOnce({ kind: "ok", rows: [row()] });
    render(<ImpactPreview load={load} onReadyChange={onReadyChange} />);

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Couldn't work out what changes. Try again.")).toBeTruthy();
    expect(onReadyChange).not.toHaveBeenCalledWith(true);

    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("alice")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(onReadyChange).toHaveBeenLastCalledWith(true);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("treats a load that throws like a failure", async () => {
    render(<ImpactPreview load={() => Promise.reject(new Error("offline"))} />);
    expect(await screen.findByText("Couldn't work out what changes. Try again.")).toBeTruthy();
  });

  it("scrolls a long list inside 240px and never truncates it", async () => {
    const rows = Array.from({ length: 60 }, (_, i) =>
      row({ userId: `u${i}`, username: `user-${String(i).padStart(2, "0")}` }),
    );
    render(<ImpactPreview load={async () => ({ kind: "ok", rows })} />);
    await screen.findByText("user-00");
    const list = screen.getByRole("list", { name: "People who lose access" });
    expect(list.className).toContain("max-h-60"); // 15rem, 240px
    expect(list.className).toContain("overflow-y-auto");
    expect(within(list).getAllByRole("listitem")).toHaveLength(60);
    await waitFor(() => expect(screen.getByText("user-59")).toBeTruthy());
  });

  it("asks once when it opens and does not ask again for a re-render", async () => {
    const load = vi.fn(async (): Promise<ImpactLoad> => ({ kind: "ok", rows: [] }));
    const { rerender } = render(<ImpactPreview load={load} />);
    await screen.findAllByText("No one loses access.");
    rerender(<ImpactPreview load={load} />);
    expect(load).toHaveBeenCalledTimes(1);
  });
});
