// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { AcmeCard } from "@/features/https/acme-card";
import { deriveEnvOverlay } from "@/features/https/card-state";
import { HttpsPage } from "@/features/https/https-page";
import "@/i18n";
import {
  acmeActiveRun,
  acmeDNSStatus,
  acmeLastError,
  acmeSwitchLock,
  httpsACME,
  httpsCertFilesKeptStatus,
  httpsCertFilesStatus,
  httpsEnvOffEmptyStatus,
  httpsEnvOffStatus,
  httpsEnvOnStatus,
  httpsFallbackStatus,
  httpsLetsEncryptStatus,
  httpsNamesChangedStatus,
  httpsPendingStatus,
  httpsRestoredStatus,
  httpsStatus,
  httpsTestCAStatus,
} from "@/test-https";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

interface Sent {
  path: string;
  method: string;
  csrf: string | null;
}

let sent: Sent[];
let status: () => Response | Promise<Response>;
let me: () => Response;
let resume: () => Response | Promise<Response>;
let stop: () => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;
let errorToast: ReturnType<typeof vi.spyOn>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const viewer = () => Response.json(makeMe({ allEnvironments: "viewer" }));

const stateOf = () => document.querySelector("[data-state]")?.getAttribute("data-state");

function problem(code: number, type: string) {
  return Response.json(
    { type, title: "x", status: code, detail: "d" },
    { status: code, headers: { "Content-Type": "application/problem+json" } },
  );
}

beforeEach(() => {
  sent = [];
  status = () => Response.json(httpsStatus());
  me = instanceAdmin;
  resume = () => new Response(null, { status: 204 });
  stop = () => new Response(null, { status: 204 });
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  successToast = vi.spyOn(toast, "success");
  errorToast = vi.spyOn(toast, "error");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      sent.push({ path, method: req.method, csrf: req.headers.get("X-CSRF-Token") });
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return status();
      if (path === "/api/https/acme/directory") return Response.json({ profiles: [] });
      if (path === "/api/https/acme/resume" && req.method === "POST") return resume();
      if (path === "/api/https/acme/stop" && req.method === "POST") return stop();
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

function renderUI(ui: React.ReactNode) {
  render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>{ui}</TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "Let's Encrypt" }));

