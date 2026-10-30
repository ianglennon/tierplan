// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Test-only page for e2e/copy (G-02-1a), never part of the app build. It
// renders the real CopyableCode plain, inside the real Dialog and inside the
// real AlertDialog, chosen by ?case=, so a real browser on a non-secure origin
// can press Copy and read the clipboard back.

import { createRoot } from "react-dom/client";

import "@/index.css";
import "@/i18n";
import { CopyableCode } from "@/components/copyable-code";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Dialog, DialogContent, DialogDescription, DialogTitle } from "@/components/ui/dialog";
import { Toaster } from "@/components/ui/sonner";

const TOKEN = "bpjt_harness_S3CRET";
const COMMAND = ` docker run -d --name coaming-agent -e COAMING_AGENT_JOIN_TOKEN='${TOKEN}' -e COAMING_AGENT_URL='wss://coaming.test/agent/connect' ghcr.io/ianglennon/coaming-agent:edge`;

const code = <CopyableCode code={COMMAND} secret={TOKEN} label="the install command" />;
const chosen = new URLSearchParams(window.location.search).get("case") ?? "plain";

function Case() {
  if (chosen === "dialog") {
    return (
      <Dialog open>
        <DialogContent>
          <DialogTitle>Install the agent</DialogTitle>
          <DialogDescription>Run this on the host.</DialogDescription>
          {code}
        </DialogContent>
      </Dialog>
    );
  }
  if (chosen === "alertdialog") {
    return (
      <AlertDialog open>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Remove environment</AlertDialogTitle>
            <AlertDialogDescription>Clean up by hand.</AlertDialogDescription>
          </AlertDialogHeader>
          {code}
          <AlertDialogFooter>
            <AlertDialogCancel>Close</AlertDialogCancel>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    );
  }
  return <main className="p-4">{code}</main>;
}

const root = document.getElementById("root");
if (!root) throw new Error("missing #root element");

createRoot(root).render(
  <>
    <Case />
    <Toaster />
  </>,
);
