// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { ACMENameTest, ACMETokenTest } from "@/api/https";
import { TokenTestResult } from "@/features/https/token-test-result";
import "@/i18n";
import { tokenTestName, tokenTestResult } from "@/test-https";

afterEach(cleanup);

type StepState = "done" | "failed" | "skipped";

// The four rows with the states named; the rest are done.
function steps(states: Partial<Record<"zone" | "created" | "read" | "deleted", StepState>> = {}) {
  return (["zone", "created", "read", "deleted"] as const).map((id) => ({
    id,
    state: states[id] ?? ("done" as StepState),
  }));
}

function failing(
  name: string,
  code: string,
  states: Parameters<typeof steps>[0],
  over: Partial<ACMENameTest> = {},
): ACMENameTest {
  return tokenTestName({
    name,
    steps: steps(states),
    error: { code, host: name },
    info: { resolvers: null, seconds: 0, state: "not_checked" },
    ...over,
  });
}

function failedResult(names: ACMENameTest[]): ACMETokenTest {
  return { names, passed: false };
}

const region = () => screen.getByRole("status");
const groups = () => Array.from(region().querySelectorAll("section"));
const rows = (group: HTMLElement) => Array.from(group.querySelectorAll("ol > li")) as HTMLElement[];
const alerts = () => Array.from(region().querySelectorAll('[data-slot="alert"]'));

describe("Token test result: the headline", () => {
  it("says 'Token test passed' with a check when every name passed", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    const headline = within(region()).getByText("Token test passed");
    expect(headline.parentElement?.querySelector("svg.lucide-check")).toBeTruthy();
    expect(within(region()).queryByText("Token test failed")).toBeNull();
  });

  it("says 'Token test failed' with a cross when a name failed", () => {
    render(
      <TokenTestResult
        result={failedResult([failing("tp.example.org", "dns_token_scope", { created: "failed" })])}
      />,
    );
    const headline = within(region()).getByText("Token test failed");
    expect(headline.parentElement?.querySelector("svg.lucide-circle-x")).toBeTruthy();
  });

  it("is a polite status region", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    expect(region().getAttribute("aria-live")).toBe("polite");
  });
});

describe("Token test result: the rows of a name", () => {
  it("lists zone, created, read back and deleted in that order, with the record and zone", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    const [group] = groups();
    const text = rows(group as HTMLElement).map((li) => li.textContent);
    expect(text[0]).toContain("example.com zone found.");
    expect(text[1]).toContain("TXT created at _acme-challenge.tierplan.example.com");
    expect(text[2]).toContain("Read back.");
    expect(text[3]).toContain("Deleted.");
  });

  it("marks each row Done, Failed or Skipped with an icon and words for a screen reader", () => {
    render(
      <TokenTestResult
        result={failedResult([
          failing("tp.example.org", "dns_token_scope", { read: "failed", deleted: "skipped" }),
        ])}
      />,
    );
    const [zone, created, read, deleted] = rows(groups()[0] as HTMLElement);
    expect(zone?.querySelector("svg.lucide-check")).toBeTruthy();
    expect(zone?.querySelector(".sr-only")?.textContent).toBe("(Done)");
    expect(created?.querySelector("svg.lucide-check")).toBeTruthy();
    expect(read?.querySelector("svg.lucide-circle-x")).toBeTruthy();
    expect(read?.querySelector(".sr-only")?.textContent).toBe("(Failed)");
    expect(deleted?.querySelector("svg.lucide-minus")).toBeTruthy();
    expect(deleted?.querySelector(".sr-only")?.textContent).toBe("(Skipped)");
  });

  it("draws a row the server has no state for as waiting", () => {
    const result = tokenTestResult({
      names: [tokenTestName({ steps: [{ id: "zone", state: "done" }] })],
    });
    render(<TokenTestResult result={result} />);
    const li = rows(groups()[0] as HTMLElement);
    expect(li).toHaveLength(4 + 1);
    expect(li[3]?.querySelector("svg.lucide-circle")).toBeTruthy();
  });
});

