// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, Copy, ShieldCheck, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { GitTestResult } from "@/api/git";
import { copyText } from "@/components/copyable-code";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// What a test found out about the host key of an ssh address.
export type TestedHostKey = GitTestResult["hostKey"];

// How long the Copy button says "Copied".
const COPIED_MS = 2000;

// A fingerprint, selectable text that wraps anywhere (it is one unbroken string), with a Copy
// button whose result is announced politely. The text is rendered as text only (T-09-28).
export function Fingerprint({ value }: { value: string }) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const host = useRef<HTMLSpanElement>(null);
  useEffect(() => () => clearTimeout(timer.current), []);

  async function copy() {
    clearTimeout(timer.current);
    if (await copyText(value, host.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    setCopied(false);
    toastError(t("env.copy.failed"));
  }

  return (
    <span ref={host} className="flex items-start gap-1">
      <code className="min-w-0 flex-1 py-1 font-mono text-xs break-all text-foreground">
        {value}
      </code>
      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        className="shrink-0 max-md:size-11"
        aria-label={t("git.hostKey.confirm.copy")}
        onClick={() => void copy()}
      >
        {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
      </Button>
      <span role="status" className="sr-only">
        {copied ? t("git.hostKey.confirm.copied") : ""}
      </span>
    </span>
  );
}

interface HostKeyConfirmProps {
  // What the test said about the host key.
  hostKey: TestedHostKey;
  // The host the test was made to.
  host: string;
  // True once the person trusted the key the host showed: it is held in the form until Save.
  pending: boolean;
  onTrust: () => void;
  // "Don't trust it": drops the pending key and the prompt.
  onDismiss: () => void;
}

// HostKeyConfirm is what a test of an ssh address says about the machine it talked to (D-03):
//  - a host with no built-in key and no trusted key: the warning Alert that asks for a decision,
//    role=status so it is announced but takes no focus;
//  - the same key once "Trust this key" was pressed: a line saying it is saved with the repository;
//  - a host whose built-in keys matched: a line saying so;
//  - a key a person trusted earlier that matched: Trusted and the start of its fingerprint.
// A changed key is HostKeyChanged: it is a different kind of message.
export function HostKeyConfirm({
  hostKey,
  host,
  pending,
  onTrust,
  onDismiss,
}: HostKeyConfirmProps) {
  const t = useT();
  if (hostKey.state === "builtin") {
    return (
      <p className="flex items-center gap-2 text-xs">
        <ShieldCheck aria-hidden="true" className="size-4 shrink-0 text-status-online-text" />
        {t("git.hostKey.builtin", { host })}
      </p>
    );
  }
  if (pending && (hostKey.state === "untrusted" || hostKey.state === "trusted")) {
    return (
      <p className="flex items-center gap-2 text-xs">
        <ShieldCheck aria-hidden="true" className="size-4 shrink-0 text-status-online-text" />
        {t("git.hostKey.confirmed")}
      </p>
    );
  }
  if (hostKey.state === "trusted") {
    return <TrustedKeyLine fingerprint={hostKey.fingerprint} />;
  }
  if (hostKey.state !== "untrusted") return null;
  return (
    <Alert variant="warning" role="status">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("git.hostKey.confirm.title")}</AlertTitle>
      <AlertDescription>
        <div className="flex flex-col gap-3">
          <p>
            {t("git.hostKey.confirm.body", {
              host,
              type: hostKey.type,
            })}
          </p>
          <div className="flex flex-col gap-1">
            <span className="text-xs text-muted-foreground">
              {t("git.hostKey.confirm.fingerprint")}
            </span>
            <Fingerprint value={hostKey.fingerprint} />
          </div>
          <p className="text-xs">{t("git.hostKey.confirm.helper")}</p>
          <div className="flex flex-wrap gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={onTrust}
            >
              {t("git.hostKey.confirm.trust")}
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="max-md:h-11"
              onClick={onDismiss}
            >
              {t("git.hostKey.confirm.dismiss")}
            </Button>
          </div>
        </div>
      </AlertDescription>
    </Alert>
  );
}

// TrustedKeyLine is "Trusted" and the first 12 characters of the fingerprint of the key a person
// confirmed, the whole of it in a Tooltip (the cell is real text; the Tooltip is a second route).
export function TrustedKeyLine({ fingerprint }: { fingerprint: string }) {
  const t = useT();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <p className="flex w-fit items-center gap-2 text-xs">
          <ShieldCheck aria-hidden="true" className="size-4 shrink-0 text-status-online-text" />
          {t("git.hostKey.trusted")}
          <span className="font-mono">{fingerprint.slice(0, 12)}</span>
        </p>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm font-mono break-all">{fingerprint}</TooltipContent>
    </Tooltip>
  );
}
