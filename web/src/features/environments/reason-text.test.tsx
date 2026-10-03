// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, renderHook, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { type ReasonParams, ReasonText, useReasonText } from "@/features/environments/reason-text";
import "@/i18n";

afterEach(cleanup);

// Every D-40 code, then the two enrolment-only refusals, then the one extra
// issue code the health rollup can report.
const D40 = [
  "agent_offline",
  "tls_untrusted",
  "auth_rejected",
  "cert_expired",
  "revoked",
  "blocked",
  "version_incompatible",
  "clock_skew",
  "timeout",
  "not_a_manager",
  "no_quorum",
  "cluster_changed",
  "engine_unreachable",
  "unsupported_engine",
  "agent_policy_denied",
];
const ENROLMENT = ["duplicate_environment", "type_mismatch"];
const EXTRA = ["cert_expiring"];
// The codes the local Docker socket reports (UI-SPEC Connection reason codes).
const LOCAL = ["unreachable", "api_too_old", "podman", "rootless", "docker_desktop"];

const ALL_PARAMS: ReasonParams = {
  node: "edge-1",
  offset: "+6 min",
  version: "0.1.0",
  engine: "Docker Engine",
  endpoint: "GET /v1/containers",
  host: "edge-1",
  environment: "prod",
  hostType: "standalone host",
  envType: "swarm",
  command: " docker service update --image x bayplan-agent",
};

function localClock(hours: number, minutes: number) {
  const date = new Date(2026, 9, 3, hours, minutes);
  return {
    iso: date.toISOString(),
    shown: new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(date),
  };
}