describe("Token test result: the info row", () => {
  const infoRow = () => {
    const li = rows(groups()[0] as HTMLElement);
    return li[li.length - 1] as HTMLElement;
  };

  it("says which resolvers saw the record and after how long", () => {
    render(
      <TokenTestResult
        result={tokenTestResult({
          names: [
            tokenTestName({
              info: { resolvers: ["1.1.1.1", "9.9.9.9"], seconds: 7, state: "seen" },
            }),
          ],
        })}
      />,
    );
    expect(infoRow().textContent).toContain("Seen by 1.1.1.1, 9.9.9.9 after 7 s.");
    expect(infoRow().querySelector("svg.lucide-info")).toBeTruthy();
    expect(infoRow().querySelector(".sr-only")?.textContent).toBe("(Note)");
  });

  it("warns when the resolvers had not seen it, with the hint, and the test still passes", () => {
    render(
      <TokenTestResult
        result={tokenTestResult({
          names: [
            tokenTestName({ info: { resolvers: ["192.168.1.1"], seconds: 60, state: "not_seen" } }),
          ],
        })}
      />,
    );
    const row = infoRow();
    expect(row.textContent).toContain("Not seen by 192.168.1.1 within 60 s.");
    const warn = row.querySelector("svg.lucide-triangle-alert");
    expect(warn).toBeTruthy();
    expect(warn?.getAttribute("class")).toContain("text-status-warning-text");
    expect(row.textContent).toContain("The token works, so this didn't fail the test.");
    expect(row.textContent).toContain("Check that this host can reach 192.168.1.1 on port 53");
    expect(within(region()).getByText("Token test passed")).toBeTruthy();
  });

  it("says 'Not checked.' when an earlier row failed", () => {
    render(
      <TokenTestResult
        result={failedResult([failing("tp.example.org", "dns_token_invalid", { zone: "failed" })])}
      />,
    );
    expect(infoRow().textContent).toContain("Not checked.");
    expect(infoRow().querySelector("svg.lucide-minus")).toBeTruthy();
  });
});

describe("Token test result: delegation", () => {
  it("shows 'Delegated to {target}.' before the zone row when the name is a CNAME", () => {
    render(
      <TokenTestResult
        result={tokenTestResult({
          names: [tokenTestName({ delegatedTo: "tp.validation.example.net" })],
        })}
      />,
    );
    const text = rows(groups()[0] as HTMLElement).map((li) => li.textContent);
    expect(text[0]).toContain("Delegated to tp.validation.example.net.");
    expect(text[1]).toContain("zone found.");
  });

  it("says the name's own zone is not on Cloudflare when it is delegated into one that is", () => {
    render(
      <TokenTestResult
        result={tokenTestResult({
          names: [
            tokenTestName({
              delegatedTo: "tp.validation.example.net",
              ownZone: "example.org",
              zone: "example.net",
            }),
          ],
        })}
      />,
    );
    const first = rows(groups()[0] as HTMLElement)[0] as HTMLElement;
    expect(first.textContent).toContain(
      "example.org is not on Cloudflare. Delegated to tp.validation.example.net.",
    );
  });

  it("shows no delegation row for a name that is not a CNAME", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    expect(region().textContent).not.toContain("Delegated to");
  });
});

describe("Token test result: a failed delete", () => {
  it("says the test record couldn't be deleted and that TierPlan keeps trying", () => {
    render(
      <TokenTestResult
        result={failedResult([failing("tp.example.org", "dns_token_scope", { deleted: "failed" })])}
      />,
    );
    const deleted = rows(groups()[0] as HTMLElement)[3] as HTMLElement;
    expect(deleted.textContent).toContain("Couldn't delete the test record.");
    expect(deleted.textContent).toContain(
      "TierPlan keeps trying to delete it. See the left-over records below.",
    );
    expect(deleted.querySelector("svg.lucide-circle-x")).toBeTruthy();
    expect(within(region()).getByText("Token test failed")).toBeTruthy();
  });

  it("shows no extra line when the delete worked", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    expect(region().textContent).not.toContain("keeps trying");
  });
});

