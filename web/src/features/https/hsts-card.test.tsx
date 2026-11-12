// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { HTTPSStatus } from "@/api/https";
import { queryClient } from "@/api/query-client";
import { hasEnvLocks } from "@/features/https/card-state";
import { formatDate } from "@/features/https/copy";
import { HstsCard } from "@/features/https/hsts-card";
import "@/i18n";
import { formatAbsolute } from "@/lib/format";
import { hstsBody, hstsLock, hstsNotSent, hstsSending, httpsHSTSStatus } from "@/test-https";
import { makeMe } from "@/test-me";

interface Sent {
  path: string;
  method: string;
  body?: unknown;
}

let sent: Sent[];
let status: () => HTTPSStatus;
let me: () => Response;
let putAnswer: () => Response | Promise<Response>;
let successToast: ReturnType<typeof vi.spyOn>;

const instanceAdmin = () => Response.json(makeMe({ instanceAdmin: true }));
const viewer = () => Response.json(makeMe({ allEnvironments: "viewer" }));

beforeEach(() => {
  sent = [];
  status = () => httpsHSTSStatus();
  me = instanceAdmin;
  putAnswer = () => Response.json(hstsSending(1));
  successToast = vi.spyOn(toast, "success");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const entry: Sent = { path, method: req.method };
      if (req.method === "PUT") entry.body = await req.clone().json();
      sent.push(entry);
      if (path === "/api/me") return me();
      if (path === "/api/https/status") return Response.json(status());
      if (path === "/api/https/hsts" && req.method === "PUT") return putAnswer();
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

function renderCard() {
  render(
    <QueryClientProvider client={queryClient}>
      <HstsCard />
    </QueryClientProvider>,
  );
}

const card = () => within(screen.getByRole("region", { name: "HSTS" }));
const switchControl = () => card().getByRole("switch", { name: "Send HSTS" });
const saveButton = () => card().queryByRole("button", { name: /^(Save HSTS setting|Saving)/ });
const puts = () => sent.filter((s) => s.method === "PUT");
const toasts = () =>
  successToast.mock.calls.map(
    (c: unknown[]) => (c[0] as { props: { children: string } }).props.children,
  );

// The Lifetime row's value (the ramp below repeats every max-age, so the row is read by its label).
const lifetime = () => card().getByText("Lifetime").nextElementSibling?.textContent;

// The card, once the status and who is signed in have both arrived.
async function ready() {
  renderCard();
  await card().findByText("Status");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
}

describe("HSTS card: the setting (TLS-06, D-10, J4)", () => {
  it("is the third card's anchor and says HSTS is off by default and never adds includeSubDomains or preload", async () => {
    await ready();
    expect(document.getElementById("hsts")?.tagName.toLowerCase()).toBe("section");
    expect(card().getByText(/HSTS tells browsers to use only HTTPS for this address/)).toBeTruthy();
    expect(
      card().getByText(
        "Off by default. Once a browser has seen HSTS it refuses plain HTTP for this address until the lifetime ends. TierPlan never adds includeSubDomains or preload.",
      ),
    ).toBeTruthy();
    expect(switchControl().getAttribute("aria-checked")).toBe("false");
  });

  it("switching on only changes a draft: Save sends {enabled: true}, toasts, and the refetched status reads Sending at max-age=300", async () => {
    await ready();
    fireEvent.click(switchControl());
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    // The lifetime promise appears under the helper while the draft is on and the saved choice is off.
    expect(
      card().getByText(
        "The lifetime starts at 5 minutes and only grows while TierPlan serves a trusted certificate without a problem.",
      ),
    ).toBeTruthy();
    expect(puts()).toEqual([]);

    status = () => httpsHSTSStatus(hstsSending(1));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(toasts()).toEqual(["HSTS turned on."]));
    expect(puts()).toEqual([{ path: "/api/https/hsts", method: "PUT", body: { enabled: true } }]);

    expect(await card().findByText("Sending")).toBeTruthy();
    expect(lifetime()).toBe("max-age=300step 1 of 4, 5 minutes");
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("true");
  });

  it("switching off sends {enabled: false}, toasts 'HSTS turned off.' and says browsers are told to forget it while the draft is off", async () => {
    status = () => httpsHSTSStatus(hstsSending(2));
    await ready();
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    fireEvent.click(switchControl());
    expect(
      card().getByText(
        "Browsers that visit while a trusted certificate is served are told to forget HSTS (max-age=0).",
      ),
    ).toBeTruthy();

    status = () => httpsHSTSStatus(hstsBody({ state: "off_clearing", clearing: true }));
    putAnswer = () => Response.json(hstsBody({ state: "off_clearing", clearing: true }));
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(toasts()).toEqual(["HSTS turned off."]));
    expect(puts()[0]?.body).toEqual({ enabled: false });
    expect(
      await card().findByText(/TierPlan tells browsers to forget HSTS \(max-age=0\)\. This also/),
    ).toBeTruthy();
  });

  it("Save is aria-disabled and does nothing while the draft equals the stored value", async () => {
    await ready();
    const save = saveButton() as HTMLElement;
    expect(save.getAttribute("aria-disabled")).toBe("true");
    expect(save.hasAttribute("disabled")).toBe(false);
    fireEvent.click(save);
    // Switching on and back off leaves nothing to save.
    fireEvent.click(switchControl());
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("false");
    fireEvent.click(switchControl());
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(saveButton() as HTMLElement);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(puts()).toEqual([]);
  });

  it("Save HSTS setting is an outline button, so the card adds no filled button", async () => {
    await ready();
    const save = saveButton() as HTMLElement;
    expect(save.getAttribute("data-variant")).toBe("outline");
  });

  it("while the status is pending shows skeleton rows and a disabled Switch", () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    renderCard();
    expect(screen.getByTestId("hsts-loading")).toBeTruthy();
    const control = card().getByRole("switch", { name: "Send HSTS" });
    expect(control.hasAttribute("disabled")).toBe(true);
    expect(saveButton()).toBeNull();
  });

  it("a failed save shows 'Something went wrong. Try again.' in the card and keeps the draft", async () => {
    putAnswer = () =>
      Response.json(
        { type: "about:blank", title: "x", status: 500, detail: "d" },
        { status: 500, headers: { "Content-Type": "application/problem+json" } },
      );
    await ready();
    fireEvent.click(switchControl());
    fireEvent.click(saveButton() as HTMLElement);
    expect(await card().findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(toasts()).toEqual([]);
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    expect(saveButton()?.getAttribute("aria-disabled")).toBe("false");
  });
});

