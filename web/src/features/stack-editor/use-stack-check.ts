// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { parseAllDocuments } from "yaml";

import { postStackCheck, type StackProblem, type StackVariable } from "@/api/stacks";
import { COMPOSE_FILE, toSaveBody, type WorkingCopy } from "@/features/stack-editor/working-copy";
import i18n from "@/i18n";

// How long after the last keystroke each check runs (UI-SPEC "Lint and inline errors"): the
// browser reads the YAML itself first, and the server checks the whole working copy a little later.
export const stackCheckTiming = { clientMs: 400, serverMs: 800 };

// What the library says without the place it says it at, which the problem states itself.
const yamlMessage = (message: string) =>
  (message.split("\n")[0] ?? "").replace(/ at line \d+, column \d+:?$/, "").trim();

// yamlProblems reads the YAML of one file in the browser and returns its syntax errors with their
// lines. It is the early answer: the server's check replaces it for the same line.
export function yamlProblems(text: string, file: string = COMPOSE_FILE): StackProblem[] {
  const out: StackProblem[] = [];
  let docs: ReturnType<typeof parseAllDocuments>;
  try {
    docs = parseAllDocuments(text);
  } catch {
    return out;
  }
  for (const doc of Array.isArray(docs) ? docs : [docs]) {
    for (const err of doc.errors) {
      const start = err.linePos?.[0];
      const end = err.linePos?.[1] ?? start;
      const line = start?.line ?? 1;
      const col = start?.col ?? 1;
      out.push({
        severity: "error",
        code: "yaml_invalid",
        message: String(
          i18n.t("stackProblems.yaml.line", {
            ns: "stackProblems",
            line,
            message: yamlMessage(err.message),
          }),
        ),
        file,
        line,
        col,
        endLine: end?.line ?? line,
        endCol: end?.col ?? col + 1,
      });
    }
  }
  return out;
}

function sortProblems(list: StackProblem[]): StackProblem[] {
  return [...list].sort(
    (a, b) => (a.file < b.file ? -1 : a.file > b.file ? 1 : 0) || a.line - b.line || a.col - b.col,
  );
}

// mergeProblems puts the browser's problems beside the server's: a problem of the server's
// replaces the browser's on the same file and line, and the rest of the browser's stay until the
// server has looked at the same text.
export function mergeProblems(server: StackProblem[], client: StackProblem[]): StackProblem[] {
  const seen = new Set(server.map((p) => `${p.file}:${p.line}`));
  return sortProblems([...server, ...client.filter((p) => !seen.has(`${p.file}:${p.line}`))]);
}

interface StackCheckState {
  // Every problem of the working copy, the server's merged with the browser's.
  problems: StackProblem[];
  // The stack files the compose file never refers to.
  unusedFiles: string[];
  // The rows of the Variables pane as the server resolved them, or null before its first answer.
  variables: StackVariable[] | null;
  // The stack files that are secret: the ones the compose file uses under secrets:, which are
  // known before a save has sealed them, and the ones already saved as secret.
  secretFiles: string[];
  // A check is on its way to the server.
  checking: boolean;
  // The last check got no answer. The last results stay.
  failed: boolean;
  // True once the server has answered at least once.
  checked: boolean;
  // Check now, without waiting for the person to stop typing (after a save).
  runNow: () => void;
}

// useStackCheck checks the working copy while the person types (STK-01, D-10): the browser reads
// the compose file's YAML 400 ms after the last edit, the server checks the whole working copy
// 800 ms after it. A check is never retried; the next edit sends the next one. When a check fails
// the last results stay, and `failed` says so. The first check of a loaded copy runs at once.
export function useStackCheck(envId: string, stackId: string, copy: WorkingCopy): StackCheckState {
  const [server, setServer] = useState<{
    problems: StackProblem[];
    unusedFiles: string[];
    variables: StackVariable[];
    secretFiles: string[];
  } | null>(null);
  const [client, setClient] = useState<StackProblem[]>([]);
  const [checking, setChecking] = useState(false);
  const [failed, setFailed] = useState(false);
  const latest = useRef(0);
  const copyRef = useRef(copy);
  copyRef.current = copy;
  const first = useRef(true);
  const serverTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const run = useCallback(() => {
    const current = copyRef.current;
    if (!current.loaded) return;
    clearTimeout(serverTimer.current);
    const mine = ++latest.current;
    const body = toSaveBody(current, 0);
    setChecking(true);
    postStackCheck(envId, stackId, { files: body.files, vars: body.vars }).then(
      (answer) => {
        if (mine !== latest.current) return;
        setServer(answer);
        setFailed(false);
        setChecking(false);
      },
      () => {
        if (mine !== latest.current) return;
        setFailed(true);
        setChecking(false);
      },
    );
  }, [envId, stackId]);

  const { loaded, files, vars } = copy;
  // biome-ignore lint/correctness/useExhaustiveDependencies: a check runs when the files or variables change, and reads the copy through its ref
  useEffect(() => {
    if (!loaded) return;
    const compose = copyRef.current.files.find((f) => f.path === COMPOSE_FILE);
    const readClient = () =>
      setClient(compose && !compose.hidden ? yamlProblems(compose.content) : []);
    if (first.current) {
      first.current = false;
      readClient();
      run();
      return;
    }
    const clientTimer = setTimeout(readClient, stackCheckTiming.clientMs);
    clearTimeout(serverTimer.current);
    serverTimer.current = setTimeout(run, stackCheckTiming.serverMs);
    return () => {
      clearTimeout(clientTimer);
      clearTimeout(serverTimer.current);
    };
  }, [loaded, files, vars, run]);

  // A page that closes sends nothing more.
  useEffect(
    () => () => {
      latest.current++;
      clearTimeout(serverTimer.current);
    },
    [],
  );

  const problems = useMemo(() => mergeProblems(server?.problems ?? [], client), [server, client]);
  return {
    problems,
    unusedFiles: server?.unusedFiles ?? [],
    variables: server?.variables ?? null,
    secretFiles: server?.secretFiles ?? [],
    checking,
    failed,
    checked: server !== null,
    runNow: run,
  };
}