describe("Token test result: the classified error", () => {
  it("draws the headline and fix of the failing name's class under that name", () => {
    render(
      <TokenTestResult
        result={failedResult([failing("tp.example.org", "dns_token_scope", { created: "failed" })])}
      />,
    );
    const alert = within(groups()[0] as HTMLElement).getByText(
      "The Cloudflare token can't manage DNS records for tp.example.org.",
    );
    expect(alert.closest('[data-slot="alert"]')).toBeTruthy();
    expect(region().textContent).toContain("The token needs both Zone:DNS:Edit");
  });

  it.each([
    ["dns_token_invalid", "Cloudflare didn't accept the API token."],
    ["dns_token_scope", "The Cloudflare token can't manage DNS records for tp.example.org."],
    ["dns_zone_not_found", "Cloudflare has no zone for tp.example.org."],
    [
      "dns_delegation_target",
      "_acme-challenge.tp.example.org is delegated to t.example.net, which TierPlan can't edit.",
    ],
    ["dns_not_visible", "The TXT record for tp.example.org didn't appear on 1.1.1.1."],
    ["dns_rate_limited", "Cloudflare is limiting requests."],
    ["dns_unavailable", "Cloudflare couldn't be reached."],
  ])(
    "renders %s under the failing name and the headline says 'Token test failed'",
    (code, headline) => {
      render(
        <TokenTestResult
          result={failedResult([
            failing(
              "tp.example.org",
              code,
              { zone: "failed" },
              {
                error: {
                  code,
                  host: "tp.example.org",
                  target: "t.example.net",
                  resolvers: ["1.1.1.1"],
                },
              },
            ),
          ])}
        />,
      );
      expect(within(region()).getByText("Token test failed")).toBeTruthy();
      expect(alerts()).toHaveLength(1);
      expect(alerts()[0]?.querySelector('[data-slot="alert-title"]')?.textContent).toBe(headline);
    },
  );

  it("uses the internal copy for a class this build does not know", () => {
    render(
      <TokenTestResult
        result={failedResult([
          failing("tp.example.org", "a_class_from_the_future", { zone: "failed" }),
        ])}
      />,
    );
    expect(alerts()[0]?.querySelector('[data-slot="alert-title"]')?.textContent).toBe(
      "Something went wrong inside TierPlan.",
    );
  });

  it("draws no alert for a failing name the server gave no error for", () => {
    const name = failing(
      "tp.example.org",
      "dns_token_scope",
      { zone: "failed" },
      { error: undefined },
    );
    render(<TokenTestResult result={failedResult([name])} />);
    expect(alerts()).toHaveLength(0);
  });

  it("draws a rate limit's retry time in the fix", () => {
    const retryAt = new Date(Date.now() + 600_000).toISOString();
    render(
      <TokenTestResult
        result={failedResult([
          failing(
            "tp.example.org",
            "dns_rate_limited",
            { zone: "failed" },
            { error: { code: "dns_rate_limited", host: "tp.example.org", retryAt } },
          ),
        ])}
      />,
    );
    expect(alerts()[0]?.querySelector('[data-slot="alert-description"]')?.textContent).toMatch(
      /^TierPlan waits until \d{1,2}[:.]\d{2}/,
    );
  });

  it("draws the alert once, under the last group, when two names fail with the same class", () => {
    render(
      <TokenTestResult
        result={failedResult([
          failing("a.example.org", "dns_token_invalid", { zone: "failed" }),
          failing("b.example.org", "dns_token_invalid", { zone: "failed" }),
        ])}
      />,
    );
    expect(alerts()).toHaveLength(1);
    const [first, last] = groups();
    expect(first?.querySelector('[data-slot="alert"]')).toBeNull();
    expect(last?.querySelector('[data-slot="alert"]')).toBeTruthy();
  });

  it("draws one alert each when two names fail with different classes", () => {
    render(
      <TokenTestResult
        result={failedResult([
          failing("a.example.org", "dns_token_invalid", { zone: "failed" }),
          failing("b.example.org", "dns_zone_not_found", { zone: "failed" }),
        ])}
      />,
    );
    expect(alerts()).toHaveLength(2);
    const [first, last] = groups();
    expect(first?.querySelector('[data-slot="alert"]')).toBeTruthy();
    expect(last?.querySelector('[data-slot="alert"]')).toBeTruthy();
  });

  it("is a note, not an alert, so the status region announces the result only once", () => {
    render(
      <TokenTestResult
        result={failedResult([failing("tp.example.org", "dns_token_scope", { zone: "failed" })])}
      />,
    );
    expect(alerts()[0]?.getAttribute("role")).toBe("note");
  });
});

describe("Token test result: partial results", () => {
  it("marks the rows after an early failure skipped and the info row not checked", () => {
    render(
      <TokenTestResult
        result={failedResult([
          failing("tp.example.org", "dns_zone_not_found", {
            zone: "failed",
            created: "skipped",
            read: "skipped",
            deleted: "skipped",
          }),
        ])}
      />,
    );
    const li = rows(groups()[0] as HTMLElement);
    expect(li[0]?.querySelector("svg.lucide-circle-x")).toBeTruthy();
    for (const row of li.slice(1, 4)) {
      expect(row.querySelector("svg.lucide-minus")).toBeTruthy();
      expect(row.querySelector(".sr-only")?.textContent).toBe("(Skipped)");
    }
    expect(li[4]?.textContent).toContain("Not checked.");
  });
});

