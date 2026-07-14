// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import "testing"

func TestCrossRefItemFromWorkPlacesContainerInValidField(t *testing.T) {
	tests := []struct {
		name           string
		crossRefType   string
		itemType       string
		containerField string
		template       map[string]any
	}{
		{
			name:           "journal article",
			crossRefType:   "journal-article",
			itemType:       "journalArticle",
			containerField: "publicationTitle",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "publicationTitle": "",
			},
		},
		{
			name:           "conference paper",
			crossRefType:   "proceedings-article",
			itemType:       "conferencePaper",
			containerField: "proceedingsTitle",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "proceedingsTitle": "",
			},
		},
		{
			name:           "book section",
			crossRefType:   "book-chapter",
			itemType:       "bookSection",
			containerField: "bookTitle",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "bookTitle": "",
			},
		},
		{
			name:           "report",
			crossRefType:   "report",
			itemType:       "report",
			containerField: "extra",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "extra": "",
			},
		},
		{
			name:           "preprint",
			crossRefType:   "posted-content",
			itemType:       "preprint",
			containerField: "extra",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "extra": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := crossRefItemFromWork(crossRefWork{
				Type:           tt.crossRefType,
				Title:          []string{"Container test"},
				DOI:            "10.1234/container",
				ContainerTitle: []string{"Container Name"},
			}, "10.1234/container")

			if got := item["itemType"]; got != tt.itemType {
				t.Fatalf("itemType = %q, want %q", got, tt.itemType)
			}
			wantContainer := "Container Name"
			if tt.containerField == "extra" {
				wantContainer = "Container: Container Name"
			}
			if got := item[tt.containerField]; got != wantContainer {
				t.Errorf("%s = %q, want %q", tt.containerField, got, wantContainer)
			}
			// Use the same template-backed validator used before an import POST.
			// The per-type template deliberately includes only the valid destination
			// field, so an accidental publicationTitle assignment fails here.
			if err := validateItemFields(tt.template, item); err != nil {
				t.Fatalf("validateItemFields(%v) = %v", item, err)
			}
		})
	}
}

func TestSetCrossRefContainerTitleUsesPublicationTitleForPeriodicals(t *testing.T) {
	for _, itemType := range []string{"journalArticle", "magazineArticle", "newspaperArticle"} {
		t.Run(itemType, func(t *testing.T) {
			item := map[string]any{"itemType": itemType}
			setCrossRefContainerTitle(item, "Periodical Name")

			if got := item["publicationTitle"]; got != "Periodical Name" {
				t.Errorf("publicationTitle = %q, want %q", got, "Periodical Name")
			}
			if err := validateItemFields(map[string]any{
				"itemType": "", "publicationTitle": "",
			}, item); err != nil {
				t.Fatalf("validateItemFields(%v) = %v", item, err)
			}
		})
	}
}

func TestCrossRefContainerTitleOmitsUnverifiedThesisUniversity(t *testing.T) {
	item := crossRefItemFromWork(crossRefWork{
		Type:           "dissertation",
		Title:          []string{"Thesis title"},
		DOI:            "10.1234/thesis",
		ContainerTitle: []string{"Container Name"},
	}, "10.1234/thesis")

	if _, ok := item["university"]; ok {
		t.Errorf("university = %q, want omitted because a CrossRef container is not verified as a university", item["university"])
	}
	if _, ok := item["extra"]; ok {
		t.Errorf("extra = %q, want omitted for a thesis container", item["extra"])
	}
	if err := validateItemFields(map[string]any{
		"itemType": "", "title": "", "DOI": "",
	}, item); err != nil {
		t.Fatalf("validateItemFields(%v) = %v", item, err)
	}
}
