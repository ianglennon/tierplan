// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CircleX, Info, TriangleAlert } from "lucide-react";
import { Fragment, type ReactNode } from "react";

import { ACMEControlError, type HTTPSStatus, useResumeACME } from "@/api/https";
import { Alert, AlertAction, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { deriveCardState, deriveEnvOverlay } from "@/features/https/card-state";
import { formatDate } from "@/features/https/copy";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Marks where a value goes inside a translated sentence. i18next puts these in as the values, and
// Rich swaps each for its node, so a long host or a list of names is drawn as text in mono, never as
// markup.
const FIRST = "\u0001";
const SECOND = "\u0002";

const PART = new RegExp(`(TIERPLAN_[A-Z_]+|${FIRST}|${SECOND})`);

// Rich draws a translated sentence with the environment variable names in mono and the two value
// markers replaced by the nodes given.
export function Rich({
  text,
  first,
  second,
}: {
  text: string;
  first?: ReactNode;
  second?: ReactNode;
}) {
  return (
    <>
      {text.split(PART).map((part, i) => {
        const key = `${i}:${part}`;
        if (part === FIRST) return <Fragment key={key}>{first}</Fragment>;
        if (part === SECOND) return <Fragment key={key}>{second}</Fragment>;
        if (part.startsWith("TIERPLAN_")) {
          return (
            <code key={key} className="font-mono text-xs">
              {part}
            </code>
          );
        }
        return <Fragment key={key}>{part}</Fragment>;
      })}
    </>
  );
}

// Mono is one value from a setting or a certificate, drawn as text and broken anywhere so a long name
// cannot widen the page.
function Mono({ children }: { children: ReactNode }) {
  return <code className="break-all font-mono text-xs">{children}</code>;
}

function MonoList({ values }: { values: string[] }) {
  return (
    <>
      {values.map((value, i) => (
        <Fragment key={value}>
          {i > 0 ? ", " : null}
          <Mono>{value}</Mono>
        </Fragment>
      ))}
    </>
  );
}

// TestDirectoryAlert says TierPlan talks to a test certificate authority (D-19). The page draws it
// under its description whatever else is shown. The address is the operator's own setting, shown as
// text and never as a link.
export function TestDirectoryAlert({ url }: { url: string }) {
  const t = useT();
  return (
    <Alert variant="warning" role="note">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("https.alerts.testDirectory.title")}</AlertTitle>
      <AlertDescription>
        <Rich
          text={t("https.alerts.testDirectory.body", { url: FIRST })}
          first={<Mono>{url}</Mono>}
        />
      </AlertDescription>
    </Alert>
  );
}

interface StateAlertsProps {
  status: HTTPSStatus;
  canWrite: boolean;
}

// StateAlerts is the persistent explanation of an unusual state of the Let's Encrypt card (UI-SPEC
// "State alerts"): certificate files, a pause after a restore, changed names, an expired certificate.
// Which one shows follows deriveCardState, so no alert works out a state of its own. They are notes,
// not announcements: they are there when the page loads, not news.
export function StateAlerts({ status, canWrite }: StateAlertsProps) {
  const t = useT();
  const acme = status.acme;
  if (!acme) return null;
  const overlay = deriveEnvOverlay(status);
  const envOn = overlay.env === "on";
  switch (deriveCardState(status)) {
    case "I":
      return (
        <Alert role="note" className="text-foreground">
          <Info aria-hidden="true" />
          <AlertDescription className="text-foreground">
            <Rich text={t("https.alerts.certFilesOnly")} />
            {envOn ? <Rich text={` ${t("https.envState.certFiles")}`} /> : null}
          </AlertDescription>
        </Alert>
      );
    case "J":
      return (
        <Alert variant="warning" role="note">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>{t("https.alerts.certFilesKept.title")}</AlertTitle>
          <AlertDescription>
            <Rich text={t("https.alerts.certFilesKept.body")} />
            {envOn ? <Rich text={` ${t("https.envState.certFiles")}`} /> : null}
          </AlertDescription>
        </Alert>
      );
    case "H":
      return <RestoreAlert canWrite={canWrite} envOn={envOn} />;
    case "E":
      return (
        <Alert variant="warning" role="note">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>{t("https.alerts.namesChanged.title")}</AlertTitle>
          <AlertDescription>
            <Rich
              text={`${t("https.alerts.namesChanged.lead", { old: FIRST, new: SECOND })} ${t(
                envOn ? "https.envState.namesChanged" : "https.alerts.namesChanged.tail",
              )}`}
              first={<MonoList values={acme.certificate?.names ?? []} />}
              second={<MonoList values={(acme.names ?? []).map((n) => n.name)} />}
            />
          </AlertDescription>
        </Alert>
      );
    case "K":
      return (
        <Alert variant="destructive" role="note">
          <CircleX aria-hidden="true" />
          <AlertTitle>{t("https.alerts.fallback.title")}</AlertTitle>
          <AlertDescription>
            {t("https.alerts.fallback.body", { date: formatDate(acme.fallback?.expiredAt) })}
          </AlertDescription>
        </Alert>
      );
    default:
      if (overlay.waitingForToken) return <WaitingForTokenAlert />;
      if (overlay.waiting) return <WaitingAlert />;
      if (overlay.env === "off") return <OffAlert kept={overlay.offWithState} />;
      return null;
  }
}