describe("Token test result: how many names", () => {
  it("draws one group for one name", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    expect(groups()).toHaveLength(1);
  });

  it("draws two groups with the same layout for two names", () => {
    render(
      <TokenTestResult
        result={tokenTestResult({
          names: [
            tokenTestName({ name: "a.example.org" }),
            tokenTestName({ name: "b.example.org" }),
          ],
        })}
      />,
    );
    expect(groups()).toHaveLength(2);
    const [a, b] = groups() as HTMLElement[];
    expect(rows(a as HTMLElement)).toHaveLength(rows(b as HTMLElement).length);
    expect(a?.querySelector("h3")?.textContent).toBe("a.example.org");
    expect(b?.querySelector("h3")?.textContent).toBe("b.example.org");
  });

  it("draws no group for a result with no names", () => {
    render(<TokenTestResult result={{ names: null, passed: true }} />);
    expect(groups()).toHaveLength(0);
  });
});

describe("Token test result: not saved and stale", () => {
  it("says nothing was saved under the headline when the failure came from Save", () => {
    render(
      <TokenTestResult
        notSaved
        result={failedResult([failing("tp.example.org", "dns_token_scope", { zone: "failed" })])}
      />,
    );
    expect(
      within(region()).getByText("Nothing was saved. The settings in use are unchanged."),
    ).toBeTruthy();
  });

  it("leaves the not-saved line out by default", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    expect(region().textContent).not.toContain("Nothing was saved");
  });

  it("greys a stale result, stops announcing it and says to test again", () => {
    render(<TokenTestResult stale result={tokenTestResult()} />);
    expect(region().className).toContain("opacity-60");
    expect(region().getAttribute("aria-live")).toBe("off");
    expect(
      within(region()).getByText("This result is for earlier settings. Test again."),
    ).toBeTruthy();
  });

  it("shows no stale note on a fresh result", () => {
    render(<TokenTestResult result={tokenTestResult()} />);
    expect(region().className).not.toContain("opacity-60");
    expect(region().textContent).not.toContain("earlier settings");
  });
});

describe("Token test result: long text and overflow", () => {
  const long = `${"very-long-label-".repeat(8)}example.org`;

  it("breaks names, zones, targets, record names and resolvers anywhere and lets them shrink", () => {
    render(
      <TokenTestResult
        result={tokenTestResult({
          names: [
            tokenTestName({
              name: long,
              record: `_acme-challenge.${long}`,
              zone: long,
              delegatedTo: `target.${long}`,
              info: { resolvers: [`resolver.${long}`], seconds: 60, state: "not_seen" },
            }),
          ],
        })}
      />,
    );
    const text = region();
    for (const value of [long, `_acme-challenge.${long}`, `target.${long}`, `resolver.${long}`]) {
      const node = Array.from(text.querySelectorAll("code")).find((c) =>
        c.textContent?.includes(value),
      );
      expect(node, value).toBeTruthy();
      expect(node?.className).toContain("break-all");
    }
    expect(text.className).toContain("min-w-0");
    expect(groups()[0]?.className).toContain("min-w-0");
    for (const li of rows(groups()[0] as HTMLElement)) {
      expect(li.className).toContain("min-w-0");
    }
    expect(groups()[0]?.querySelector("h3")?.className).toContain("break-all");
  });

  it("wraps a long value in the error alert", () => {
    render(
      <TokenTestResult
        result={failedResult([
          failing(
            long,
            "dns_delegation_target",
            { zone: "failed" },
            {
              error: { code: "dns_delegation_target", host: long, target: `target.${long}` },
            },
          ),
        ])}
      />,
    );
    const alert = alerts()[0] as HTMLElement;
    expect(alert.querySelector('[data-slot="alert-title"]')?.className).toContain("break-words");
    expect(alert.querySelector('[data-slot="alert-description"]')?.className).toContain(
      "break-words",
    );
    expect(alert.className).toContain("min-w-0");
  });
});

describe("Token test result: server text is text", () => {
  it("draws a name that looks like markup as literal text", () => {
    const evil = '<img src=x onerror="window.__pwned = true">';
    render(
      <TokenTestResult
        result={failedResult([
          failing(evil, "dns_zone_not_found", { zone: "failed" }, { zone: evil, record: evil }),
        ])}
      />,
    );
    expect(region().querySelector("img")).toBeNull();
    expect(region().textContent).toContain(evil);
  });
});
