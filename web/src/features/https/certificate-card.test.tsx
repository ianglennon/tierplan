// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSCertificate, HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { deriveMode } from "@/features/https/card-state";
import { CertificateCard } from "@/features/https/certificate-card";
import { HttpsPage } from "@/features/https/https-page";
import "@/i18n";
import { formatAbsolute } from "@/lib/format";
import {
  acmeLastError,
  certFilesInfo,
  httpsCertFilesKeptStatus,
  httpsCertFilesStatus,
  httpsEnvOffEmptyStatus,
  httpsEnvOffStatus,
  httpsEnvOnIssuedStatus,
  httpsEnvOnStatus,
  httpsFallbackStatus,
  httpsLetsEncryptStatus,
  httpsPendingStatus,
  httpsRestoredStatus,
  httpsStatus,
  httpsStoppedStatus,
  httpsTestCAStatus,
} from "@/test-https";
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
      <TooltipProvider>
        <CertificateCard />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

function withCertificate(over: Partial<HTTPSCertificate>): HTTPSStatus {
  const base = httpsStatus();
  return { ...base, certificate: { ...base.certificate, ...over } };
}

const card = () => within(screen.getByRole("region", { name: "Certificate in use" }));

// The dd that follows the dt with this label.
function rowValue(label: string): HTMLElement {
  const dt = card().getByText(label, { selector: "dt" });
  const dd = dt.nextElementSibling;
  if (!(dd instanceof HTMLElement)) throw new Error(`no value for ${label}`);
  return dd;
}

describe("Certificate in use card", () => {
  it("shows three skeleton rows while the status is pending, and no certificate", () => {
    answer = () => new Promise<Response>(() => {});
    renderCard();
    const loading = screen.getByTestId("certificate-loading");
    expect(loading.children).toHaveLength(3);
    expect(card().queryByText("Mode")).toBeNull();
    expect(card().queryByText("Self-signed")).toBeNull();
  });

  it("shows the unreachable alert inside the card when the status cannot be read", async () => {
    answer = () => Response.json({}, { status: 500 });
    renderCard();
    const alert = await card().findByRole("alert");
    expect(alert.textContent).toBe("Can't reach TierPlan. Check your connection and try again.");
    expect(screen.queryByTestId("certificate-loading")).toBeNull();
    expect(card().queryByText("Mode")).toBeNull();
  });

  it("renders a self-signed certificate with its badge, rows and note", async () => {
    // Half a day over, so the whole days left stay 364 whatever the clock reads while the test runs.
    const notAfter = new Date(Date.now() + 364.5 * 86_400_000).toISOString();
    answer = () => Response.json(withCertificate({ notAfter }));
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Mode")).getByText("Self-signed")).toBeTruthy();
    expect(rowValue("Issuer").textContent).toBe("TierPlan");
    expect(rowValue("Expires").textContent).toContain("Expires in 364 days");
    expect(
      card().getByText(
        "TierPlan made this certificate for this install. Browsers warn about it until you use Let's Encrypt or your own certificate files.",
      ),
    ).toBeTruthy();
  });

  it("renders a certificate-files certificate with its own badge and reason", async () => {
    answer = () => Response.json(withCertificate({ source: "cert_files", issuer: "Example CA" }));
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Mode")).getByText("Certificate files")).toBeTruthy();
    expect(rowValue("Issuer").textContent).toBe("Example CA");
    expect(card().queryByText(/made this certificate/)).toBeNull();
  });

  it("shows Unknown when the issuer is empty", async () => {
    answer = () => Response.json(withCertificate({ issuer: "" }));
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Issuer").textContent).toBe("Unknown");
  });

  it("lists the IP addresses of a certificate that has only IP names", async () => {
    answer = () => Response.json(withCertificate({ names: ["192.168.1.20", "::1"] }));
    renderCard();
    await card().findByText("Mode");
    const items = within(rowValue("Names")).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual(["192.168.1.20", "::1"]);
  });

  it("lists every one of six names with no 'and N more'", async () => {
    const names = ["a.example.com", "b.example.com", "localhost", "127.0.0.1", "::1", "10.0.0.5"];
    answer = () => Response.json(withCertificate({ names }));
    renderCard();
    await card().findByText("Mode");
    const items = within(rowValue("Names")).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual(names);
    expect(card().queryByText(/more/)).toBeNull();
  });

  it("renders one name as the same list", async () => {
    answer = () => Response.json(withCertificate({ names: ["tierplan.example.com"] }));
    renderCard();
    await card().findByText("Mode");
    const list = within(rowValue("Names")).getByRole("list");
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
    expect(list.textContent).toBe("tierplan.example.com");
  });

  it("wraps a 120 character host and a long issuer instead of widening the column", async () => {
    const host = `${"a".repeat(55)}.${"b".repeat(55)}.example.com`;
    const issuer = "The Very Long Certificate Authority Name ".repeat(5).trim();
    answer = () => Response.json(withCertificate({ names: [host], issuer }));
    renderCard();
    await card().findByText("Mode");
    expect(host.length).toBeGreaterThanOrEqual(120);
    expect(rowValue("Names").className).toContain("break-all");
    expect(rowValue("Names").className).toContain("min-w-0");
    expect(rowValue("Issuer").className).toContain("break-all");
    expect(rowValue("Issuer").className).toContain("font-mono");
    expect(within(rowValue("Names")).getByText(host).className).toContain("font-mono");
    expect(rowValue("Issuer").textContent).toBe(issuer);
  });

  it("shows a punycode name as the certificate carries it, and markup as text", async () => {
    const punycode = "xn--bcher-kva.example";
    const markup = "<img src=x onerror=alert(1)>";
    answer = () => Response.json(withCertificate({ names: [punycode], issuer: markup }));
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Names")).getByText(punycode)).toBeTruthy();
    expect(card().queryByText("bücher.example")).toBeNull();
    expect(rowValue("Issuer").textContent).toBe(markup);
    expect(rowValue("Issuer").querySelector("img")).toBeNull();
  });

  it("shows no renewal or error rows outside Let's Encrypt", async () => {
    renderCard();
    await card().findByText("Mode");
    expect(card().queryByText("Last renewed")).toBeNull();
    expect(card().queryByText("Next renewal")).toBeNull();
    expect(card().queryByText("Last error")).toBeNull();
    expect(
      card()
        .getAllByRole("term")
        .map((dt) => dt.textContent),
    ).toEqual(["Mode", "Issuer", "Names", "Expires"]);
  });
});

