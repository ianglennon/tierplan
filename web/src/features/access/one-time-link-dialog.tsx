// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { ReactNode } from "react";
import type { OneTimeLink } from "@/api/users";
import { CopyableCode } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";

const HOUR_MS = 3_600_000;

// fullUrl makes the link something an admin can hand over. Without a public URL
// the server answers an address relative to this Coaming, base path included;
// copied as it is, it would be useless to anyone else.
function fullUrl(url: string): string {
  try {
    return new URL(url, window.location.origin).href;
  } catch {
    return url;
  }
}

// secretOf is the part of the link that must not be on screen unless asked for:
// the token after "#token=", which the browser never sends to a server.
function secretOf(url: string): string {
  const match = /#token=(.+)$/.exec(url);
  if (match?.[1]) return match[1];
  const at = url.indexOf("#");
  return at >= 0 ? url.slice(at + 1) : "";
}

// TITLE_KEY says which heading a link carries: a freshly created user's, a
// regenerated invite's or a password reset's.
const TITLE_KEY = {
  ready: "users.link.readyTitle",
  invite: "users.link.inviteTitle",
  reset: "users.link.resetTitle",
} as const;

export type LinkTitle = keyof typeof TITLE_KEY;

interface OneTimeLinkContentProps {
  title: LinkTitle;
  username: string;
  link: OneTimeLink;
  onClose: () => void;
  // An extra button beside Close, such as "Set up access" for a new user.
  extra?: ReactNode;
}

// OneTimeLinkContent is the body of every one-time link screen: the title, the
// expiry line, the masked link with Copy, the notice and Close. The link lives
// only in the props of whoever shows this, which drops it when the dialog
// closes; nothing here writes it to storage, a URL or a query (UI-SPEC
// "One-time secrets").
export function OneTimeLinkContent({
  title,
  username,
  link,
  onClose,
  extra,
}: OneTimeLinkContentProps) {
  const t = useT();
  const full = fullUrl(link.url);
  const hours = Math.max(0, Math.round((Date.parse(link.expiresAt) - Date.now()) / HOUR_MS));
  return (
    <>
      <DialogHeader>
        <DialogTitle className="break-words">{t(TITLE_KEY[title], { username })}</DialogTitle>
        <DialogDescription>
          {t("users.link.expires", { when: formatAbsolute(link.expiresAt), count: hours })}
        </DialogDescription>
      </DialogHeader>
      <CopyableCode
        code={full}
        label={t("users.link.label")}
        secret={secretOf(full)}
        showLabel={t("users.link.show")}
        hideLabel={t("users.link.hide")}
      />
      <Alert>
        <AlertDescription>{t("users.link.notice", { username })}</AlertDescription>
      </Alert>
      <DialogFooter>
        {extra}
        <Button type="button" variant="outline" className="h-11 md:h-8" onClick={onClose}>
          {t("users.link.close")}
        </Button>
      </DialogFooter>
    </>
  );
}

interface OneTimeLinkDialogProps {
  title: LinkTitle;
  username: string;
  // The link, or null when there is none to show. The caller sets it when one
  // is made and clears it when the dialog closes, so a closed dialog holds no
  // secret.
  link: OneTimeLink | null;
  onClose: () => void;
}

// OneTimeLinkDialog shows a one-time link to the admin who asked for it: a new
// user's invite, a regenerated invite or a password reset. It cannot be shown
// again after it closes.
export function OneTimeLinkDialog({ title, username, link, onClose }: OneTimeLinkDialogProps) {
  return (
    <Dialog open={link !== null} onOpenChange={(open) => (open ? undefined : onClose())}>
      <DialogContent showCloseButton={false}>
        {link ? (
          <OneTimeLinkContent title={title} username={username} link={link} onClose={onClose} />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