// The card, once the status and who is signed in have both arrived.
async function ready() {
  renderUI(<AcmeCard />);
  await card().findByText("Names on the certificate");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

// The text of every note in the card, the persistent state alerts among them.
const notes = () => card().queryAllByRole("note");
const noteWith = (title: string) =>
  notes().find((n) => within(n).queryByText(title) !== null) as HTMLElement | undefined;

describe("Paused after restore (ACME-08, D-20)", () => {
  it("shows the warning with a primary Resume button, and resuming clears it with a toast", async () => {
    status = () => Response.json(httpsRestoredStatus());
    resume = () => {
      status = () => Response.json(httpsLetsEncryptStatus());
      return new Response(null, { status: 204 });
    };
    await ready();

    const alert = noteWith("Paused after restore") as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain("This instance was restored from a backup.");
    const button = within(alert).getByRole("button", { name: "Resume Let's Encrypt" });
    fireEvent.click(button);

    await waitFor(() => expect(noteWith("Paused after restore")).toBeUndefined());
    const resumes = sent.filter((s) => s.path === "/api/https/acme/resume");
    expect(resumes).toHaveLength(1);
    expect(resumes[0]?.method).toBe("POST");
    expect(resumes[0]?.csrf).toBe("csrf-1");
    expect(successToast).toHaveBeenCalledTimes(1);
    const content = successToast.mock.calls[0]?.[0] as { props: { children: string } };
    expect(content.props.children).toBe("Let's Encrypt resumed.");
    expect(card().queryByRole("button", { name: "Resume Let's Encrypt" })).toBeNull();
  });
});

const accountOf = () => ({
  contact: "ops@example.com",
  registeredAt: "2026-11-10T08:00:00Z",
  termsOfServiceUrl: "",
});

describe("State alerts", () => {
  it("I: certificate files and no Let's Encrypt state say to remove them, and the card is disabled with no button", async () => {
    status = () => Response.json(httpsCertFilesStatus());
    await ready();
    expect(stateOf()).toBe("I");
    const sentence = "Remove TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE to use Let's Encrypt";
    const note = notes().find((n) => n.textContent?.includes(sentence));
    expect(note).toBeTruthy();
    expect(note?.textContent).toBe(`${sentence}.`);
    expect(note?.querySelector("[data-slot=alert-title]")).toBeNull();
    const vars = [...(note?.querySelectorAll("code") ?? [])];
    expect(vars.map((c) => c.textContent)).toEqual([
      "TIERPLAN_TLS_CERT_FILE",
      "TIERPLAN_TLS_KEY_FILE",
    ]);
    expect(vars.every((c) => c.className.includes("font-mono"))).toBe(true);
    // The form sits in a disabled fieldset, so the controls match :disabled.
    expect(card().getByLabelText("Contact email (optional)").matches(":disabled")).toBe(true);
    expect(card().getByRole("checkbox").matches(":disabled")).toBe(true);
    expect(card().queryAllByRole("button")).toHaveLength(0);
  });

  it("J: certificate files with Let's Encrypt state kept say what is kept, with no Stop", async () => {
    status = () => Response.json(httpsCertFilesKeptStatus());
    await ready();
    expect(stateOf()).toBe("J");
    const alert = noteWith("Paused by certificate files") as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain(
      "TierPlan keeps your Let's Encrypt certificate, order and account.",
    );
    expect(alert.textContent).toContain("Remove both variables and restart to pick them up again.");
    expect(alert.querySelectorAll("code")).toHaveLength(2);
    expect(card().queryByRole("button", { name: "Stop using Let's Encrypt" })).toBeNull();
    // The only button is the Advanced disclosure, which is shown disabled; there is no action.
    expect(
      card()
        .queryAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Advanced"]);
  });

  it("H: the restore pause shows Resume Let's Encrypt as the only filled button of the card", async () => {
    status = () => Response.json(httpsRestoredStatus());
    await ready();
    expect(stateOf()).toBe("H");
    const alert = noteWith("Paused after restore") as HTMLElement;
    expect(alert.textContent).toContain("so a copy can't use up the original's limits");
    expect(alert.textContent).toContain("The certificate being served is unchanged.");
    const filled = card()
      .getAllByRole("button")
      .filter((b) => b.className.includes("bg-primary"));
    expect(filled.map((b) => b.textContent)).toEqual(["Resume Let's Encrypt"]);
    expect(within(alert).getByRole("button", { name: "Resume Let's Encrypt" })).toBeTruthy();
    // Issue and Renew are there but not usable, and the paused line says why.
    const issue = card().getByRole("button", { name: "Issue certificate" });
    expect(issue.getAttribute("aria-disabled")).toBe("true");
    expect(card().getByText("Resume Let's Encrypt first.")).toBeTruthy();
  });

  it("E: changed names name both sets, in mono, and say the old names keep renewing", async () => {
    status = () => Response.json(httpsNamesChangedStatus());
    await ready();
    expect(stateOf()).toBe("E");
    const alert = noteWith("The names have changed") as HTMLElement;
    expect(alert.textContent).toContain("The certificate covers tierplan.example.com.");
    expect(alert.textContent).toContain(
      "The public URL and agent URL now need tierplan.example.org, agents.example.org.",
    );
    expect(alert.textContent).toContain(
      "TierPlan keeps renewing the old names until you issue again.",
    );
    expect([...alert.querySelectorAll("code")].map((c) => c.textContent)).toEqual([
      "tierplan.example.com",
      "tierplan.example.org",
      "agents.example.org",
    ]);
    expect(card().getByRole("button", { name: "Issue certificate" })).toBeTruthy();
  });

  it("K: an expired certificate shows the destructive fallback alert with the expiry date", async () => {
    const expiredAt = "2026-11-09T10:00:00Z";
    status = () => Response.json(httpsFallbackStatus({ fallback: { expiredAt } }));
    await ready();
    expect(stateOf()).toBe("K");
    const alert = noteWith("Serving the self-signed certificate") as HTMLElement;
    const date = new Intl.DateTimeFormat(undefined, {
      day: "numeric",
      month: "short",
      year: "numeric",
    }).format(new Date(expiredAt));
    expect(alert.textContent).toContain(
      `The Let's Encrypt certificate expired on ${date} because renewal kept failing.`,
    );
    expect(alert.textContent).toContain("Agents that use the self-signed pin stay connected.");
    expect(alert.className).toContain("text-destructive");
  });

  it("the alerts are notes, not announcements", async () => {
    status = () => Response.json(httpsRestoredStatus());
    await ready();
    expect((noteWith("Paused after restore") as HTMLElement).getAttribute("role")).toBe("note");
  });
});

describe("Test directory alert (D-19)", () => {
  const url = "https://acme-staging.example.test/directory";
  const heading = () => screen.getByRole("heading", { name: "HTTPS" });

  it("sits under the description and above the status card, whatever else is shown", async () => {
    status = () => Response.json(httpsTestCAStatus({ directoryUrl: url }));
    renderUI(<HttpsPage />);
    const alert = (await screen.findByText("Test certificate authority")).closest(
      "[data-slot=alert]",
    ) as HTMLElement;
    expect(alert.getAttribute("role")).toBe("note");
    expect(alert.textContent).toContain("TIERPLAN_ACME_DIRECTORY_URL is set, so TierPlan talks to");
    expect(alert.textContent).toContain("instead of Let's Encrypt.");
    expect(alert.textContent).toContain("isn't supported");
    const urlNode = within(alert).getByText(url);
    expect(urlNode.tagName).toBe("CODE");
    expect(urlNode.className).toContain("font-mono");
    const region = await screen.findByRole("region", { name: "Certificate in use" });
    expect(
      heading().compareDocumentPosition(alert) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(alert.compareDocumentPosition(region) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("is shown on top of the certificate-files state too, and not at all without an override", async () => {
    status = () =>
      Response.json(httpsCertFilesStatus({ directoryOverridden: true, directoryUrl: url }));
    renderUI(<HttpsPage />);
    expect(await screen.findByText("Test certificate authority")).toBeTruthy();
    await waitFor(() =>
      expect(document.body.textContent).toContain("Remove TIERPLAN_TLS_CERT_FILE and"),
    );
    cleanup();
    queryClient.clear();
    status = () => Response.json(httpsStatus());
    renderUI(<HttpsPage />);
    await screen.findByRole("region", { name: "Certificate in use" });
    expect(screen.queryByText("Test certificate authority")).toBeNull();
  });

  it("draws the address as text only, never as markup or a link", async () => {
    const hostile = '<img src=x onerror="alert(1)">';
    status = () => Response.json(httpsTestCAStatus({ directoryUrl: hostile }));
    renderUI(<HttpsPage />);
    const alert = (await screen.findByText("Test certificate authority")).closest(
      "[data-slot=alert]",
    ) as HTMLElement;
    expect(alert.textContent).toContain(hostile);
    expect(alert.querySelector("img")).toBeNull();
    expect(alert.querySelector("a")).toBeNull();
  });
});

describe("Resume Let's Encrypt (ACME-08)", () => {
  const press = () =>
    fireEvent.click(card().getByRole("button", { name: /^Resum(e Let's Encrypt|ing)/ }));

  it("is not offered without settings.write, but the explanation is", async () => {
    me = viewer;
    status = () => Response.json(httpsRestoredStatus());
    await ready();
    expect(noteWith("Paused after restore")).toBeTruthy();
    expect(card().queryByRole("button", { name: "Resume Let's Encrypt" })).toBeNull();
  });

  it("shows 'Resuming...' with a spinner while the request is in flight, and ignores a second press", async () => {
    status = () => Response.json(httpsRestoredStatus());
    let release: () => void = () => {};
    resume = () =>
      new Promise<Response>((resolve) => {
        release = () => {
          status = () => Response.json(httpsLetsEncryptStatus());
          resolve(new Response(null, { status: 204 }));
        };
      });
    await ready();
    press();
    await waitFor(() => expect(card().getByRole("button", { name: "Resuming..." })).toBeTruthy());
    const button = card().getByRole("button", { name: "Resuming..." });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(sent.filter((s) => s.path === "/api/https/acme/resume")).toHaveLength(1);
    release();
    await waitFor(() => expect(noteWith("Paused after restore")).toBeUndefined());
  });

  it("a 409 reads the status again and shows no error", async () => {
    status = () => Response.json(httpsRestoredStatus());
    resume = () => {
      status = () => Response.json(httpsLetsEncryptStatus());
      return problem(409, "acme_not_paused");
    };
    await ready();
    press();
    await waitFor(() => expect(noteWith("Paused after restore")).toBeUndefined());
    expect(errorToast).not.toHaveBeenCalled();
    expect(successToast).not.toHaveBeenCalled();
  });

  it("any other failure toasts an error and leaves the alert for another try", async () => {
    status = () => Response.json(httpsRestoredStatus());
    resume = () => problem(500, "about:blank");
    await ready();
    press();
    await waitFor(() => expect(errorToast).toHaveBeenCalledTimes(1));
    expect(noteWith("Paused after restore")).toBeTruthy();
    expect(card().getByRole("button", { name: "Resume Let's Encrypt" })).toBeTruthy();
  });
});

describe("Stop row (D-14)", () => {
  const stopButton = () => card().queryByRole("button", { name: "Stop using Let's Encrypt" });

  const served = (): [string, HTTPSStatus][] => [
    ["D", httpsLetsEncryptStatus()],
    ["E", httpsNamesChangedStatus()],
    ["F", httpsLetsEncryptStatus({ lastError: acmeLastError(), failures: 1 })],
    ["K", httpsFallbackStatus()],
    ["H", httpsRestoredStatus()],
  ];

  for (const [state, value] of served()) {
    it(`is offered in state ${state} with settings.write`, async () => {
      status = () => Response.json(value);
      await ready();
      expect(stateOf()).toBe(state);
      const button = stopButton() as HTMLElement;
      expect(button).toBeTruthy();
      expect(button.getAttribute("aria-disabled")).toBe("false");
      expect(button.className).toContain("text-destructive");
    });
  }

  it("is the last thing in the card, after a separator", async () => {
    status = () => Response.json(httpsLetsEncryptStatus());
    await ready();
    const body = document.querySelector("[data-state]") as HTMLElement;
    const last = body.lastElementChild as HTMLElement;
    expect(last.contains(stopButton())).toBe(true);
    expect(last.previousElementSibling?.getAttribute("data-slot")).toBe("separator");
  });

  const absent: [string, HTTPSStatus][] = [
    ["A", httpsStatus()],
    ["B", httpsStatus({ acme: httpsACME({ account: accountOf(), hasState: true }) })],
    ["I", httpsCertFilesStatus()],
    ["J", httpsCertFilesKeptStatus()],
    [
      "L",
      {
        ...httpsLetsEncryptStatus({ mode: "off" }),
        certificate: httpsStatus().certificate,
      },
    ],
  ];
  for (const [state, value] of absent) {
    it(`is not offered in state ${state}`, async () => {
      status = () => Response.json(value);
      await ready();
      expect(stateOf()).toBe(state);
      expect(stopButton()).toBeNull();
    });
  }

  it("is not offered without settings.write", async () => {
    me = viewer;
    status = () => Response.json(httpsLetsEncryptStatus());
    await ready();
    expect(stopButton()).toBeNull();
  });

  it("is aria-disabled while a run is active: focusable, and pressing it opens nothing", async () => {
    status = () =>
      Response.json(httpsLetsEncryptStatus({ activeRun: acmeActiveRun({ kind: "renew" }) }));
    await ready();
    expect(stateOf()).toBe("C");
    const button = stopButton() as HTMLElement;
    expect(button.getAttribute("aria-disabled")).toBe("true");
    expect(button.hasAttribute("disabled")).toBe(false);
    expect(card().getByText("A run is already in progress.").id).toBe(
      button.getAttribute("aria-describedby"),
    );
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("opens the confirmation with Cancel focused, and confirming puts the status card back on Self-signed", async () => {
    status = () => Response.json(httpsLetsEncryptStatus());
    stop = () => {
      status = () =>
        Response.json(httpsStatus({ acme: httpsACME({ account: accountOf(), hasState: true }) }));
      return new Response(null, { status: 204 });
    };
    renderUI(<HttpsPage />);
    const certificate = await screen.findByRole("region", { name: "Certificate in use" });
    await within(certificate).findByText("Let's Encrypt");
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    fireEvent.click(
      await within(await screen.findByRole("region", { name: "Let's Encrypt" })).findByRole(
        "button",
        {
          name: "Stop using Let's Encrypt",
        },
      ),
    );
    const dialog = within(await screen.findByRole("alertdialog"));
    await waitFor(() =>
      expect(document.activeElement).toBe(
        dialog.getByRole("button", { name: "Keep Let's Encrypt" }),
      ),
    );
    fireEvent.click(dialog.getByRole("button", { name: "Stop using Let's Encrypt" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    const stops = sent.filter((s) => s.path === "/api/https/acme/stop");
    expect(stops).toHaveLength(1);
    expect(stops[0]?.csrf).toBe("csrf-1");
    expect(within(certificate).getByText("Self-signed")).toBeTruthy();
    expect(successToast).toHaveBeenCalledTimes(1);
    const content = successToast.mock.calls[0]?.[0] as { props: { children: string } };
    expect(content.props.children).toBe("Back on the self-signed certificate.");
    // Stopped (state L or B): the row is gone with it.
    expect(
      within(screen.getByRole("region", { name: "Let's Encrypt" })).queryByRole("button", {
        name: "Stop using Let's Encrypt",
      }),
    ).toBeNull();
  });
});

describe("TIERPLAN_ACME=on, waiting for the first certificate (D-07, UI-SPEC J5)", () => {
  const TITLE = "TierPlan is getting a certificate";

  it("says TierPlan orders by itself, asks for no click and adds no state letter", async () => {
    status = () => Response.json(httpsEnvOnStatus());
    await ready();
    expect(stateOf()).toBe("B");
    const alert = noteWith(TITLE) as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain("orders the certificate by itself as soon as it is ready");
    expect(alert.textContent).toContain("You don't need to press anything.");
    expect(alert.textContent).toContain("The steps show here while it runs.");
    expect([...alert.querySelectorAll("code")].map((c) => c.textContent)).toEqual([
      "TIERPLAN_ACME",
    ]);
    expect(alert.getAttribute("role")).toBe("note");
  });

  it("is not shown while a start-time order runs, and the run panel takes over", async () => {
    status = () =>
      Response.json(
        httpsEnvOnStatus({ activeRun: acmeActiveRun({ trigger: "schedule", kind: "issue" }) }),
      );
    await ready();
    expect(stateOf()).toBe("C");
    expect(noteWith(TITLE)).toBeUndefined();
  });

  it("is not shown once the order failed, refused or was told to wait", async () => {
    status = () => Response.json(httpsEnvOnStatus({ lastError: acmeLastError(), failures: 1 }));
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
    cleanup();
    queryClient.clear();

    status = () =>
      Response.json(
        httpsEnvOnStatus({
          refusals: [{ code: "names_ip", host: "10.0.0.1" }],
        }),
      );
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
    cleanup();
    queryClient.clear();

    status = () =>
      Response.json(
        httpsEnvOnStatus({
          blocked: {
            reason: "told_to_wait",
            until: new Date(Date.now() + 3_600_000).toISOString(),
          },
        }),
      );
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
  });

  it("is not shown when the variable is unset, or once a certificate is served", async () => {
    status = () => Response.json(httpsPendingStatus());
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
    cleanup();
    queryClient.clear();

    status = () =>
      Response.json(
        httpsLetsEncryptStatus({ locks: { acme: { var: "TIERPLAN_ACME", value: "on" } } }),
      );
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
  });

  it("gives way to the pause alerts, which say what is missing", async () => {
    status = () => Response.json(httpsEnvOnStatus({ pausedReason: "restore" }));
    await ready();
    expect(stateOf()).toBe("H");
    expect(noteWith(TITLE)).toBeUndefined();
  });
});

describe("TIERPLAN_ACME=on on the other states (D-08, D-09)", () => {
  const on = { locks: acmeSwitchLock("on") };

  it("E: the names-changed alert ends with the environment sentence instead of the renewal one", async () => {
    status = () => Response.json(httpsNamesChangedStatus(on));
    await ready();
    expect(stateOf()).toBe("E");
    const alert = noteWith("The names have changed") as HTMLElement;
    expect(alert.textContent).toContain("The certificate covers tierplan.example.com.");
    expect(
      alert.textContent?.endsWith(
        "TIERPLAN_ACME is on, so TierPlan orders a certificate for the new names by itself.",
      ),
    ).toBe(true);
    expect(alert.textContent).not.toContain("keeps renewing the old names");
    expect([...alert.querySelectorAll("code")].map((c) => c.textContent)).toEqual([
      "tierplan.example.com",
      "tierplan.example.org",
      "agents.example.org",
      "TIERPLAN_ACME",
    ]);
  });

  it("H: the restore alert keeps Resume Let's Encrypt and says TierPlan orders again as soon as you resume", async () => {
    status = () => Response.json(httpsRestoredStatus(on));
    await ready();
    expect(stateOf()).toBe("H");
    const alert = noteWith("Paused after restore") as HTMLElement;
    expect(alert.textContent).toContain("The certificate being served is unchanged.");
    expect(alert.textContent).toContain(
      "TIERPLAN_ACME is on, so TierPlan orders again as soon as you resume.",
    );
    expect(within(alert).getByRole("button", { name: "Resume Let's Encrypt" })).toBeTruthy();
  });

  it("I and J: the certificate-files alerts add that TIERPLAN_ACME starts working when the files are removed", async () => {
    const sentence =
      "TIERPLAN_ACME is on and starts working when the certificate files are removed.";
    status = () => Response.json(httpsCertFilesStatus(on));
    await ready();
    expect(stateOf()).toBe("I");
    expect(notes().some((n) => n.textContent?.includes(sentence))).toBe(true);
    cleanup();
    queryClient.clear();

    status = () => Response.json(httpsCertFilesKeptStatus(on));
    await ready();
    expect(stateOf()).toBe("J");
    expect((noteWith("Paused by certificate files") as HTMLElement).textContent).toContain(
      sentence,
    );
  });

  it("leaves every alert as in Phases 14 and 15 when the variable is unset", async () => {
    status = () => Response.json(httpsRestoredStatus());
    await ready();
    expect((noteWith("Paused after restore") as HTMLElement).textContent).not.toContain(
      "TIERPLAN_ACME",
    );
    cleanup();
    queryClient.clear();

    status = () => Response.json(httpsCertFilesStatus());
    await ready();
    expect(notes().some((n) => n.textContent?.includes("TIERPLAN_ACME"))).toBe(false);
  });
});

describe("TIERPLAN_ACME=off (D-02, D-04)", () => {
  const TITLE = "Let's Encrypt is turned off by TIERPLAN_ACME";
  // The variable name is its own element, so the title is found by its text content.
  const warning = () =>
    notes().find((n) => n.querySelector("[data-slot=alert-title]")?.textContent === TITLE) as
      | HTMLElement
      | undefined;

  it("with a certificate, order and account kept, warns and says they are kept", async () => {
    status = () => Response.json(httpsEnvOffStatus());
    await ready();
    const alert = warning() as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.className).toContain("border-status-warning");
    expect(alert.textContent).toContain(
      "TierPlan serves its self-signed certificate and makes no Let's Encrypt requests.",
    );
    expect(alert.textContent).toContain(
      "Your Let's Encrypt certificate, order and account are kept.",
    );
    expect(alert.textContent).toContain(
      "Remove TIERPLAN_ACME, or set it to on, to use them again.",
    );
    expect(
      [...alert.querySelectorAll("code")].every((c) => c.className.includes("font-mono")),
    ).toBe(true);
    // The mode is off, so there is no Stop row.
    expect(card().queryByRole("button", { name: "Stop using Let's Encrypt" })).toBeNull();
  });

  it("with nothing stored, is one sentence in an info alert", async () => {
    status = () => Response.json(httpsEnvOffEmptyStatus());
    await ready();
    expect(warning()).toBeUndefined();
    const note = notes().find(
      (n) => n.textContent === "Let's Encrypt is turned off by TIERPLAN_ACME.",
    );
    expect(note).toBeTruthy();
    expect(note?.className).not.toContain("border-status-warning");
    expect(note?.querySelector("[data-slot=alert-title]")).toBeNull();
    expect(card().queryByRole("button", { name: "Stop using Let's Encrypt" })).toBeNull();
  });

  it("is not drawn when the variable is unset", async () => {
    status = () => Response.json(httpsStatus());
    await ready();
    expect(notes().some((n) => n.textContent?.includes("turned off by TIERPLAN_ACME"))).toBe(false);
  });
});

describe("TIERPLAN_ACME=on with DNS-01 and no token anywhere (D-07)", () => {
  const TITLE = "Waiting for a Cloudflare token";
  const GETTING = "TierPlan is getting a certificate";
  const noToken = (dns: Parameters<typeof acmeDNSStatus>[0] = {}) =>
    httpsEnvOnStatus({
      challengeType: "dns-01",
      dns: acmeDNSStatus({ tokenSource: "none", ...dns }),
    });

  it("shows the warning with the variable names in mono, and no error panel or other waiting alert", async () => {
    status = () => Response.json(noToken());
    await ready();
    const alert = noteWith(TITLE) as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain(
      "TIERPLAN_ACME is on and DNS-01 is selected, but there is no token. TierPlan orders nothing until there is one. Add a token above, or set TIERPLAN_CLOUDFLARE_API_TOKEN_FILE.",
    );
    expect([...alert.querySelectorAll("code")].map((c) => c.textContent)).toEqual([
      "TIERPLAN_ACME",
      "TIERPLAN_CLOUDFLARE_API_TOKEN_FILE",
    ]);
    expect(alert.getAttribute("role")).toBe("note");
    expect(noteWith(GETTING)).toBeUndefined();
    expect(card().queryByText(/Next automatic attempt/)).toBeNull();
    expect(card().queryByText(/TierPlan keeps trying/)).toBeNull();
  });

  it("is not shown when the environment supplies the token: the ordinary waiting alert shows", async () => {
    status = () => Response.json(noToken({ tokenSource: "env" }));
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
    expect(noteWith(GETTING)).toBeTruthy();
  });

  it("is not shown when a saved token is in use", async () => {
    status = () => Response.json(noToken({ tokenSource: "stored", tokenSaved: true }));
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
  });

  it("is not shown under HTTP-01, or with TIERPLAN_ACME unset", async () => {
    status = () =>
      Response.json(httpsEnvOnStatus({ challengeType: "http-01", dns: acmeDNSStatus() }));
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
    cleanup();
    queryClient.clear();

    status = () =>
      Response.json(
        httpsPendingStatus({
          challengeType: "dns-01",
          dns: acmeDNSStatus({ tokenSource: "none" }),
        }),
      );
    await ready();
    expect(noteWith(TITLE)).toBeUndefined();
  });

  it("reads from deriveEnvOverlay: waitingForToken is set and waiting is not", () => {
    expect(deriveEnvOverlay(noToken())).toMatchObject({ waitingForToken: true, waiting: false });
    expect(deriveEnvOverlay(noToken({ tokenSource: "env" }))).toMatchObject({
      waitingForToken: false,
      waiting: true,
    });
    expect(deriveEnvOverlay(httpsPendingStatus()).waitingForToken).toBe(false);
  });
});
