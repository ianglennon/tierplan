// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/rbac"
)

func TestRequirementOfEveryRegisteredPermission(t *testing.T) {
	want := map[rbac.Perm]rbac.Need{
		rbac.PermEnvironmentsRead:      {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermEnvironmentsCreate:    {Kind: rbac.NeedInstance},
		rbac.PermEnvironmentsManage:    {Kind: rbac.NeedInstance},
		rbac.PermEnvironmentsToken:     {Kind: rbac.NeedInstance},
		rbac.PermEnvironmentsRevoke:    {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermEnvironmentsDelete:    {Kind: rbac.NeedInstance},
		rbac.PermEnvironmentsConfigure: {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermEnvironmentsAccess:    {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermEnvironmentsAudit:     {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermContainersRead:        {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermImagesRead:            {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermVolumesRead:           {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermNetworksRead:          {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermContainersControl:     {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermContainersRemove:      {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermImagesPull:            {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermImagesDelete:          {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermNetworksManage:        {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermNetworksConnect:       {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermVolumesCreate:         {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermAuditRead:             {Kind: rbac.NeedInstance},
		rbac.PermSettingsRead:          {Kind: rbac.NeedInstance},
		rbac.PermSettingsWrite:         {Kind: rbac.NeedInstance},
		rbac.PermUsersManage:           {Kind: rbac.NeedInstance},
		rbac.PermTeamsManage:           {Kind: rbac.NeedInstance},
		rbac.PermBackupManage:          {Kind: rbac.NeedInstance},
		rbac.PermContainersExec:        {Kind: rbac.NeedExtra, Role: rbac.RoleOperator},
		rbac.PermSecretsReveal:         {Kind: rbac.NeedExtra, Role: rbac.RoleOperator},
		rbac.PermVolumesDelete:         {Kind: rbac.NeedExtra, Role: rbac.RoleOperator},
		rbac.PermJanitorPolicy:         {Kind: rbac.NeedExtra, Role: rbac.RoleOperator},
		rbac.PermShellsManage:          {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermRegistriesInstance:    {Kind: rbac.NeedInstance},
		rbac.PermRegistriesManage:      {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermServicesRead:          {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermServicesControl:       {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermServicesManage:        {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermNodesRead:             {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermNodesManage:           {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermSecretsRead:           {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermSecretsManage:         {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermConfigsRead:           {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermConfigsManage:         {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermStacksRead:            {Kind: rbac.NeedRole, Role: rbac.RoleViewer},
		rbac.PermStacksUpdate:          {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermStacksDeploy:          {Kind: rbac.NeedRole, Role: rbac.RoleOperator},
		rbac.PermStacksCreate:          {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
		rbac.PermStacksDelete:          {Kind: rbac.NeedRole, Role: rbac.RoleAdmin},
	}
	all := rbac.All()
	if len(all) != len(want) {
		t.Fatalf("registry has %d permissions, the table pins %d: a new permission needs a row here", len(all), len(want))
	}
	for _, p := range all {
		if got := rbac.RequirementOf(p); got != want[p.Name] {
			t.Errorf("RequirementOf(%s) = %+v, want %+v", p.Name, got, want[p.Name])
		}
	}
}

func TestMatrixRowsAreSortedAndFollowTheRegistry(t *testing.T) {
	rows := rbac.MatrixRows(rbac.DefaultRegistry())
	if len(rows) != len(rbac.All()) {
		t.Fatalf("%d rows for %d permissions", len(rows), len(rbac.All()))
	}
	if !slices.IsSortedFunc(rows, func(a, b rbac.MatrixRow) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	}) {
		t.Fatal("rows are not sorted by permission name")
	}
	byName := map[string]rbac.MatrixRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	read := byName["containers.read"]
	if !read.Viewer || !read.Operator || !read.Admin || read.Extra || read.Scope != "environment" {
		t.Errorf("containers.read = %+v, want every role and no extra", read)
	}
	cfg := byName["environments.configure"]
	if cfg.Viewer || cfg.Operator || !cfg.Admin {
		t.Errorf("environments.configure = %+v, want Admin only", cfg)
	}
	exec := byName["containers.exec"]
	if exec.Viewer || exec.Operator || !exec.Admin || !exec.Extra || !exec.Dangerous {
		t.Errorf("containers.exec = %+v, want Admin, and an Operator only as an extra", exec)
	}
	users := byName["users.manage"]
	if users.Viewer || users.Operator || users.Admin || users.Scope != "global" {
		t.Errorf("users.manage = %+v, want instance admin only", users)
	}
}
