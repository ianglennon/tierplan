// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, Copy, Info } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { StackGitWebhookSecret } from "@/api/stack-git";
import { copyText } from "@/components/copyable-code";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Label } from "@/components/ui/label";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long a Copy button says "Copied" before it goes back to "Copy".
const COPIED_MS = 2000;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface ValueRowProps {
  // The id of the read-only field; its label points at it.
  id: string;
  label: string;
  value: string;
  // The accessible name of the read-only field, when the visible label is not enough.
  fieldLabel?: string;
  copyLabel: string;
  copiedLabel: string;
}

// CopyValueRow is one value of the panel: a label, the value in a read-only mono input that scrolls inside itself, and a
// Copy button that says what it copied. Nothing here reaches a toast, the URL or storage; a failed copy selects the
// text for a hand copy and says so without repeating it.
export function CopyValueRow({
  id,
  label,
  value,
  fieldLabel,
  copyLabel,
  copiedLabel,
}: ValueRowProps) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const group = useRef<HTMLDivElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  async function copy() {
    clearTimeout(timer.current);
    if (await copyText(value, group.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    setCopied(false);
    input.current?.select();
    toastError(t("env.copy.failed"));
  }

  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <InputGroup ref={group} className="h-11 md:h-8">
        <InputGroupInput
          ref={input}
          id={id}
          readOnly
          value={value}
          aria-label={fieldLabel}
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          className="h-full font-mono text-xs break-all"
          onFocus={(e) => e.currentTarget.select()}
        />
        <InputGroupAddon align="inline-end">
          <InputGroupButton
            size="icon-sm"
            className="max-md:size-11"
            aria-label={copied ? copiedLabel : copyLabel}
            onClick={() => void copy()}
          >
            {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      <span role="status" className="sr-only">
        {copied ? copiedLabel : ""}
      </span>
    </div>
  );
}

interface ShowOnceSecretProps {
  id: string;
  // What a create or a rotate answered. It is the caller's component state and nothing else keeps it.
  secret: StackGitWebhookSecret;
  // Clears the secret from the caller's state; the caller puts focus back on the card's heading.
  onDone: () => void;
}

// ShowOnceSecret is the one place the full webhook address and the secret appear (UI-SPEC "Git tab", defaults 9 and
// 10): a status Alert shown after a create or a rotate that offers both values to copy and goes with Done. The values
// are props from the caller's state, so they disappear when it clears them, on route change and on unmount. A generic
// webhook with the secret in its address has no Secret row, because the address already is the secret.
export function ShowOnceSecret({ id, secret, onDone }: ShowOnceSecretProps) {
  const t = useT();
  return (
    <Alert role="status" aria-live="polite" aria-labelledby={`${id}-title`}>
      <Info aria-hidden="true" />
      <AlertTitle id={`${id}-title`}>{t("stackGit.webhook.once.title")}</AlertTitle>
      <AlertDescription className="flex min-w-0 flex-col gap-3">
        <p>{t("stackGit.webhook.once.body")}</p>
        <CopyValueRow
          id={`${id}-address`}
          label={t("stackGit.webhook.once.address")}
          value={secret.url}
          copyLabel={t("stackGit.webhook.once.copyAddress")}
          copiedLabel={t("stackGit.webhook.once.copiedAddress")}
        />
        {secret.secret ? (
          <CopyValueRow
            id={`${id}-secret`}
            label={t("stackGit.webhook.once.secret")}
            value={secret.secret}
            fieldLabel={t("stackGit.webhook.once.secretLabel")}
            copyLabel={t("stackGit.webhook.once.copySecret")}
            copiedLabel={t("stackGit.webhook.once.copiedSecret")}
          />
        ) : null}
        <div>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onDone}>
            {t("stackGit.webhook.once.done")}
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}