describe("HSTS card: the Status row (D-12, D-23)", () => {
  it("off with nothing sent: Off, 'Nothing is sent.', no Lifetime row, no steps", async () => {
    await ready();
    expect(card().getByText("Off")).toBeTruthy();
    expect(card().getByText("Nothing is sent.")).toBeTruthy();
    expect(card().queryByText("Lifetime")).toBeNull();
  });

  it("off, clearing: Off with max-age=0 and the earlier one-year header note", async () => {
    status = () => httpsHSTSStatus(hstsBody({ state: "off_clearing", clearing: true }));
    await ready();
    expect(card().getByText("Off")).toBeTruthy();
    expect(
      card().getByText(
        "TierPlan tells browsers to forget HSTS (max-age=0). This also clears the one-year setting that earlier versions sent for certificate files.",
      ),
    ).toBeTruthy();
  });

  it("sending: the value, the max-age and the step in words", async () => {
    status = () => httpsHSTSStatus(hstsSending(2));
    await ready();
    expect(card().getByText("Sending")).toBeTruthy();
    expect(card().getByText("The browser keeps the policy for the lifetime shown.")).toBeTruthy();
    expect(lifetime()).toBe("max-age=604800step 2 of 4, 1 week");
  });
});

describe("HSTS card: without settings.write", () => {
  it("disables the Switch, draws no Save and shows the read-only line, and still shows the status", async () => {
    me = viewer;
    status = () => httpsHSTSStatus(hstsSending(1));
    renderCard();
    await card().findByText("Sending");
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    expect(switchControl().hasAttribute("disabled")).toBe(true);
    expect(saveButton()).toBeNull();
    expect(card().getByText("You can view these settings but not change them.")).toBeTruthy();
  });
});

