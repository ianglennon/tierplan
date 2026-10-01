// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

interface CopyableCodeProps {
  code: string;
  label: string;
}

// Stub: the behaviour arrives in the next commit (RED first).
export function CopyableCode({ code, label }: CopyableCodeProps) {
  const t = useT();
  return (
    <div>
      <pre>
        <code>{code}</code>
      </pre>
      <Button type="button" aria-label={t("env.copy.button", { label })}>
        <Copy aria-hidden="true" />
      </Button>
    </div>
  );
}