describe("ReasonText", () => {
  it("states clock_skew with the node and offset and the NTP fix hint", () => {
    render(<ReasonText code="clock_skew" params={{ node: "edge-1", offset: "6 min" }} />);
    expect(screen.getByText("The clock on edge-1 is 6 min out of step.")).toBeTruthy();
    expect(
      screen.getByText(
        "Sync the host's clock (NTP). Bayplan accepts up to 5 minutes of difference.",
      ),
    ).toBeTruthy();
  });

  it("adds since HH:MM in the browser's clock", () => {
    const { iso, shown } = localClock(14, 5);
    render(<ReasonText code="agent_offline" params={{ node: "edge-1" }} since={iso} />);
    expect(screen.getByText(`since ${shown}`)).toBeTruthy();
  });

  it("leaves since out when the time is missing or unreadable", () => {
    const { container } = render(<ReasonText code="agent_offline" since="not a time" />);
    expect(container.textContent).not.toContain("since");
  });

  it("puts the update command of version_incompatible in CopyableCode and says which way the version is off", () => {
    render(
      <ReasonText
        code="version_incompatible"
        params={{ version: "0.0.9", command: ALL_PARAMS.command, direction: "older" }}
      />,
    );
    expect(screen.getByText("Agent 0.0.9 can't talk to this Bayplan.")).toBeTruthy();
    expect(screen.getByText("The agent is older than Bayplan supports.")).toBeTruthy();
    expect(screen.getByText("Update the agent by hand:")).toBeTruthy();
    const block = screen.getByRole("region", { name: "the agent update command" });
    expect(within(block).getByText(/docker service update --image x bayplan-agent/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy the agent update command" })).toBeTruthy();
  });

  it("says the agent is newer when it is", () => {
    render(<ReasonText code="version_incompatible" params={{ direction: "newer" }} />);
    expect(screen.getByText("The agent is newer than this Bayplan.")).toBeTruthy();
    // No command to show, so the hint does not promise one.
    expect(screen.getByText("Update the agent by hand.")).toBeTruthy();
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("shows a node name with markup in it as text", () => {
    const { container } = render(
      <ReasonText code="agent_offline" params={{ node: "<b>evil</b>" }} />,
    );
    expect(container.textContent).toContain("The agent on <b>evil</b> isn't connected.");
    expect(container.querySelector("b")).toBeNull();
  });

  it("shows an agent supplied value as text in every parameter", () => {
    const hostile = "<img src=x onerror=alert(1)>";
    const { container } = render(
      <ReasonText
        code="unsupported_engine"
        params={{ engine: hostile, version: hostile, node: hostile }}
      />,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(container.textContent).toContain(hostile);
  });

  it("shows the commands of agent_offline in CopyableCode, standalone and swarm", () => {
    render(<ReasonText code="agent_offline" params={{ node: "edge-1" }} />);
    expect(
      within(screen.getByRole("region", { name: "the standalone check command" })).getByText(
        "docker ps --filter name=bayplan-agent",
      ),
    ).toBeTruthy();
    expect(
      within(screen.getByRole("region", { name: "the swarm check command" })).getByText(
        "docker service ps bayplan-agent",
      ),
    ).toBeTruthy();
  });

  it("writes a path in a hint as code", () => {
    const { container } = render(<ReasonText code="engine_unreachable" params={{ node: "n1" }} />);
    expect(container.querySelector("code")?.textContent).toBe("/var/run/docker.sock");
  });

  it("falls back to wording without the missing parameter instead of showing a placeholder", () => {
    const { rerender, container } = render(
      <ReasonText code="clock_skew" params={{ node: "edge-1" }} />,
    );
    expect(container.textContent).toContain("The clock on a node is out of step.");
    rerender(<ReasonText code="duplicate_environment" params={{ host: "edge-1" }} />);
    expect(container.textContent).toContain("edge-1 already belongs to another environment.");
    expect(container.textContent).toContain("Open the other environment");
    rerender(
      <ReasonText code="duplicate_environment" params={{ host: "edge-1", environment: "prod" }} />,
    );
    expect(container.textContent).toContain("edge-1 already belongs to prod.");
    expect(container.textContent).toContain("Open prod, or remove it first");
    expect(container.textContent).not.toContain("{{");
  });

  it("says what type_mismatch is, with or without the names", () => {
    const { rerender, container } = render(
      <ReasonText
        code="type_mismatch"
        params={{ host: "h1", hostType: "standalone host", environment: "prod", envType: "swarm" }}
      />,
    );
    expect(container.textContent).toContain("h1 is a standalone host, but prod is a swarm.");
    expect(container.textContent).toContain("Add it as a separate environment.");
    rerender(<ReasonText code="type_mismatch" />);
    expect(container.textContent).toContain(
      "This host is a different type from the environment it tried to join.",
    );
  });

  it("gives a code it does not know a generic line and does not crash", () => {
    const { container } = render(<ReasonText code="teapot" />);
    expect(container.textContent).toContain("Connection problem (teapot)");
    expect(container.textContent).toContain("Look at the agent's log on the host");
    expect(container.textContent).not.toContain("reason.");
  });

  it("shows an unknown code as text even when it holds markup", () => {
    const { container } = render(<ReasonText code="<i>x</i>" />);
    expect(container.querySelector("i")).toBeNull();
    expect(container.textContent).toContain("Connection problem (<i>x</i>)");
  });

  describe("every code", () => {
    for (const code of [...D40, ...ENROLMENT, ...EXTRA, ...LOCAL]) {
      it(`${code} has text and a fix hint, with every parameter and with none`, () => {
        const { result } = renderHook(() => useReasonText());
        for (const params of [ALL_PARAMS, undefined]) {
          const described = result.current(code, params);
          expect(described.known).toBe(true);
          expect(described.text.length).toBeGreaterThan(8);
          expect(described.hint.length).toBeGreaterThan(8);
          for (const part of [described.text, described.hint]) {
            expect(part).not.toContain("{{");
            expect(part).not.toContain("reason.");
            expect(part).not.toContain("undefined");
          }
        }
      });
    }

    it("covers 15 D-40 codes and the two enrolment refusals", () => {
      expect(D40).toHaveLength(15);
      expect(ENROLMENT).toEqual(["duplicate_environment", "type_mismatch"]);
    });
  });

  describe("the type of the environment", () => {
    const STANDALONE = "docker ps --filter name=bayplan-agent";
    const SWARM = "docker service ps bayplan-agent";

    it("shows only the swarm check for a swarm and only the standalone check for a standalone host", () => {
      render(<ReasonText code="agent_offline" params={{ envType: "swarm" }} />);
      expect(screen.getByText(SWARM)).toBeTruthy();
      expect(screen.queryByText(STANDALONE)).toBeNull();
      cleanup();
      render(<ReasonText code="agent_offline" params={{ envType: "standalone" }} />);
      expect(screen.getByText(STANDALONE)).toBeTruthy();
      expect(screen.queryByText(SWARM)).toBeNull();
    });

    it("shows both checks while the type is not known", () => {
      render(<ReasonText code="agent_offline" />);
      expect(screen.getByText(STANDALONE)).toBeTruthy();
      expect(screen.getByText(SWARM)).toBeTruthy();
    });
  });

  it("restyles an inline path for a tooltip's inverted colours, and keeps the chip elsewhere", () => {
    const { container, unmount } = render(
      <ReasonText code="engine_unreachable" variant="tooltip" />,
    );
    const inTooltip = container.querySelector("code");
    expect(inTooltip?.textContent).toBe("/var/run/docker.sock");
    expect(inTooltip?.className).toContain("text-background");
    unmount();
    const full = render(<ReasonText code="engine_unreachable" />);
    expect(full.container.querySelector("code")?.className).toContain("bg-muted");
    expect(full.container.querySelector("code")?.className).not.toContain("text-background");
  });

  describe("variants", () => {
    it("compact is the text and the hint on two lines, without commands", () => {
      const { container } = render(
        <ReasonText code="agent_offline" params={{ node: "edge-1" }} variant="compact" />,
      );
      const lines = Array.from(container.querySelectorAll("p")).map((p) => p.textContent);
      expect(lines).toEqual([
        "The agent on edge-1 isn't connected.",
        "Check that it's running. It reconnects by itself when it can reach Bayplan.",
      ]);
      expect(screen.queryByRole("region")).toBeNull();
      expect(screen.queryByRole("button")).toBeNull();
    });

    it("tooltip is the text and a one-line hint, never a command", () => {
      const { container } = render(
        <ReasonText
          code="version_incompatible"
          params={{ command: ALL_PARAMS.command }}
          variant="tooltip"
        />,
      );
      expect(container.textContent).toContain("The agent can't talk to this Bayplan.");
      expect(container.textContent).toContain("Update the agent by hand.");
      expect(container.textContent).not.toContain("docker service update");
      expect(screen.queryByRole("button")).toBeNull();
    });

    it("full puts a lead before the text when given one", () => {
      render(<ReasonText code="auth_rejected" lead="Refused:" />);
      expect(screen.getByText("Refused: Bayplan rejected the agent's credentials.")).toBeTruthy();
    });
  });

  describe("useReasonText", () => {
    it("returns the text and the hint for a code", () => {
      const { result } = renderHook(() => useReasonText());
      const described = result.current("tls_untrusted");
      expect(described.text).toBe("The agent doesn't trust Bayplan's certificate.");
      expect(described.hint).toContain("Use a certificate from a public CA");
      expect(described.commands).toEqual([]);
    });
  });
});