// The rest of the Status row (UI-SPEC "Status values"): not sent with its reason, the clearing variant
// (D-23), and withdrawn (D-12).
describe("HSTS card: not sent, and why (D-25, E9)", () => {
  const WHY = "Why";

  it("on but not sent: the status, its extra line, and the reason sentence with its fix", async () => {
    status = () => httpsHSTSStatus(hstsNotSent("self_signed"));
    await ready();
    expect(card().getByText("On, but not sent")).toBeTruthy();
    expect(
      card().getByText("HSTS is only sent over a certificate that browsers trust."),
    ).toBeTruthy();
    expect(card().getByText(WHY)).toBeTruthy();
    expect(card().getByText("It is self-signed.")).toBeTruthy();
    expect(card().getByText("Use a certificate from a public certificate authority.")).toBeTruthy();
    // Nothing is sent, so there is no lifetime to show.
    expect(card().queryByText("Lifetime")).toBeNull();
    expect(card().queryByText("Next step")).toBeNull();
  });

  it("not sent with clearing: the D-23 line about max-age=0 and the earlier one-year header", async () => {
    status = () => httpsHSTSStatus(hstsNotSent("unknown_authority", { clearing: true }));
    await ready();
    expect(card().getByText("On, but not sent")).toBeTruthy();
    expect(
      card().getByText(
        "TierPlan sends max-age=0, so a browser that trusts this certificate forgets the one-year HSTS that earlier versions sent. A self-signed certificate gets no HSTS header at all.",
      ),
    ).toBeTruthy();
    expect(
      card().queryByText("HSTS is only sent over a certificate that browsers trust."),
    ).toBeNull();
  });

  it.each([
    ["self_signed", "It is self-signed."],
    ["unknown_authority", "It isn't signed by an authority this server trusts."],
    ["expired", "It has expired or isn't valid yet."],
    ["name_mismatch", "It doesn't cover"],
    ["no_public_url", "No public URL is set, so TierPlan can't check the name."],
  ] as const)("the %s reason has a sentence", async (reason, sentence) => {
    status = () => httpsHSTSStatus(hstsNotSent(reason));
    await ready();
    expect(card().getByText(new RegExp(sentence.replace(/[.'?]/g, "\\$&")))).toBeTruthy();
  });

  it("a certificate from a test directory gives the test_ca reason", async () => {
    const lets = httpsHSTSStatus(hstsNotSent("unknown_authority"));
    status = () => ({ ...lets, certificate: { ...lets.certificate, testCA: true } });
    await ready();
    expect(card().getByText("It comes from a test certificate authority.")).toBeTruthy();
    expect(card().getByText("Test certificates aren't trusted by browsers.")).toBeTruthy();
  });

  it("a reason this build does not know reads as unknown_authority", async () => {
    status = () =>
      httpsHSTSStatus(hstsNotSent("unknown_authority", { notSentReason: "brand_new" as never }));
    await ready();
    expect(card().getByText("It isn't signed by an authority this server trusts.")).toBeTruthy();
  });

  it("a long host in name_mismatch is drawn in mono and breaks anywhere", async () => {
    const host = `${"a-very-long-label-".repeat(8)}example.com`;
    const lets = httpsHSTSStatus(hstsNotSent("name_mismatch"));
    status = () => ({
      ...lets,
      certificate: { ...lets.certificate, trust: { checked: true, verified: false, host } },
    });
    await ready();
    const hostNodes = card().getAllByText(host);
    expect(hostNodes.length).toBeGreaterThan(0);
    for (const node of hostNodes) {
      expect(node.tagName.toLowerCase()).toBe("code");
      expect(node.className).toContain("break-all");
      expect(node.className).toContain("font-mono");
    }
  });
});

describe("HSTS card: withdrawn (D-12, D-14)", () => {
  const NOT_AFTER = "2026-12-01T10:00:00Z";
  const date = () => formatDate(NOT_AFTER);

  it("Let's Encrypt, renewal failing: the status, the notice with the expiry date, in a warning note", async () => {
    status = () =>
      httpsHSTSStatus({
        enabled: true,
        storedEnabled: true,
        state: "withdrawn",
        clearing: true,
        withdrawal: { notAfter: NOT_AFTER, source: "lets_encrypt" },
      });
    await ready();
    expect(card().getByText("Withdrawn for now")).toBeTruthy();
    const title = card().getByText("HSTS is withdrawn until the certificate is renewed");
    const notice = title.closest("[data-slot=alert]") as HTMLElement;
    expect(notice.getAttribute("role")).toBe("note");
    expect(notice.className).not.toContain("destructive");
    expect(
      within(notice).getByText(
        new RegExp(
          `The certificate expires on ${date().replace(/[.]/g, "\\.")} and renewal isn't working\\.`,
        ),
      ),
    ).toBeTruthy();
    expect(
      within(notice).getByText(/would refuse the self-signed certificate that takes over/),
    ).toBeTruthy();
    expect(within(notice).getByText(/the lifetime starts again at 5 minutes/)).toBeTruthy();
  });

  it("certificate files, near expiry: the files wording with the expiry date", async () => {
    status = () =>
      httpsHSTSStatus({
        enabled: true,
        storedEnabled: true,
        state: "withdrawn",
        clearing: true,
        withdrawal: { notAfter: NOT_AFTER, source: "cert_files" },
      });
    await ready();
    expect(card().getByText("HSTS is withdrawn until the certificate is replaced")).toBeTruthy();
    expect(card().getByText(/When the files hold a new certificate, HSTS comes back/)).toBeTruthy();
    expect(card().queryByText(/renewal isn't working/)).toBeNull();
  });

  it("any other state draws no notice", async () => {
    status = () => httpsHSTSStatus(hstsSending(1));
    await ready();
    expect(card().queryByText(/HSTS is withdrawn until/)).toBeNull();
  });
});

describe("HSTS card: the lifetime steps (D-11, D-22, J7)", () => {
  const stepList = () => card().getByRole("list", { name: "Lifetime steps" });
  const items = () => within(stepList()).getAllByRole("listitem");

  it("is an ordered list of four with 5 minutes, 1 week, 1 month and 1 year and their max-age values", async () => {
    status = () => httpsHSTSStatus(hstsSending(2));
    await ready();
    expect(stepList().tagName.toLowerCase()).toBe("ol");
    expect(items()).toHaveLength(4);
    expect(items().map((li) => li.textContent)).toEqual([
      expect.stringContaining("5 minutes"),
      expect.stringContaining("1 week"),
      expect.stringContaining("1 month"),
      expect.stringContaining("1 year"),
    ]);
    for (const [i, age] of [300, 604800, 2592000, 31536000].entries()) {
      expect(within(items()[i] as HTMLElement).getByText(`max-age=${age}`)).toBeTruthy();
    }
  });

  it("sending at step 2: step 1 Done, step 2 Current in words, steps 3 and 4 Later", async () => {
    status = () => httpsHSTSStatus(hstsSending(2));
    await ready();
    const [one, two, three, four] = items() as HTMLElement[];
    expect(one?.getAttribute("data-state")).toBe("done");
    expect(within(one as HTMLElement).getByText("(Done)")).toBeTruthy();
    expect(two?.getAttribute("data-state")).toBe("current");
    expect(within(two as HTMLElement).getByText("(Current)")).toBeTruthy();
    // The current step is named in words as well as for a screen reader.
    expect(within(two as HTMLElement).getByText("Current")).toBeTruthy();
    for (const later of [three, four]) {
      expect(later?.getAttribute("data-state")).toBe("later");
      expect(within(later as HTMLElement).getByText("(Later)")).toBeTruthy();
    }
    expect(card().queryByText("The lifetime starts at step 1 when HSTS is sent.")).toBeNull();
  });

  it("the Next step row gives the absolute time, with the condition", async () => {
    const nextStepAt = "2026-12-05T09:30:00Z";
    status = () => httpsHSTSStatus(hstsSending(2, { nextStepAt }));
    await ready();
    expect(card().getByText("Next step")).toBeTruthy();
    expect(card().getByText(formatAbsolute(nextStepAt))).toBeTruthy();
    expect(card().getByText("if nothing goes wrong before then")).toBeTruthy();
  });

  it("step 4 reads Final step and gives no time", async () => {
    status = () => httpsHSTSStatus(hstsSending(4));
    await ready();
    expect(card().getByText("Final step")).toBeTruthy();
    expect(card().queryByText("if nothing goes wrong before then")).toBeNull();
    expect(items().at(-1)?.getAttribute("data-state")).toBe("current");
  });

  it("a short-lived certificate caps the ramp at a week: steps 3 and 4 are Not used, and Next step says so", async () => {
    status = () => httpsHSTSStatus(hstsSending(2, { capped: true, nextStepAt: undefined }));
    await ready();
    const [, two, three, four] = items() as HTMLElement[];
    expect(two?.getAttribute("data-state")).toBe("current");
    for (const unused of [three, four]) {
      expect(unused?.getAttribute("data-state")).toBe("unused");
      expect(within(unused as HTMLElement).getByText("Not used for this certificate")).toBeTruthy();
      expect(within(unused as HTMLElement).getByText("(Not used)")).toBeTruthy();
    }
    expect(card().getByText("Capped at 1 week for this certificate")).toBeTruthy();
  });

  it.each([
    ["not sent", () => httpsHSTSStatus(hstsNotSent("self_signed"))],
    [
      "withdrawn",
      () =>
        httpsHSTSStatus({
          enabled: true,
          storedEnabled: true,
          state: "withdrawn",
          clearing: true,
          withdrawal: { notAfter: "2026-12-01T10:00:00Z", source: "lets_encrypt" },
        }),
    ],
  ])(
    "%s: every step is Later and the list says the lifetime starts at step 1",
    async (_name, make) => {
      status = make;
      await ready();
      for (const li of items()) {
        expect(li.getAttribute("data-state")).toBe("later");
        expect(within(li).getByText("(Later)")).toBeTruthy();
      }
      expect(card().getByText("The lifetime starts at step 1 when HSTS is sent.")).toBeTruthy();
    },
  );

  it("is hidden while the setting is off, and carries its foot note while on", async () => {
    await ready();
    expect(card().queryByRole("list", { name: "Lifetime steps" })).toBeNull();
    expect(card().queryByText(/A step lasts a day, a week and a month/)).toBeNull();
    cleanup();
    queryClient.clear();
    status = () => httpsHSTSStatus(hstsSending(1));
    await ready();
    expect(
      card().getByText(
        "A step lasts a day, a week and a month. A withdrawal, an untrusted certificate or a new public host starts again at step 1. A restore starts again too.",
      ),
    ).toBeTruthy();
  });

  it("never uses the destructive variant for HSTS", async () => {
    status = () => httpsHSTSStatus(hstsNotSent("self_signed"));
    await ready();
    expect(document.querySelector("#hsts [data-variant=destructive]")).toBeNull();
  });
});

describe("HSTS card: TIERPLAN_HSTS locks the setting (D-10, D-17, SET-02)", () => {
  it("the Switch is disabled in the effective position, Save is not drawn, and the lock lines name the variable and the stored value", async () => {
    status = () => httpsHSTSStatus({ ...hstsSending(1), ...hstsLock("on", "off") });
    await ready();
    const control = switchControl();
    expect(control.hasAttribute("disabled")).toBe(true);
    expect(control.getAttribute("aria-checked")).toBe("true");
    expect(saveButton()).toBeNull();
    const note = document.getElementById("hsts-lock") as HTMLElement;
    expect(note.textContent).toBe(
      "Set by the TIERPLAN_HSTS environment variable.Stored value: off, used if TIERPLAN_HSTS is removed.",
    );
    expect(control.getAttribute("aria-describedby")).toContain("hsts-lock");
    expect(within(note).getByRole("img", { name: "Locked" })).toBeTruthy();
    // The stored draft helper is not shown for a locked setting.
    expect(card().queryByText(/The lifetime starts at 5 minutes and only grows/)).toBeNull();
  });

  it("locked off over a stored on: the Switch reads off and the stored value is on", async () => {
    status = () =>
      httpsHSTSStatus({
        ...hstsBody({ state: "off_clearing", clearing: true }),
        ...hstsLock("off", "on"),
      });
    await ready();
    expect(switchControl().getAttribute("aria-checked")).toBe("false");
    expect(document.getElementById("hsts-lock")?.textContent).toContain(
      "Stored value: on, used if TIERPLAN_HSTS is removed.",
    );
    // The draft helper compares with the stored choice, which a lock leaves alone.
    expect(card().queryByText(/told to forget HSTS \(max-age=0\)\.$/)).toBeNull();
  });

  it("a 409 hsts_locked on save redraws the Switch locked from the refetched status, with no toast", async () => {
    let refused = false;
    status = () =>
      refused
        ? httpsHSTSStatus({ ...hstsSending(1), ...hstsLock("on", "off") })
        : httpsHSTSStatus();
    putAnswer = () => {
      refused = true;
      return Response.json(
        { type: "hsts_locked", title: "x", status: 409, detail: "d" },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    };
    await ready();
    fireEvent.click(switchControl());
    fireEvent.click(saveButton() as HTMLElement);
    await waitFor(() => expect(document.getElementById("hsts-lock")).not.toBeNull());
    expect(switchControl().hasAttribute("disabled")).toBe(true);
    expect(switchControl().getAttribute("aria-checked")).toBe("true");
    expect(saveButton()).toBeNull();
    expect(toasts()).toEqual([]);
    expect(card().queryByText("Something went wrong. Try again.")).toBeNull();
  });

  it("without settings.write a lock still shows as text", async () => {
    me = viewer;
    status = () => httpsHSTSStatus({ ...hstsSending(1), ...hstsLock("on", "off") });
    renderCard();
    await card().findByText("Sending");
    await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
    expect(document.getElementById("hsts-lock")?.textContent).toContain("TIERPLAN_HSTS");
  });
});

describe("the env-lock notice counts the HSTS lock (SET-02, D-17)", () => {
  it("hasEnvLocks is true for an HSTS lock alone, and false with no lock anywhere", () => {
    expect(hasEnvLocks(httpsHSTSStatus({ ...hstsSending(1), ...hstsLock("on") }))).toBe(true);
    expect(hasEnvLocks(httpsHSTSStatus(hstsSending(1)))).toBe(false);
    expect(hasEnvLocks(httpsHSTSStatus())).toBe(false);
  });
});
