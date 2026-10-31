// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { CopyableCode } from "@/components/copyable-code";
import { useT } from "@/i18n/use-t";

interface CommandBlockProps {
  // What the command is for, in the words the guidance uses: "Run this on the first manager". It is
  // shown above the block and completes the Copy button's name.
  caption: string;
  command: string;
}

// CommandBlock is text to copy and nothing more (D-23). The block scrolls sideways rather than
// wrapping, so a flag is never broken in the middle of a token; the button copies the text as shown.
// Nothing here runs a command, sends it to the server or puts it in an address.
export function CommandBlock({ caption, command }: CommandBlockProps) {
  const t = useT();
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <p className="text-sm font-medium">{caption}</p>
      <CopyableCode code={command} label={t("poolGuide.command.label", { caption })} />
    </div>
  );
}
