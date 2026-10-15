// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ServiceReview } from "@/api/services";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ReviewStep } from "@/features/service-editor/review-step";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import "@/i18n";
import { DIGEST, editSpec, reviewBody } from "@/test-service";

// The review step (plan 06-19, D-03, D-08, D-09; UI-SPEC "Review and apply"): the field-level
// difference between the spec as it was loaded and the one the server judged, with the server's
// refusals and warnings.

const NEW_IMAGE = `registry.example.com/shop/api:1.5@sha256:${"ab".repeat(32)}`;

let onBack: ReturnType<typeof vi.fn<() => void>>;
let onApply: ReturnType<typeof vi.fn<() => void>>;

beforeEach(() => {
  onBack = vi.fn<() => void>();
  onApply = vi.fn<() => void>();
});

afterEach(() => cleanup());

function withReplicas(spec: SpecValue, replicas: number): SpecValue {
  return { ...spec, Mode: { Replicated: { Replicas: replicas } } };
}

function withImage(spec: SpecValue, image: string): SpecValue {
  const tt = spec.TaskTemplate as { ContainerSpec: Record<string, unknown> };
  return { ...spec, TaskTemplate: { ...tt, ContainerSpec: { ...tt.ContainerSpec, Image: image } } };
}

interface Options {
  current?: SpecValue;
  edited?: SpecValue;
  review?: ServiceReview | null;
  applying?: boolean;
}

function renderStep({ current, edited, review, applying }: Options = {}) {
  const changed = withReplicas(editSpec(), 5);
  return render(
    <TooltipProvider>
      <ReviewStep
        envId="e1"
        name="api"
        current={current ?? editSpec()}
        edited={edited ?? changed}
        review={review === null ? undefined : (review ?? reviewBody({ spec: changed }))}
        applying={applying ?? false}
        onBack={onBack}
        onApply={onApply}
      />
    </TooltipProvider>,
  );
}

const apply = () => screen.getByRole("button", { name: /^(Apply changes|Applying…)$/ });