// OffAlert says TIERPLAN_ACME=off: nothing is ordered and, when there is something, what is kept
// (D-02, D-04). With nothing stored it is one sentence.
function OffAlert({ kept }: { kept: boolean }) {
  const t = useT();
  if (!kept) {
    return (
      <Alert role="note" className="text-foreground">
        <Info aria-hidden="true" />
        <AlertDescription className="text-foreground">
          <Rich text={t("https.envState.offNothing")} />
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <Alert variant="warning" role="note">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>
        <Rich text={t("https.envState.off.title")} />
      </AlertTitle>
      <AlertDescription>
        <Rich text={t("https.envState.off.body")} />
      </AlertDescription>
    </Alert>
  );
}

// WaitingAlert says TierPlan orders the certificate by itself because TIERPLAN_ACME is on (D-07): no
// certificate yet, nothing running, refused or failed. It sits above the buttons and asks for nothing.
function WaitingAlert() {
  const t = useT();
  return (
    <Alert role="note" className="text-foreground">
      <Info aria-hidden="true" />
      <AlertTitle>{t("https.envState.waiting.title")}</AlertTitle>
      <AlertDescription className="text-foreground">
        <Rich text={t("https.envState.waiting.body")} />
      </AlertDescription>
    </Alert>
  );
}

// WaitingForTokenAlert says TIERPLAN_ACME is on and DNS-01 is selected but there is no token anywhere
// (D-07). Nothing is attempted, so nothing is charged and no error shows; the alert says what is missing.
function WaitingForTokenAlert() {
  const t = useT();
  return (
    <Alert variant="warning" role="note">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("https.envState.waitingToken.title")}</AlertTitle>
      <AlertDescription>
        <Rich text={t("https.envState.waitingToken.body")} />
      </AlertDescription>
    </Alert>
  );
}

// RestoreAlert is the pause a restore puts on Let's Encrypt (D-20), with the one filled button of the
// card: Resume. Without settings.write the alert is only an explanation.
function RestoreAlert({ canWrite, envOn }: { canWrite: boolean; envOn: boolean }) {
  const t = useT();
  const resume = useResumeACME();

  async function press() {
    if (resume.isPending) return;
    try {
      await resume.mutateAsync();
      toastSuccess(t("https.alerts.restore.resumed"));
    } catch (error) {
      // A 409 means the page was out of date: the status was read again and says what the card is now.
      if (!(error instanceof ACMEControlError && error.status === 409)) {
        toastError(t("common.mutationFailed"));
      }
    }
  }

  return (
    <Alert variant="warning" role="note" className="has-data-[slot=alert-action]:pr-2.5">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("https.alerts.restore.title")}</AlertTitle>
      <AlertDescription>
        {t("https.alerts.restore.body")}
        {envOn ? <Rich text={` ${t("https.envState.restore")}`} /> : null}
      </AlertDescription>
      {canWrite ? (
        <AlertAction className="static col-start-2 mt-2">
          <Button
            type="button"
            aria-disabled={resume.isPending}
            className={`${TOUCH_HEIGHT} max-md:w-full aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void press()}
          >
            {resume.isPending ? (
              <>
                <Spinner aria-hidden="true" role="presentation" />
                {t("https.alerts.restore.resuming")}
              </>
            ) : (
              t("https.alerts.restore.resume")
            )}
          </Button>
        </AlertAction>
      ) : null}
    </Alert>
  );
}