describe("Certificate in use card: Mode row", () => {
  const REASONS: ReadonlyArray<readonly [string, () => HTTPSStatus, string, string]> = [
    [
      "default",
      () => httpsStatus(),
      "Self-signed",
      "This is the default. No certificate files and no Let's Encrypt certificate are set up.",
    ],
    [
      "pending",
      () => httpsPendingStatus(),
      "Self-signed",
      "Let's Encrypt is set up, but no certificate has been issued yet.",
    ],
    [
      "stopped",
      () => httpsStoppedStatus(),
      "Self-signed",
      "Let's Encrypt is stopped. Issue a certificate to use it again.",
    ],
    [
      "fallback",
      () => httpsFallbackStatus(),
      "Self-signed",
      "The Let's Encrypt certificate expired. TierPlan serves its own certificate until a renewal works.",
    ],
    [
      "files",
      () => httpsCertFilesKeptStatus({ pausedReason: "" }),
      "Certificate files",
      "Set by TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE. Certificate files take priority over Let's Encrypt.",
    ],
    [
      "filesPaused",
      () => httpsCertFilesStatus(),
      "Certificate files",
      "Set by TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE. Let's Encrypt is paused while they are set.",
    ],
    [
      "issued",
      () => httpsLetsEncryptStatus(),
      "Let's Encrypt",
      "TierPlan got this certificate from Let's Encrypt and renews it.",
    ],
    [
      "pausedRestore",
      () => httpsRestoredStatus(),
      "Let's Encrypt",
      "Paused after a restore. Resume Let's Encrypt below to renew.",
    ],
    [
      "envPending",
      () => httpsEnvOnStatus(),
      "Self-signed",
      "TIERPLAN_ACME is on. TierPlan is getting a certificate by itself.",
    ],
    [
      "envOff",
      () => httpsEnvOffStatus(),
      "Self-signed",
      "Let's Encrypt is turned off by TIERPLAN_ACME.",
    ],
    [
      "envOff",
      () => httpsEnvOffEmptyStatus(),
      "Self-signed",
      "Let's Encrypt is turned off by TIERPLAN_ACME.",
    ],
    [
      "issuedEnv",
      () => httpsEnvOnIssuedStatus(),
      "Let's Encrypt",
      "Turned on by TIERPLAN_ACME. TierPlan renews it by itself.",
    ],
  ];

  it.each(REASONS)("says why the %s mode is in force", async (reason, make, badge, sentence) => {
    answer = () => Response.json(make());
    renderCard();
    await card().findByText("Mode");
    expect(deriveMode(make()).reason).toBe(reason);
    expect(within(rowValue("Mode")).getByText(badge)).toBeTruthy();
    expect(rowValue("Mode").textContent).toContain(sentence);
  });

  it("puts TIERPLAN_ACME in mono in an environment reason", async () => {
    answer = () => Response.json(httpsEnvOnStatus());
    renderCard();
    await card().findByText("Mode");
    const names = Array.from(rowValue("Mode").querySelectorAll("code")).map((c) => c.textContent);
    expect(names).toEqual(["TIERPLAN_ACME"]);
  });

  it("keeps the precedence: files, then a restore pause, then the fallback, then the environment", () => {
    const on = { locks: { acme: { var: "TIERPLAN_ACME", value: "on" } } };
    expect(deriveMode(httpsCertFilesKeptStatus({ pausedReason: "cert_files", ...on })).reason).toBe(
      "filesPaused",
    );
    expect(deriveMode(httpsRestoredStatus(on)).reason).toBe("pausedRestore");
    expect(deriveMode(httpsFallbackStatus(on)).reason).toBe("fallback");
    expect(deriveMode(httpsEnvOnStatus()).reason).toBe("envPending");
    // Without the variable the same installs read as before.
    expect(deriveMode(httpsPendingStatus()).reason).toBe("pending");
    expect(deriveMode(httpsStoppedStatus()).reason).toBe("stopped");
    expect(deriveMode(httpsLetsEncryptStatus()).reason).toBe("issued");
  });

  it("puts the file variable names in mono", async () => {
    answer = () => Response.json(httpsCertFilesKeptStatus({ pausedReason: "" }));
    renderCard();
    await card().findByText("Mode");
    const names = Array.from(rowValue("Mode").querySelectorAll("code")).map((c) => c.textContent);
    expect(names).toEqual(["TIERPLAN_TLS_CERT_FILE", "TIERPLAN_TLS_KEY_FILE"]);
  });

  it("shows the validation badge for Let's Encrypt", async () => {
    answer = () => Response.json(httpsLetsEncryptStatus({ challengeType: "dns-01" }));
    renderCard();
    await card().findByText("Mode");
    const badge = within(rowValue("Mode")).getByText("DNS-01");
    expect(badge.getAttribute("data-variant")).toBe("outline");
  });

  it.each([
    [
      "self-signed",
      () => httpsStatus({ acme: httpsLetsEncryptStatus({ challengeType: "dns-01" }).acme }),
    ],
    ["certificate files", () => httpsCertFilesKeptStatus({ challengeType: "dns-01" })],
  ] as const)("shows no validation badge for %s", async (_name, make) => {
    answer = () => Response.json(make());
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Mode")).queryByText("HTTP-01")).toBeNull();
    expect(within(rowValue("Mode")).queryByText("DNS-01")).toBeNull();
  });

  it("says HTTP-01 for a Let's Encrypt certificate ordered over HTTP", async () => {
    answer = () => Response.json(httpsLetsEncryptStatus({ challengeType: "http-01" }));
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Mode")).getByText("HTTP-01")).toBeTruthy();
  });

  it("falls back to the default sentence for a reason it does not know, and still draws the badge", async () => {
    const unknown = httpsStatus({
      acme: httpsStatus().acme && {
        ...(httpsStatus().acme as NonNullable<HTTPSStatus["acme"]>),
        pausedReason: "from_the_future" as never,
      },
    });
    answer = () => Response.json(unknown);
    renderCard();
    await card().findByText("Mode");
    expect(deriveMode(unknown).reason).toBe("default");
    expect(within(rowValue("Mode")).getByText("Self-signed")).toBeTruthy();
    expect(rowValue("Mode").textContent).toContain("This is the default.");
  });

  it("keeps the Test CA badge beside the mode", async () => {
    answer = () => Response.json(httpsTestCAStatus());
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Mode")).getByText("Test CA")).toBeTruthy();
  });

  it("wraps the reason sentence instead of widening the column", async () => {
    answer = () => Response.json(httpsFallbackStatus());
    renderCard();
    await card().findByText("Mode");
    const sentence = within(rowValue("Mode")).getByText(/The Let's Encrypt certificate expired/);
    expect(sentence.className).toContain("break-words");
    expect(rowValue("Mode").className).toContain("min-w-0");
  });
});

const filesBlock = () => within(screen.getByRole("region", { name: "Certificate files" }));

function filesValue(label: string): HTMLElement {
  const dt = filesBlock().getByText(label, { selector: "dt" });
  const dd = dt.nextElementSibling;
  if (!(dd instanceof HTMLElement)) throw new Error(`no value for ${label}`);
  return dd;
}

function withFiles(over: Parameters<typeof certFilesInfo>[0] = {}): HTTPSStatus {
  return { ...httpsCertFilesStatus(), certFilesInfo: certFilesInfo(over) };
}

describe("Certificate in use card: certificate files block", () => {
  it("shows the two paths in mono with their lock sentences, and a verified certificate", async () => {
    answer = () => Response.json(withFiles());
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    const cert = filesValue("Certificate file");
    expect(within(cert).getByText("/run/secrets/tls.crt").className).toContain("font-mono");
    expect(cert.textContent).toContain("Set by the TIERPLAN_TLS_CERT_FILE environment variable.");
    expect(within(cert).getByRole("img", { name: "Locked" })).toBeTruthy();
    const key = filesValue("Key file");
    expect(within(key).getByText("/run/secrets/tls.key").className).toContain("font-mono");
    expect(key.textContent).toContain("Set by the TIERPLAN_TLS_KEY_FILE environment variable.");
    const trusted = filesValue("Trusted");
    expect(trusted.textContent).toContain("Verifies for tierplan.example.com.");
    expect(within(trusted).getByText("tierplan.example.com").className).toContain("font-mono");
    expect(trusted.textContent).toContain("HSTS can be sent for it.");
    // No stored-value line: these two variables have no stored counterpart.
    expect(filesBlock().queryByText(/Stored value/)).toBeNull();
  });

  it("explains in the foot that the files are only read and checked again", async () => {
    answer = () => Response.json(withFiles());
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    expect(
      filesBlock().getByText(
        "TierPlan only reads these files and checks them again about once a minute, so a renewed certificate is picked up without a restart. To use Let's Encrypt, remove both variables and restart.",
      ),
    ).toBeTruthy();
  });

  it.each([
    ["self_signed", "It is self-signed.", "Use a certificate from a public certificate authority."],
    [
      "unknown_authority",
      "It isn't signed by an authority this server trusts.",
      "Use a certificate from a public authority. If it is, the file may be missing the intermediate certificates: put them after the certificate in the same file.",
    ],
    ["expired", "It has expired or isn't valid yet.", "Renew the certificate."],
    [
      "name_mismatch",
      "It doesn't cover tierplan.example.com.",
      "Get a certificate that includes tierplan.example.com, or change the public URL in Settings > General.",
    ],
    [
      "no_public_url",
      "No public URL is set, so TierPlan can't check the name.",
      "Set the public URL in Settings > General.",
    ],
  ] as const)(
    "says why a %s certificate doesn't verify, and its fix",
    async (reason, sentence, fix) => {
      answer = () =>
        Response.json(
          withFiles({
            trust: { checked: true, verified: false, reason, host: "tierplan.example.com" },
          }),
        );
      renderCard();
      await card().findByText("Certificate file", { selector: "dt" });
      const trusted = filesValue("Trusted");
      expect(trusted.textContent).toContain("Doesn't verify.");
      expect(trusted.textContent).toContain(sentence);
      expect(trusted.textContent).toContain(fix);
      expect(trusted.textContent).toContain("HSTS is never sent for it.");
      expect(trusted.textContent).not.toContain("HSTS can be sent for it.");
      expect(trusted.textContent).not.toContain("Verifies for");
    },
  );

  it("gives a leaf-only file the missing-intermediates hint", async () => {
    answer = () =>
      Response.json(
        withFiles({ trust: { checked: true, verified: false, reason: "unknown_authority" } }),
      );
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    expect(filesValue("Trusted").textContent).toContain("missing the intermediate certificates");
  });

  it("says 'Not checked yet.' when the status carries no verdict, and still shows the paths", async () => {
    answer = () => Response.json(withFiles({ trust: { checked: false, verified: false } }));
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    const trusted = filesValue("Trusted");
    expect(trusted.textContent).toContain("Not checked yet.");
    expect(trusted.textContent).not.toContain("Doesn't verify");
    expect(trusted.querySelector("svg")?.getAttribute("class")).toContain("text-muted-foreground");
    expect(filesValue("Certificate file").textContent).toContain("/run/secrets/tls.crt");
    expect(filesValue("Key file").textContent).toContain("/run/secrets/tls.key");
  });

  it("falls back to the unknown-authority sentence for a reason it does not know", async () => {
    answer = () =>
      Response.json(
        withFiles({
          trust: { checked: true, verified: false, reason: "from_the_future" as never },
        }),
      );
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    expect(filesValue("Trusted").textContent).toContain(
      "It isn't signed by an authority this server trusts.",
    );
  });

  it("draws no block when there is no certificate-files info", async () => {
    answer = () => Response.json(httpsLetsEncryptStatus());
    renderCard();
    await card().findByText("Mode");
    expect(screen.queryByRole("region", { name: "Certificate files" })).toBeNull();
    expect(card().queryByText("Certificate file", { selector: "dt" })).toBeNull();
  });

  it("has nothing to edit: no input, button or link in the block", async () => {
    answer = () => Response.json(withFiles());
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    expect(filesBlock().queryAllByRole("textbox")).toHaveLength(0);
    expect(filesBlock().queryAllByRole("button")).toHaveLength(0);
    expect(filesBlock().queryAllByRole("link")).toHaveLength(0);
    expect(document.querySelector('input[type="file"]')).toBeNull();
  });

  it("wraps two 120 character paths and a long host with break-all and never widens the card", async () => {
    const certPath = `/${"c".repeat(59)}/${"d".repeat(58)}.crt`;
    const keyPath = `/${"k".repeat(59)}/${"l".repeat(58)}.key`;
    const host = `${"a".repeat(55)}.${"b".repeat(55)}.example.com`;
    expect(certPath.length).toBeGreaterThanOrEqual(120);
    answer = () =>
      Response.json(
        withFiles({
          certPath,
          keyPath,
          trust: { checked: true, verified: false, reason: "name_mismatch", host },
        }),
      );
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    expect(filesValue("Certificate file").className).toContain("break-all");
    expect(filesValue("Certificate file").className).toContain("min-w-0");
    expect(filesValue("Key file").className).toContain("break-all");
    expect(filesValue("Key file").className).toContain("min-w-0");
    expect(screen.getByRole("region", { name: "Certificate files" }).className).toContain(
      "min-w-0",
    );
    const mismatch = within(filesValue("Trusted")).getAllByText(host);
    expect(mismatch.length).toBeGreaterThan(0);
    for (const el of mismatch) {
      expect(el.className).toContain("font-mono");
      expect(el.className).toContain("break-all");
    }
  });

  it("shows markup in a path as text", async () => {
    const certPath = "/x/<img src=x onerror=alert(1)>.crt";
    answer = () => Response.json(withFiles({ certPath }));
    renderCard();
    await card().findByText("Certificate file", { selector: "dt" });
    expect(filesValue("Certificate file").querySelector("img")).toBeNull();
    expect(filesValue("Certificate file").textContent).toContain(certPath);
  });
});

const NEXT_AT = "2027-01-05T09:30:00Z";
const labels = () =>
  card()
    .getAllByRole("term")
    .map((dt) => dt.textContent);

describe("Certificate in use card: Let's Encrypt", () => {
  it("shows a first issue with its rows: Issued, the next renewal and its basis, no error", async () => {
    answer = () =>
      Response.json(httpsLetsEncryptStatus({ renewal: { basis: "two_thirds", nextAt: NEXT_AT } }));
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Mode")).getByText("Let's Encrypt")).toBeTruthy();
    expect(within(rowValue("Mode")).queryByText("Test CA")).toBeNull();
    expect(labels()).toEqual([
      "Mode",
      "Issuer",
      "Names",
      "Expires",
      "Last renewed",
      "Next renewal",
      "Last error",
    ]);
    expect(rowValue("Last renewed").textContent).toMatch(/^Issued /);
    expect(rowValue("Next renewal").textContent).toBe(
      `${formatAbsolute(NEXT_AT)}Two thirds through the certificate's life`,
    );
    expect(rowValue("Last error").textContent).toBe("None");
    expect(card().queryByText(/made this certificate/)).toBeNull();
  });

  it("says Renewed once the newest issue was a renewal", async () => {
    answer = () =>
      Response.json(
        httpsLetsEncryptStatus({
          certificate: {
            issuedAt: new Date(Date.now() - 2 * 86_400_000).toISOString(),
            names: ["tierplan.example.com"],
            notAfter: new Date(Date.now() + 88 * 86_400_000).toISOString(),
            notBefore: new Date(Date.now() - 2 * 86_400_000).toISOString(),
            renewed: true,
          },
        }),
      );
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Last renewed").textContent).toMatch(/^Renewed /);
  });

  it.each([
    ["ari", "Chosen by Let's Encrypt"],
    ["two_thirds", "Two thirds through the certificate's life"],
    ["half", "Half way through the certificate's life"],
  ] as const)("words the %s basis of the next renewal", async (basis, words) => {
    answer = () => Response.json(httpsLetsEncryptStatus({ renewal: { basis, nextAt: NEXT_AT } }));
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Next renewal")).getByText(words)).toBeTruthy();
    expect(within(rowValue("Next renewal")).getByText(formatAbsolute(NEXT_AT))).toBeTruthy();
  });

  it("marks a certificate from the test directory with a destructive Test CA badge", async () => {
    answer = () => Response.json(httpsTestCAStatus());
    renderCard();
    await card().findByText("Mode");
    const source = within(rowValue("Mode"));
    expect(source.getByText("Let's Encrypt")).toBeTruthy();
    const badge = source.getByText("Test CA");
    expect(badge.getAttribute("data-variant")).toBe("destructive");
  });

  it("says the renewals are paused after a restore, with no date", async () => {
    answer = () =>
      Response.json(httpsLetsEncryptStatus({ pausedReason: "restore", renewal: undefined }));
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Next renewal").textContent).toBe("Paused until you resume");
  });

  it("says renewals are paused by certificate files, with no date, though the server still plans one", async () => {
    // The server keeps the mode active and sends the planned renewal; the scheduler does nothing.
    answer = () =>
      Response.json(
        httpsCertFilesKeptStatus({
          renewal: { basis: "ari", nextAt: NEXT_AT },
        }),
      );
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Next renewal").textContent).toBe("Paused while certificate files are set");
    expect(card().queryByText(formatAbsolute(NEXT_AT))).toBeNull();
    expect(card().queryByText("Chosen by Let's Encrypt")).toBeNull();
  });

  it("shows the classified headline, when it happened and a link to the details below", async () => {
    answer = () =>
      Response.json(
        httpsLetsEncryptStatus({
          failures: 2,
          lastError: acmeLastError({ code: "caa_forbids", host: "tp.example.org" }),
        }),
      );
    renderCard();
    await card().findByText("Mode");
    const value = rowValue("Last error");
    expect(value.textContent).toContain("A CAA record forbids Let's Encrypt for tp.example.org.");
    expect(value.querySelector("time")).toBeTruthy();
    expect(within(value).getByRole("link", { name: "Details below" }).getAttribute("href")).toBe(
      "#lets-encrypt-last-run",
    );
  });

  it("shows an unknown class as the internal headline", async () => {
    answer = () =>
      Response.json(
        httpsLetsEncryptStatus({
          lastError: acmeLastError({ code: "from_the_future" as never }),
        }),
      );
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Last error").textContent).toContain("Something went wrong inside TierPlan.");
  });

  it("shows the self-signed badge, no self-signed note and the last error after an expiry fallback", async () => {
    answer = () => Response.json(httpsFallbackStatus());
    renderCard();
    await card().findByText("Mode");
    expect(within(rowValue("Mode")).getByText("Self-signed")).toBeTruthy();
    expect(card().queryByText(/made this certificate/)).toBeNull();
    expect(rowValue("Last error").textContent).toContain(
      "Let's Encrypt couldn't reach http://tierplan.example.com/.well-known/acme-challenge/.",
    );
    // The fallback serves no Let's Encrypt renewal date.
    expect(card().queryByText("Next renewal")).toBeNull();
  });

  it("has no Let's Encrypt rows when it is stopped, and keeps the self-signed note", async () => {
    answer = () =>
      Response.json(
        httpsStatus({
          acme: httpsLetsEncryptStatus({ mode: "off", renewal: undefined }).acme,
        }),
      );
    renderCard();
    await card().findByText("Mode");
    expect(labels()).toEqual(["Mode", "Issuer", "Names", "Expires"]);
    expect(card().getByText(/made this certificate/)).toBeTruthy();
  });

  it("shows only the Last error row after a failed first issue, which leaves the mode off", async () => {
    answer = () =>
      Response.json(
        httpsStatus({
          acme: httpsLetsEncryptStatus({
            mode: "off",
            renewal: undefined,
            certificate: undefined,
            failures: 1,
            lastError: acmeLastError(),
          }).acme,
        }),
      );
    renderCard();
    await card().findByText("Mode");
    expect(labels()).toEqual(["Mode", "Issuer", "Names", "Expires", "Last error"]);
    expect(rowValue("Last error").textContent).toContain(
      "Let's Encrypt couldn't reach http://tierplan.example.com/.well-known/acme-challenge/.",
    );
    const link = within(rowValue("Last error")).getByRole("link", { name: "Details below" });
    expect(link.getAttribute("href")).toBe("#lets-encrypt-last-run");
    expect(card().getByText(/made this certificate/)).toBeTruthy();
  });

  it("keeps a stopped certificate's dates out of the rows when an issue after Stop failed", async () => {
    answer = () =>
      Response.json(
        httpsStatus({
          acme: httpsLetsEncryptStatus({
            mode: "off",
            renewal: undefined,
            failures: 1,
            lastError: acmeLastError(),
          }).acme,
        }),
      );
    renderCard();
    await card().findByText("Mode");
    expect(labels()).toEqual(["Mode", "Issuer", "Names", "Expires", "Last error"]);
  });

  it("has no Let's Encrypt rows when the status carries no Let's Encrypt block", async () => {
    answer = () => Response.json({ ...httpsStatus(), acme: undefined });
    renderCard();
    await card().findByText("Mode");
    expect(labels()).toEqual(["Mode", "Issuer", "Names", "Expires"]);
  });

  it("says 'Not renewing' on a certificate close to its end while renewals are failing", async () => {
    const soon = new Date(Date.now() + 3.5 * 86_400_000).toISOString();
    const status = httpsLetsEncryptStatus({ failures: 2, lastError: acmeLastError() });
    answer = () =>
      Response.json({ ...status, certificate: { ...status.certificate, notAfter: soon } });
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Expires").textContent).toContain("Not renewing");
  });

  it("does not say 'Not renewing' while nothing has failed", async () => {
    const soon = new Date(Date.now() + 3.5 * 86_400_000).toISOString();
    const status = httpsLetsEncryptStatus();
    answer = () =>
      Response.json({ ...status, certificate: { ...status.certificate, notAfter: soon } });
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Expires").textContent).not.toContain("Not renewing");
  });

  it("wraps a long headline instead of widening the column", async () => {
    const host = `${"a".repeat(60)}.${"b".repeat(60)}.example.com`;
    answer = () => Response.json(httpsLetsEncryptStatus({ lastError: acmeLastError({ host }) }));
    renderCard();
    await card().findByText("Mode");
    expect(rowValue("Last error").className).toContain("break-all");
    expect(rowValue("Last error").textContent).toContain(host);
  });
});

describe("HTTPS page", () => {
  it("puts the heading and description above the certificate card", async () => {
    render(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <TestRouter>
            <HttpsPage />
          </TestRouter>
        </TooltipProvider>
      </QueryClientProvider>,
    );
    expect(screen.getByRole("heading", { level: 2, name: "HTTPS" })).toBeTruthy();
    expect(
      screen.getByText(
        "TierPlan always serves HTTPS. See the certificate it is using, or get a free one from Let's Encrypt that browsers trust.",
      ),
    ).toBeTruthy();
    await card().findByText("Mode");
  });
});