describe("ReviewStep", () => {
  it("says what it is and shows the changes against the spec as it was loaded", () => {
    renderStep();
    expect(screen.getByRole("heading", { name: "Review changes" })).toBeTruthy();
    expect(screen.getByText("Check what will change on api, then apply.")).toBeTruthy();
    const table = screen.getByRole("table", { name: "Changes to api" });
    const row = within(table).getByText("Replicas").closest("tr");
    expect(row?.textContent).toContain("Changed");
    expect(row?.textContent).toContain("3");
    expect(row?.textContent).toContain("5");
  });

  it("groups the changes under the cards of the form", () => {
    renderStep();
    expect(screen.getByText("Basics")).toBeTruthy();
  });

  it("shows the image as it was and as it will be, digest and all", () => {
    const next = withImage(withReplicas(editSpec(), 5), NEW_IMAGE);
    renderStep({
      edited: next,
      review: reviewBody({
        spec: next,
        image: {
          ref: NEW_IMAGE,
          tag: "1.5",
          digest: `sha256:${"ab".repeat(32)}`,
          host: "registry.example.com",
          credential: "ci-reader",
          changed: true,
        },
      }),
    });
    const row = screen.getByRole("group", { name: "Image" });
    expect(within(row).getByText(`registry.example.com/shop/api:1.4@${DIGEST}`)).toBeTruthy();
    expect(within(row).getByText(NEW_IMAGE)).toBeTruthy();
    expect(within(row).getByText("Credential: “ci-reader”")).toBeTruthy();
    expect(screen.queryByText("Looking up the digest…")).toBeNull();
  });

  it("shows a skeleton and Looking up the digest… while the review has not answered, and cannot be applied", () => {
    const next = withImage(editSpec(), "registry.example.com/shop/api:1.5");
    renderStep({ edited: next, review: null });
    const row = screen.getByRole("group", { name: "Image" });
    expect(within(row).getByText("Looking up the digest…")).toBeTruthy();
    expect(row.querySelector('[data-slot="skeleton"]')).not.toBeNull();
    expect(apply().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(apply());
    expect(onApply).not.toHaveBeenCalled();
    // The difference already shows what the person typed.
    expect(screen.getByRole("table", { name: "Changes to api" })).toBeTruthy();
  });

  it("shows a withheld value as bullets on the current side and says a replaced one was replaced, never the value", () => {
    const secret = "hunter2-very-secret";
    const typed = editSpec({
      TaskTemplate: {
        ContainerSpec: {
          Image: `registry.example.com/shop/api:1.4@${DIGEST}`,
          Env: ["MODE=prod", `DB_PASSWORD=${secret}`],
        },
      },
    });
    // The server masks the new value again for a caller who may not reveal it.
    renderStep({ edited: typed, review: reviewBody({ spec: editSpec() }) });
    const row = screen.getByText("Environment variable DB_PASSWORD").closest("tr");
    expect(row?.textContent).toContain("••••");
    expect(row?.textContent).toContain("Hidden value replaced");
    expect(document.body.textContent).not.toContain(secret);
  });

  it("lists a hidden value that was left alone nowhere", () => {
    renderStep();
    expect(screen.queryByText("Environment variable DB_PASSWORD")).toBeNull();
  });

  it("says there is nothing to apply, and cannot be applied, when the spec did not change", () => {
    renderStep({
      edited: editSpec(),
      review: reviewBody({ spec: editSpec(), changed: false }),
    });
    expect(screen.getByText("No changes")).toBeTruthy();
    expect(apply().getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("There's nothing to apply.").length).toBeGreaterThanOrEqual(1);
    fireEvent.click(apply());
    expect(onApply).not.toHaveBeenCalled();
  });

  it("turns away a caller below Admin with the server's refusal, naming each setting", () => {
    renderStep({
      review: reviewBody({
        spec: withReplicas(editSpec(), 5),
        allowed: false,
        refusal: "x: y",
        findings: [
          {
            path: "TaskTemplate.ContainerSpec.Mounts[0].Source",
            value: "/var/run/docker.sock",
            rule: "bind_path",
            why: "a bind of this host path gives the task access outside its own data",
          },
          {
            path: "TaskTemplate.ContainerSpec.CapabilityAdd[0]",
            value: "SYS_ADMIN",
            rule: "cap_add",
            why: "an added Linux capability widens what the task may do on the host",
          },
          {
            path: "TaskTemplate.Networks[0].Target",
            value: "host",
            rule: "host_network",
            why: "the host network shares the node's network stack with the task",
          },
        ],
      }),
    });
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain(
      "This service uses settings that need Admin: a bind mount of /var/run/docker.sock, the SYS_ADMIN capability, host networking. Ask an administrator to apply it.",
    );
    expect(apply().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(apply());
    expect(onApply).not.toHaveBeenCalled();
  });

  it("lets an Admin through, and warns that the manager's agents refuse host access when they do", () => {
    const finding = {
      path: "TaskTemplate.ContainerSpec.Mounts[0].Source",
      value: "/var/run/docker.sock",
      rule: "bind_path" as const,
      why: "a bind of this host path gives the task access outside its own data",
    };
    renderStep({
      review: reviewBody({
        spec: withReplicas(editSpec(), 5),
        allowed: true,
        findings: [finding],
        hostPolicy: { managerAllowsPrivileged: false, readOnly: false },
      }),
    });
    expect(
      screen.getByText(
        "This swarm's manager agents don't let Bayplan change services with host access. Set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true on them to allow it.",
      ),
    ).toBeTruthy();
    expect(apply().getAttribute("aria-disabled")).toBeNull();
  });

  it("does not warn about host access when the spec has none", () => {
    renderStep({
      review: reviewBody({
        spec: withReplicas(editSpec(), 5),
        hostPolicy: { managerAllowsPrivileged: false, readOnly: false },
      }),
    });
    expect(screen.queryByText(/manager agents don't let Bayplan/)).toBeNull();
  });

  it("cannot be applied on a read-only host, and says why", () => {
    renderStep({
      review: reviewBody({
        spec: withReplicas(editSpec(), 5),
        hostPolicy: { managerAllowsPrivileged: true, readOnly: true },
      }),
    });
    expect(apply().getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("This host is read-only (set on the host)").length).toBeGreaterThan(
      0,
    );
  });

  it("says a digest could not be looked up, and cannot be applied", () => {
    renderStep({
      review: reviewBody({
        spec: withReplicas(editSpec(), 5),
        imageError: {
          ref: "registry.example.com/shop/api:1.5",
          host: "registry.example.com",
          reason: "not_found",
        },
      }),
    });
    // Once in the alert, and once as the reason Apply is unavailable.
    expect(
      screen.getAllByText(
        "Couldn't look up registry.example.com/shop/api:1.5 on registry.example.com to pin it to a digest. Check the name, the tag and the credential, then try again.",
      ).length,
    ).toBeGreaterThanOrEqual(1);
    expect(apply().getAttribute("aria-disabled")).toBe("true");
  });

  it("names what the spec uses that does not exist, and cannot be applied", () => {
    renderStep({
      review: reviewBody({
        spec: withReplicas(editSpec(), 5),
        missing: { networks: ["backend", "edge"], secrets: [], configs: [] },
      }),
    });
    expect(
      screen.getByText(
        "The spec names networks that don't exist: backend, edge. Create them first, then review again.",
      ),
    ).toBeTruthy();
    expect(apply().getAttribute("aria-disabled")).toBe("true");
  });

  it("passes on the manager's notes as text", () => {
    renderStep({
      review: reviewBody({
        spec: withReplicas(editSpec(), 5),
        warnings: ["image <b>x</b> may be old"],
      }),
    });
    expect(screen.getByText("image <b>x</b> may be old")).toBeTruthy();
    expect(document.querySelector("b")).toBeNull();
  });

  it("goes back to editing and applies", () => {
    renderStep();
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    expect(onBack).toHaveBeenCalledTimes(1);
    fireEvent.click(apply());
    expect(onApply).toHaveBeenCalledTimes(1);
  });

  it("reads Applying… and does not apply twice while the request is out", () => {
    renderStep({ applying: true });
    expect(apply().textContent).toContain("Applying…");
    expect(apply().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(apply());
    expect(onApply).not.toHaveBeenCalled();
  });

  it("wraps long values in mono so nothing runs off the page", () => {
    const long = "x".repeat(300);
    const next = withImage(withReplicas(editSpec(), 5), `registry.example.com/${long}:1`);
    renderStep({ edited: next, review: reviewBody({ spec: next }) });
    const cell = screen.getAllByText(`registry.example.com/${long}:1`)[0];
    expect(cell?.className).toMatch(/break-all/);
    expect(cell?.className).toMatch(/font-mono/);
  });
});
