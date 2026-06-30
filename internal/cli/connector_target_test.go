// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: cover connector target path mapping without live Zotero.

package cli

import (
	"reflect"
	"testing"

	"zotero-pp-cli/internal/connector"
)

func TestConnectorTargetPaths(t *testing.T) {
	selected := connector.SelectedCollection{Targets: []connector.SelectedTarget{
		{ID: "L1", Name: "My Library", Level: 0, FilesEditable: true},
		{ID: "C1", Name: "Parent", Level: 1, FilesEditable: true},
		{ID: "C2", Name: "Child", Level: 2, FilesEditable: true},
		{ID: "C3", Name: "Sibling", Level: 1, FilesEditable: true},
	}}
	got := connectorTargetPaths(selected)
	paths := map[string]string{}
	for _, target := range got {
		paths[target.ID] = target.Path
	}
	want := map[string]string{"C1": "Parent", "C2": "Parent/Child", "C3": "Sibling"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestAPICollectionPaths(t *testing.T) {
	rows := []apiCollectionRow{
		collectionRow("PARENT", "Parent", nil),
		collectionRow("CHILD", "Child", "PARENT"),
		collectionRow("SIBLING", "Sibling", nil),
	}
	got := apiCollectionPaths(rows)
	want := map[string]string{"PARENT": "Parent", "CHILD": "Parent/Child", "SIBLING": "Sibling"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %#v, want %#v", got, want)
	}
}

func collectionRow(key, name string, parent any) apiCollectionRow {
	var row apiCollectionRow
	row.Key = key
	row.Data.Key = key
	row.Data.Name = name
	row.Data.ParentCollection = parent
	return row
}
