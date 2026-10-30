// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { eligibleOwnersQuery, type OwnedItem, userOwnedQuery } from "@/api/owned";
import { deleteUser, type Reassignment } from "@/api/users";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { ownedName } from "@/features/access/owned-name";
import { ownerLabel } from "@/features/access/owner-label";
import { Truncated } from "@/features/access/truncated";
import { useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The Select value of "Leave paused"; no user has this ID.
const PAUSED = "__paused__";

const keyOf = (item: Pick<OwnedItem, "kind" | "resourceId">) => `${item.kind}/${item.resourceId}`;

interface OwnedRowProps {
  item: OwnedItem;
  // The ID of the user chosen to take it over, or absent to leave it paused.
  owner: string | undefined;
  onChange: (owner: string | undefined) => void;
  disabled: boolean;
}

// OwnedRow is one automation of the user being deleted: its name, type and
// environment, and an Owner select of "Leave paused" (the default) and each user
// who could run it now. If the list is read again and the chosen user is no longer
// on it, the choice falls back to Leave paused rather than being sent.
function OwnedRow({ item, owner, onChange, disabled }: OwnedRowProps) {
  const t = useT();
  const query = useQuery(eligibleOwnersQuery(item.kind, item.resourceId));
  const candidates = query.data ?? [];
  const known = query.data;

  useEffect(() => {
    if (owner && known && !known.some((u) => u.id === owner)) onChange(undefined);
  }, [owner, known, onChange]);

  return (
    <li className="flex min-w-0 flex-col gap-2 py-2">
      <div className="flex min-w-0 flex-col gap-0.5">
        <Truncated text={ownedName(t, item)} className="max-w-full font-medium" />
        <p className="text-xs break-words text-muted-foreground">
          {item.label} · {item.envName}
        </p>
      </div>
      <div className="flex min-w-0 flex-col gap-1">
        <Label htmlFor={`delete-owner-${keyOf(item)}`} className="text-xs">
          {t("owned.delete.owner")}
        </Label>
        <Select
          value={owner ?? PAUSED}
          onValueChange={(v) => onChange(v === PAUSED ? undefined : v)}
          disabled={disabled || query.isPending}
        >
          <SelectTrigger
            id={`delete-owner-${keyOf(item)}`}
            aria-label={t("owned.delete.ownerLabel", { automation: ownedName(t, item) })}
            className={`${TOUCH_HEIGHT} w-full`}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent className="max-h-72">
            <SelectItem value={PAUSED}>{t("owned.delete.leavePaused")}</SelectItem>
            {candidates.map((u) => (
              <SelectItem key={u.id} value={u.id}>
                {ownerLabel(u)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {query.isError ? (
          <p role="alert" className="text-xs text-destructive">
            {t("owned.delete.eligibleFailed", { automation: ownedName(t, item) })}
          </p>
        ) : null}
      </div>
    </li>
  );
}

interface DeleteUserDialogProps {
  // The user to delete, or null while the dialog is closed. lastInstanceAdmin
  // says the server would refuse, so the button is unavailable with the reason.
  user: { id: string; username: string; lastInstanceAdmin?: boolean } | null;
  onClose: () => void;
  // Called once the user is deleted.
  onDone: () => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// DeleteUserDialog is "Delete {username}?" (D-22c, D-18). Deleting is permanent,
// so the username must be typed before the button works. Focus starts on Cancel.
// It cannot be dismissed while the request is in flight, and a refusal stays
// inside the dialog.
export function DeleteUserDialog({ user, onClose, onDone, onStale }: DeleteUserDialogProps) {
  const t = useT();
  const { data: me } = useMe();
  const queryClient = useQueryClient();
  const owned = useQuery({ ...userOwnedQuery(user?.id ?? ""), enabled: user !== null });
  const items = owned.data ?? [];
  // Who takes over which automation, by kind/resource; absent means leave it paused.
  const [owners, setOwners] = useState<Record<string, string | undefined>>({});
  const [typed, setTyped] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const username = user?.username ?? "";
  const matched = matchesName(typed, username);
  const isSelf = user !== null && me?.user.id === user.id;

  async function remove() {
    if (!user || !matched || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const reassign: Reassignment[] = items.flatMap((item) => {
        const ownerId = owners[keyOf(item)];
        return ownerId ? [{ kind: item.kind, resourceId: item.resourceId, ownerId }] : [];
      });
      const result = await deleteUser(user.id, reassign);
      if (result.kind === "ok") {
        toastSuccess(t("userpage.delete.done", { username }));
        setTyped("");
        setOwners({});
        onDone();
        return;
      }
      if (result.kind === "last_instance_admin") setError(t("userpage.errors.lastAdmin"));
      else if (result.kind === "not_found") setError(t("userpage.errors.gone", { username }));
      else if (result.kind === "not_eligible") {
        setError(t("owned.delete.reassignFailed"));
        // Who could take over has changed: read each list again.
        void queryClient.invalidateQueries({ queryKey: ["owned", "eligible"] });
      } else if (result.kind === "automation_gone") {
        setError(t("owned.delete.gone", { username }));
        setOwners({});
        void queryClient.invalidateQueries({ queryKey: userOwnedQuery(user.id).queryKey });
      } else setError(t("userpage.delete.failed", { username }));
      onStale();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(open: boolean) {
    if (open || pending) return;
    setError(null);
    setTyped("");
    setOwners({});
    onClose();
  }

  const confirm = (
    <Button
      type="button"
      aria-disabled={!matched || pending}
      className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
      onClick={() => void remove()}
    >
      {pending ? (
        <>
          <Spinner aria-hidden="true" />
          {t("userpage.delete.pending")}
        </>
      ) : (
        t("userpage.delete.confirm")
      )}
    </Button>
  );

  return (
    <AlertDialog open={user !== null} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("userpage.delete.title", { username })}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {t("userpage.delete.body", { username })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {isSelf ? (
          <Alert>
            <AlertDescription>{t("userpage.delete.ownAccount")}</AlertDescription>
          </Alert>
        ) : null}
        {items.length > 0 ? (
          <section className="flex min-w-0 flex-col gap-2">
            <h3 className="text-sm font-semibold break-words">
              {t("owned.delete.heading", { username })}
            </h3>
            <ul
              aria-label={t("owned.delete.list", { username })}
              className="flex max-h-60 flex-col divide-y overflow-y-auto"
            >
              {items.map((item) => (
                <OwnedRow
                  key={keyOf(item)}
                  item={item}
                  owner={owners[keyOf(item)]}
                  onChange={(owner) => setOwners((prev) => ({ ...prev, [keyOf(item)]: owner }))}
                  disabled={pending}
                />
              ))}
            </ul>
            <p className="text-xs text-muted-foreground">{t("owned.delete.helper")}</p>
          </section>
        ) : null}
        <TypeToConfirm
          name={username}
          value={typed}
          onChange={setTyped}
          onConfirm={() => void remove()}
          disabled={pending}
        />
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("userpage.delete.cancel")}
          </AlertDialogCancel>
          <NeedsTooltip reason={user?.lastInstanceAdmin ? t("needs.lastAdmin") : undefined}>
            {confirm}
          </NeedsTooltip>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
