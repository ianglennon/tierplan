// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { commitUrl, ShaLink } from "@/features/stack-git/sha-link";
import "@/i18n";

const SHA = "0123456789abcdef0123456789abcdef01234567";

afterEach(cleanup);

function mount(host: string, path = "acme/stacks", sha = SHA) {
  render(
    <TooltipProvider>
      <ShaLink sha={sha} repoHost={host} repoPath={path} />
    </TooltipProvider>,
  );
}

describe("commitUrl", () => {
  it("builds the commit page of the three known hosts from a fixed pattern", () => {
    expect(commitUrl("github.com", "acme/stacks", SHA)).toBe(
      `https://github.com/acme/stacks/commit/${SHA}`,
    );
    expect(commitUrl("gitlab.com", "acme/group/stacks", SHA)).toBe(
      `https://gitlab.com/acme/group/stacks/-/commit/${SHA}`,
    );
    expect(commitUrl("bitbucket.org", "acme/stacks", SHA)).toBe(
      `https://bitbucket.org/acme/stacks/commits/${SHA}`,
    );
  });

  it("removes a .git suffix from the path", () => {
    expect(commitUrl("github.com", "acme/stacks.git", SHA)).toBe(
      `https://github.com/acme/stacks/commit/${SHA}`,
    );
  });

  it("gives no address for any other host, a host with a port or one that only looks like a known one", () => {
    expect(commitUrl("gitea.example.com", "acme/stacks", SHA)).toBeNull();
    expect(commitUrl("github.com:8443", "acme/stacks", SHA)).toBeNull();
    expect(commitUrl("github.com.evil.example", "acme/stacks", SHA)).toBeNull();
    expect(commitUrl("evil.example/github.com", "acme/stacks", SHA)).toBeNull();
    expect(commitUrl("", "acme/stacks", SHA)).toBeNull();
  });

  it("gives no address for a commit ID that is not 40 hex characters", () => {
    expect(commitUrl("github.com", "acme/stacks", "0123456")).toBeNull();
    expect(commitUrl("github.com", "acme/stacks", `${SHA}0`)).toBeNull();
    expect(commitUrl("github.com", "acme/stacks", `${"g".repeat(40)}`)).toBeNull();
    expect(commitUrl("github.com", "acme/stacks", "../../etc/passwd")).toBeNull();
  });

  it("gives no address for a path that could leave its repository or carry markup", () => {
    expect(commitUrl("github.com", "", SHA)).toBeNull();
    expect(commitUrl("github.com", "acme", SHA)).toBeNull();
    expect(commitUrl("github.com", "../acme/stacks", SHA)).toBeNull();
    expect(commitUrl("github.com", "acme/../stacks", SHA)).toBeNull();
    expect(commitUrl("github.com", "acme/stacks?x=1", SHA)).toBeNull();
    expect(commitUrl("github.com", "acme/stacks#frag", SHA)).toBeNull();
    expect(commitUrl("github.com", "javascript:alert(1)//x", SHA)).toBeNull();
    expect(commitUrl("github.com", "acme/<b>stacks", SHA)).toBeNull();
  });
});

describe("ShaLink", () => {
  it.each([
    ["github.com", `https://github.com/acme/stacks/commit/${SHA}`],
    ["gitlab.com", `https://gitlab.com/acme/stacks/-/commit/${SHA}`],
    ["bitbucket.org", `https://bitbucket.org/acme/stacks/commits/${SHA}`],
  ])("links a commit on %s that opens in a new tab and says so", (host, href) => {
    mount(host);
    const link = screen.getByRole("link", {
      name: `Open commit 0123456 on ${host} (opens in a new tab)`,
    });
    expect(link.getAttribute("href")).toBe(href);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link.querySelector("code")?.textContent).toBe("0123456");
    expect(link.querySelector("svg")).toBeTruthy();
  });

  it("shows plain mono text for a Gitea host", () => {
    mount("gitea.example.com");
    expect(screen.queryByRole("link")).toBeNull();
    const code = screen.getByText("0123456");
    expect(code.tagName).toBe("CODE");
  });

  it("shows plain text for a commit ID that is not 40 hex characters", () => {
    mount("github.com", "acme/stacks", "abc1234");
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("abc1234").tagName).toBe("CODE");
  });

  it("puts the full commit ID in the tooltip when focused", async () => {
    mount("gitea.example.com");
    screen.getByText("0123456").focus();
    expect((await screen.findAllByText(SHA)).length).toBeGreaterThan(0);
  });
});
