// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check } from "lucide-react";
import type { KeyboardEvent } from "react";

import type { TagName } from "@/api/tags";
import { TableCell, TableRow } from "@/components/ui/table";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface TagRowProps {
  tag: TagName;
  variant: "full" | "picker";
  // Picker only: whether this is the tag chosen, and what choosing one does.
  selected?: boolean;
  onSelect?: (tag: string) => void;
}

// TagRow is one tag of the browser. The name is the registry's own text, so it is drawn as
// text, in mono, cut with an ellipsis and whole in a tooltip. In the picker a click or Enter
// chooses the row.
export function TagRow({ tag, variant, selected, onSelect }: TagRowProps) {
  const t = useT();
  const picker = variant === "picker";

  function onKeyDown(e: KeyboardEvent<HTMLTableRowElement>) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect?.(tag.name);
    }
  }

  return (
    <TableRow
      aria-selected={picker ? Boolean(selected) : undefined}
      tabIndex={picker ? 0 : undefined}
      onClick={picker ? () => onSelect?.(tag.name) : undefined}
      onKeyDown={picker ? onKeyDown : undefined}
      className={cn("h-9", picker && "cursor-pointer")}
    >
      <TableCell>
        <span className="flex min-w-0 items-center gap-2">
          <TruncatedText mono>{tag.name}</TruncatedText>
          {picker && selected ? (
            <>
              <Check aria-hidden="true" className="size-4 shrink-0" />
              <span className="sr-only">{t("tags.selected")}</span>
            </>
          ) : null}
        </span>
      </TableCell>
    </TableRow>
  );
}
