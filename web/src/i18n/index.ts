// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import i18n from "i18next";
import { initReactI18next } from "react-i18next";

import actions from "./en/actions.json";
import activeshells from "./en/activeshells.json";
import aggregate from "./en/aggregate.json";
import audit from "./en/audit.json";
import auth from "./en/auth.json";
import backup from "./en/backup.json";
import bell from "./en/bell.json";
import common from "./en/common.json";
import configs from "./en/configs.json";
import containerdetail from "./en/containerdetail.json";
import containers from "./en/containers.json";
import dashboard from "./en/dashboard.json";
import env from "./en/env.json";
import envaccess from "./en/envaccess.json";
import envsettings from "./en/envsettings.json";
import events from "./en/events.json";
import fullpage from "./en/fullpage.json";
import grants from "./en/grants.json";
import images from "./en/images.json";
import install from "./en/install.json";
import invite from "./en/invite.json";
import jobs from "./en/jobs.json";
import logs from "./en/logs.json";
import needs from "./en/needs.json";
import networks from "./en/networks.json";
import noaccess from "./en/noaccess.json";
import nodes from "./en/nodes.json";
import overview from "./en/overview.json";
import owned from "./en/owned.json";
import placement from "./en/placement.json";
import pulls from "./en/pulls.json";
import reason from "./en/reason.json";
import registries from "./en/registries.json";
import restartloop from "./en/restartloop.json";
import restore from "./en/restore.json";
import schedule from "./en/schedule.json";
import secrets from "./en/secrets.json";
import servicedetail from "./en/servicedetail.json";
import serviceform from "./en/serviceform.json";
import services from "./en/services.json";
import sessions from "./en/sessions.json";
import settings from "./en/settings.json";
import setup from "./en/setup.json";
import shell from "./en/shell.json";
import specedit from "./en/specedit.json";
import stackchanges from "./en/stackchanges.json";
import stackconverge from "./en/stackconverge.json";
import stackdeploy from "./en/stackdeploy.json";
import stackeditor from "./en/stackeditor.json";
import stackhistory from "./en/stackhistory.json";
import stackimport from "./en/stackimport.json";
import stacklinks from "./en/stacklinks.json";
import stacklists from "./en/stacklists.json";
import stacklogs from "./en/stacklogs.json";
import stackoverview from "./en/stackoverview.json";
import stackproblems from "./en/stackproblems.json";
import stackpull from "./en/stackpull.json";
import stackremove from "./en/stackremove.json";
import stackrendered from "./en/stackrendered.json";
import stackresources from "./en/stackresources.json";
import stackretention from "./en/stackretention.json";
import stackreview from "./en/stackreview.json";
import stackrevision from "./en/stackrevision.json";
import stackrollback from "./en/stackrollback.json";
import stacks from "./en/stacks.json";
import stackstandalone from "./en/stackstandalone.json";
import stackstop from "./en/stackstop.json";
import stackunmanaged from "./en/stackunmanaged.json";
import stackupdate from "./en/stackupdate.json";
import stackvars from "./en/stackvars.json";
import stackyamldiff from "./en/stackyamldiff.json";
import stats from "./en/stats.json";
import switcher from "./en/switcher.json";
import tags from "./en/tags.json";
import tasks from "./en/tasks.json";
import teams from "./en/teams.json";
import terminal from "./en/terminal.json";
import usedby from "./en/usedby.json";
import userpage from "./en/userpage.json";
import users from "./en/users.json";
import volumes from "./en/volumes.json";

// English only for now (D-65). Resources are bundled statically, so there is
// no language detector and no backend plugin: nothing is fetched at runtime.
// Each later plan adds its own namespace file (setup, auth, shell, env,
// dashboard, audit, overview, install, reason, settings, envsettings, fullpage, switcher, aggregate, bell, backup, invite, needs, users, userpage, sessions, teams, grants, envaccess, owned, noaccess, restore, schedule, actions, activeshells, containers, containerdetail, networks, volumes, logs, terminal, events, pulls, jobs, registries, images, usedBy, tags, services, servicedetail, serviceForm, tasks, specedit, placement, nodes, secrets, configs, stats, restartLoop, stacks, stackChanges, stackConverge, stackDeploy, stackEditor, stackHistory, stackImport, stackLinks, stackLists, stackLogs, stackOverview, stackProblems, stackPull, stackRendered, stackResources, stackRetention, stackReview, stackRevision, stackRollback, stackStandalone, stackUnmanaged, stackVars, stackYamlDiff) and registers it
// here, so parallel work never edits the same JSON. The "containers" namespace
// holds the containers list copy, and the "terminal" namespace the browser shell's
// (never "shell", which is the app chrome's). Keys are shaped
// area.screen.element. Plural keys use the i18next _one and _other suffixes.
export const resources = {
  en: {
    actions,
    activeshells,
    aggregate,
    audit,
    auth,
    backup,
    bell,
    common,
    configs,
    containerdetail,
    containers,
    dashboard,
    env,
    envaccess,
    envsettings,
    events,
    fullpage,
    grants,
    images,
    install,
    invite,
    jobs,
    logs,
    needs,
    networks,
    noaccess,
    nodes,
    overview,
    owned,
    placement,
    pulls,
    reason,
    registries,
    restartLoop: restartloop,
    restore,
    schedule,
    secrets,
    servicedetail,
    serviceForm: serviceform,
    services,
    sessions,
    settings,
    setup,
    shell,
    specEditor: specedit,
    stackChanges: stackchanges,
    stackConverge: stackconverge,
    stackDeploy: stackdeploy,
    stackEditor: stackeditor,
    stackHistory: stackhistory,
    stackImport: stackimport,
    stackLogs: stacklogs,
    stackLinks: stacklinks,
    stackLists: stacklists,
    stackOverview: stackoverview,
    stackProblems: stackproblems,
    stackPull: stackpull,
    stackRemove: stackremove,
    stackRendered: stackrendered,
    stackResources: stackresources,
    stackRetention: stackretention,
    stackStop: stackstop,
    stackUnmanaged: stackunmanaged,
    stackUpdate: stackupdate,
    stackReview: stackreview,
    stackRevision: stackrevision,
    stackRollback: stackrollback,
    stackStandalone: stackstandalone,
    stackVars: stackvars,
    stackYamlDiff: stackyamldiff,
    stacks,
    stats,
    switcher,
    tags,
    tasks,
    teams,
    terminal,
    usedBy: usedby,
    userpage,
    users,
    volumes,
  },
} as const;

void i18n.use(initReactI18next).init({
  resources,
  lng: "en",
  fallbackLng: "en",
  defaultNS: "common",
  ns: Object.keys(resources.en),
  interpolation: {
    // React already escapes rendered values.
    escapeValue: false,
  },
});

export default i18n;
