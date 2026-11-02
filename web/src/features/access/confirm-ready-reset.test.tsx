// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { Grant } from "@/api/grants";
import { queryClient } from "@/api/query-client";
import { DeleteTeamDialog } from "@/features/access/delete-team-dialog";
import { RemoveGrantDialog } from "@/features/access/remove-grant-dialog";
import { RemoveMemberDialog } from "@/features/access/remove-member-dialog";
import "@/i18n";

// The preview stands in as a button that reports itself ready and, like the real
// one on a slow cache or a deferred effect, says nothing when it goes away. The
// confirm must still not carry "ready" from one target to the next (WR-C-06).
vi.mock("@/features/access/impact-preview", () => ({
  ImpactPreview: ({ onReadyChange }: { onReadyChange?: (ready: boolean) => void }) => (
    <button type="button" onClick={() => onReadyChange?.(true)}>
      load preview
    </button>
  ),
}));

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({}, { status: 404 })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
});

const confirm = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;
const grant = (id: string): Grant => ({
  id,
  subjectType: "user",
  subjectId: "u1",
  scope: "env",
  envId: `env-${id}`,
  envName: `Env ${id}`,
  role: "viewer",
  extras: [],
  createdAt: "2026-10-08T10:00:00Z",
});

function wrap(node: React.ReactNode) {
  return <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>;
}

describe("a destructive confirm is never ready for a target whose impact has not loaded", () => {
  it("Remove access: a different grant starts not ready", () => {
    const subject = { type: "user", id: "u1", name: "alice" } as const;
    const ui = (g: Grant | null) =>
      wrap(
        <RemoveGrantDialog
          subject={subject}
          grant={g}
          onClose={() => {}}
          onDone={() => {}}
          onStale={() => {}}
        />,
      );
    const { rerender } = render(ui(grant("a")));
    expect(confirm("Remove access").getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(screen.getByText("load preview"));
    expect(confirm("Remove access").getAttribute("aria-disabled")).toBe("false");

    rerender(ui(grant("b")));
    expect(confirm("Remove access").getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(screen.getByText("load preview"));
    expect(confirm("Remove access").getAttribute("aria-disabled")).toBe("false");
  });

  it("Remove member: a different membership starts not ready", () => {
    const m = (user: string) => ({
      team: { id: "t1", name: "Ops" },
      user: { id: user, username: user },
    });
    const ui = (v: ReturnType<typeof m> | null) =>
      wrap(
        <RemoveMemberDialog
          membership={v}
          onClose={() => {}}
          onDone={() => {}}
          onStale={() => {}}
        />,
      );
    const { rerender } = render(ui(m("u1")));
    fireEvent.click(screen.getByText("load preview"));
    expect(confirm("Remove from team").getAttribute("aria-disabled")).toBe("false");
    rerender(ui(m("u2")));
    expect(confirm("Remove from team").getAttribute("aria-disabled")).toBe("true");
  });

  it("Delete team: a different team starts not ready", () => {
    const ui = (v: { id: string; name: string } | null) =>
      wrap(<DeleteTeamDialog team={v} onClose={() => {}} onDone={() => {}} onStale={() => {}} />);
    const { rerender } = render(ui({ id: "t1", name: "Ops" }));
    fireEvent.click(screen.getByText("load preview"));
    expect(confirm("Delete team").getAttribute("aria-disabled")).toBe("false");
    rerender(ui({ id: "t2", name: "Dev" }));
    expect(confirm("Delete team").getAttribute("aria-disabled")).toBe("true");
  });
});
