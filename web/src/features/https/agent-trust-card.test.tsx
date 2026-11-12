// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AgentTrustCard } from "@/features/https/agent-trust-card";
import { HttpsPage } from "@/features/https/https-page";
import "@/i18n";
import { httpsCertFilesStatus, httpsLetsEncryptStatus, httpsStatus } from "@/test-https";
import { TestRouter } from "@/test-router";

let answer: () => Response | Promise<Response>;

beforeEach(() => {
  answer = () => Response.json(httpsStatus());
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/https/status") return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
});

function renderCard() {
  render(
    <QueryClientProvider client={queryClient}>
      <AgentTrustCard />
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Agent trust" }));

function unverified(): HTTPSStatus {
  return {
    ...httpsCertFilesStatus(),
    agentTrust: "unverified",
  };
}

describe("Agent trust card", () => {
  it("says an agent connects only if it sees the pinned certificate when the self-signed one is served", async () => {
    answer = () => Response.json(httpsStatus());
    renderCard();
    expect(
      await card().findByText(
        "Install commands for new agents carry the pin of this certificate. An agent connects only if the certificate it sees matches.",
      ),
    ).toBeTruthy();
  });

  it("says agents trust the public certificate and keep the self-signed pin, so no reinstall is needed", async () => {
    answer = () => Response.json(httpsLetsEncryptStatus());
    renderCard();
    expect(
      await card().findByText(
        "Agents trust this certificate through public certificate authorities, so install commands carry no pin. Every agent also keeps the self-signed pin it receives when it connects. If TierPlan goes back to its self-signed certificate, agents keep connecting and nothing needs reinstalling.",
      ),
    ).toBeTruthy();
  });

  it("says agents cannot verify an unverified certificate and links the agent docs in a new tab", async () => {
    answer = () => Response.json(unverified());
    renderCard();
    const link = await card().findByRole("link", { name: "agent docs" });
    expect(link.getAttribute("href")).toBe(
      "https://github.com/ianglennon/tierplan/blob/main/docs/agent.md#a-proxy-with-its-own-certificate-tierplan_agent_pin",
    );
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link.closest("p")?.textContent).toBe(
      "Public certificate authorities don't vouch for this certificate, so install commands carry no pin and agents can't verify it that way. Behind a proxy or a private authority, give each agent the pin of the certificate it will see. See the agent docs.",
    );
  });

  it("is one paragraph with no controls and no docs link outside the unverified variant", async () => {
    answer = () => Response.json(httpsLetsEncryptStatus());
    renderCard();
    await card().findByText(/Agents trust this certificate/);
    expect(card().queryAllByRole("button")).toHaveLength(0);
    expect(card().queryAllByRole("link")).toHaveLength(0);
    expect(card().queryAllByRole("textbox")).toHaveLength(0);
  });

  it("shows skeleton rows while the status is pending and no variant", () => {
    answer = () => new Promise<Response>(() => {});
    renderCard();
    expect(screen.getByTestId("agent-trust-loading")).toBeTruthy();
    expect(card().queryByText(/install commands/)).toBeNull();
  });

  it("shows the unreachable alert when the status cannot be read", async () => {
    answer = () => Response.json({}, { status: 500 });
    renderCard();
    const alert = await card().findByRole("alert");
    expect(alert.textContent).toBe("Can't reach TierPlan. Check your connection and try again.");
  });

  it("is the last card of the HTTPS page, after Let's Encrypt", async () => {
    render(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <TestRouter>
            <HttpsPage />
          </TestRouter>
        </TooltipProvider>
      </QueryClientProvider>,
    );
    await card().findByText(/Install commands for new agents/);
    const titles = screen.getAllByRole("region").map((r) => r.querySelector("h3")?.textContent);
    expect(titles.at(-1)).toBe("Agent trust");
    expect(titles).toContain("Let's Encrypt");
    expect(titles.indexOf("Let's Encrypt")).toBeLessThan(titles.indexOf("Agent trust"));
  });
});
