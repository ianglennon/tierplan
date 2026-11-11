// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { TriangleAlert } from "lucide-react";

import type { HTTPSACME } from "@/api/https";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { errorCopy, formatDate } from "@/features/https/copy";
import { rich, Sentence, SLOT } from "@/features/https/sentence";
import { Rich } from "@/features/https/state-alerts";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";

type LeftOver = NonNullable<HTTPSACME["dns"]["leftOver"]>[number];

// The label the DNS-01 solver puts in front of the certificate name in the record it writes.
const CHALLENGE_LABEL = "_acme-challenge.";

// When the next try is, as a clock time, with the date in front when it is not today. A next try is
// always ahead, which RelativeTime has no words for, and the retry schedule reaches a day.
function nextTryText(iso: string): string {
  const clock = formatClock(iso);
  if (!clock) return "";
  return new Date(iso).toDateString() === new Date().toDateString()
    ? clock
    : `${formatDate(iso)} ${clock}`;
}

// RetryLine is when a left-over record was last tried and when it is tried next, or why retries are
// paused (D-23): a restore waits for the admin to resume Let's Encrypt, certificate files for the
// operator to unset them. A pause replaces the next-try sentence.
function RetryLine({ item }: { item: LeftOver }) {
  const t = useT();
  const next = item.nextTryAt ? nextTryText(item.nextTryAt) : "";
  let after = null;
  if (item.retryPaused) {
    if (item.pausedReason === "restore") after = <Rich text={t("https.leftover.pausedRestore")} />;
    else if (item.pausedReason === "cert_files")
      after = <Rich text={t("https.leftover.pausedCertFiles")} />;
  } else if (item.nextTryAt && next) {
    after = (
      <Sentence
        text={t("https.leftover.next", { time: SLOT })}
        slot={<time dateTime={item.nextTryAt}>{next}</time>}
      />
    );
  }
  if (!item.lastTryAt && !after) return null;
  return (
    <span className="block text-xs text-muted-foreground">
      {item.lastTryAt ? (
        <Sentence
          text={t("https.leftover.tried", { when: SLOT })}
          slot={<RelativeTime value={item.lastTryAt} />}
        />
      ) : null}
      {item.lastTryAt && after ? " " : null}
      {after}
    </span>
  );
}

// LeftOverRecords lists the TXT records TierPlan wrote at the DNS provider and could not delete yet
// (CHAL-04, D-18). It is a note at the top of the Run and actions section, shown in every card state and
// under either saved method whenever the status lists a record, and it has no action: retries are
// automatic (D-17) and TierPlan only ever deletes the record ID it stored (D-16), so the page cannot be
// asked to remove a record by its name. The records come in the order the status gives them, oldest
// first, with no cap and no collapse. Every name, ID and reason is the server's, drawn as text.
export function LeftOverRecords({ dns }: { dns: HTTPSACME["dns"] }) {
  const t = useT();
  const records = dns.leftOver ?? [];
  if (records.length === 0) return null;
  return (
    <Alert variant="warning" role="note">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("https.leftover.title", { count: records.length })}</AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <span className="block">{t("https.leftover.intro")}</span>
        <ul className="flex flex-col gap-3">
          {records.map((item) => {
            const host = item.name.startsWith(CHALLENGE_LABEL)
              ? item.name.slice(CHALLENGE_LABEL.length)
              : item.name;
            return (
              <li key={`${item.recordId}:${item.name}`} className="min-w-0 break-words">
                <span className="block text-sm text-foreground">
                  {rich(t, "https.leftover.item", { name: item.name, id: item.recordId })}
                </span>
                {item.code ? (
                  <span className="block text-xs text-muted-foreground">
                    {
                      errorCopy(t, item.code, {
                        host,
                        zone: host,
                        target: "",
                        resolvers: t("https.error.resolversDefault"),
                      }).headline
                    }
                  </span>
                ) : null}
                <RetryLine item={item} />
              </li>
            );
          })}
        </ul>
        <span className="block text-xs">
          {rich(t, "https.leftover.footer", { prefix: "tierplan:" })}
        </span>
      </AlertDescription>
    </Alert>
  );
}
