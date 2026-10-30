// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronDown } from "lucide-react";

import { CopyableCode } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { useT } from "@/i18n/use-t";

// The command rotates the key offline: the data directory is locked while Coaming
// runs, so a `docker exec` into the running container would be refused. <volume>
// is the volume the Coaming container mounts at /data.
const ROTATE_COMMAND = "docker run --rm -v <volume>:/data ghcr.io/ianglennon/coaming key rotate";

// MasterKeyNote points to key rotation, which is a command-line task. It is
// collapsed by default: most visits to this page are for a backup.
export function MasterKeyNote() {
  const t = useT();
  return (
    <Collapsible className="flex min-w-0 flex-col gap-2">
      <CollapsibleTrigger asChild>
        <Button type="button" variant="ghost" size="sm" className="group -ms-2 w-fit max-md:h-11">
          <ChevronDown
            aria-hidden="true"
            className="transition-transform group-data-[state=open]:rotate-180"
          />
          {t("backup.masterKey.trigger")}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="flex min-w-0 flex-col gap-3">
        <p className="text-sm">{t("backup.masterKey.body")}</p>
        <CopyableCode code={ROTATE_COMMAND} label={t("backup.masterKey.codeLabel")} />
        <p className="text-sm text-muted-foreground">{t("backup.masterKey.note")}</p>
      </CollapsibleContent>
    </Collapsible>
  );
}
