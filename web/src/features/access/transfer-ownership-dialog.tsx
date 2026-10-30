// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import { eligibleOwnersQuery, type OwnedItem, transferOwned } from "@/api/owned";
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
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { ownedName } from "@/features/access/owned-name";
import { ownerLabel } from "@/features/access/owner-label";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface TransferFormProps {
  item: OwnedItem;
  onClose: () => void;
  onDone: (item: OwnedItem) => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
  // Tells the dialog a request is in flight, so it refuses to close.
  onPendingChange: (pending: boolean) => void;
}

function TransferForm({ item, onClose, onDone, onStale, onPendingChange }: TransferFormProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const query = useQuery(eligibleOwnersQuery(item.kind, item.resourceId));
  const [owner, setOwner] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const candidates = query.data ?? [];
  const none = query.isSuccess && candidates.length === 0;
  const chosen = candidates.find((u) => u.id === owner);
  const ready = chosen !== undefined && !pending;

  async function transfer() {
    if (!chosen || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    onPendingChange(true);
    setError(null);
    try {
      const result = await transferOwned(item.kind, item.resourceId, chosen.id);
      if (result.kind === "ok") {
        toastSuccess(
          t("owned.transfer.done", { automation: ownedName(t, item), username: chosen.username }),
        );
        onDone(result.item);
        return;
      }
      if (result.kind === "not_eligible") {
        setError(t("owned.transfer.errors.notEligible"));
        // Who could take it over has changed: read the list again.
        setOwner("");
        void queryClient.invalidateQueries({
          queryKey: eligibleOwnersQuery(item.kind, item.resourceId).queryKey,
        });
      } else if (result.kind === "not_found") {
        setError(t("owned.transfer.errors.gone", { automation: ownedName(t, item) }));
        onStale();
      } else {
        setError(t("owned.transfer.errors.generic", { automation: ownedName(t, item) }));
      }
    } finally {
      inFlight.current = false;
      setPending(false);
      onPendingChange(false);
    }
  }

  return (
    <>
      <DialogHeader>
        {/* A long name wraps rather than running off the dialog. */}
        <DialogTitle className="break-words">
          {t("owned.transfer.title", { automation: ownedName(t, item) })}
        </DialogTitle>
        <DialogDescription className="break-words">{t("owned.transfer.body")}</DialogDescription>
      </DialogHeader>
      <form
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          void transfer();
        }}
        className="flex min-w-0 flex-col gap-4"
      >
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription className="break-words">{error}</AlertDescription>
          </Alert>
        ) : null}
        {query.isPending ? (
          <div aria-busy="true">
            <span role="status" className="sr-only">
              {t("owned.transfer.loading")}
            </span>
            <Skeleton className="h-11 w-full md:h-8" aria-hidden="true" />
          </div>
        ) : query.isError ? (
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span>{t("owned.transfer.loadFailed")}</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={query.isFetching}
                onClick={() => void query.refetch()}
              >
                {query.isFetching ? <Spinner aria-hidden="true" /> : null}
                {t("owned.transfer.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : none ? (
          <Alert role="status">
            <AlertDescription className="break-words">
              {t("owned.transfer.none", { environment: item.envName })}
            </AlertDescription>
          </Alert>
        ) : (
          <div className="flex flex-col gap-2">
            <Label htmlFor="transfer-owner">{t("owned.transfer.owner")}</Label>
            <Select value={owner} onValueChange={setOwner} disabled={pending}>
              <SelectTrigger id="transfer-owner" className={`${TOUCH_HEIGHT} w-full`}>
                <SelectValue placeholder={t("owned.transfer.placeholder")} />
              </SelectTrigger>
              {/* The list scrolls inside the viewport however many users there are. */}
              <SelectContent className="max-h-72">
                {candidates.map((u) => (
                  <SelectItem key={u.id} value={u.id}>
                    {ownerLabel(u)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">{t("owned.transfer.helper")}</p>
          </div>
        )}
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            className={TOUCH_HEIGHT}
            disabled={pending}
            onClick={onClose}
          >
            {t("owned.transfer.cancel")}
          </Button>
          <Button
            type="submit"
            aria-disabled={!ready}
            className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={(e) => {
              if (!ready) e.preventDefault();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("owned.transfer.pending")}
              </>
            ) : (
              t("owned.transfer.confirm")
            )}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}

interface TransferOwnershipDialogProps {
  // The automation to hand over, or null while the dialog is closed.
  item: OwnedItem | null;
  onClose: () => void;
  // Called with the automation as it now is, once it has a new owner.
  onDone: (item: OwnedItem) => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// TransferOwnershipDialog is "Transfer {automation}" (D-14, UI-SPEC "Owned
// automations"): a Select of the users who could run it right now, and a
// confirm that stays unavailable until one is chosen. With nobody eligible an
// Alert names what to do instead of the Select. The server checks eligibility
// again, and a refusal stays inside the dialog. The form starts afresh each time
// it opens, and the dialog cannot be dismissed while the request is in flight.
export function TransferOwnershipDialog({
  item,
  onClose,
  onDone,
  onStale,
}: TransferOwnershipDialogProps) {
  const [pending, setPending] = useState(false);
  return (
    <Dialog
      open={item !== null}
      onOpenChange={(open) => {
        if (!open && !pending) onClose();
      }}
    >
      <DialogContent showCloseButton={false} className="max-h-[90svh] overflow-y-auto">
        {item ? (
          <TransferForm
            item={item}
            onClose={onClose}
            onDone={onDone}
            onStale={onStale}
            onPendingChange={setPending}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
