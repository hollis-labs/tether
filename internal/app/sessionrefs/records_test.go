package sessionrefs

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

type recordRows struct {
	attached            []store.SessionRefRow
	rows                []store.SessionRefRow
	result              store.AttachRefResult
	err                 error
	session, workstream string
	filter              store.ListSessionRefsOptions
}

func (s *recordRows) AttachSessionRef(row store.SessionRefRow) (store.AttachRefResult, error) {
	s.attached = append(s.attached, row)
	return s.result, s.err
}
func (s *recordRows) ListSessionRefs(id string, f store.ListSessionRefsOptions) ([]store.SessionRefRow, error) {
	s.session, s.filter = id, f
	return s.rows, s.err
}
func (s *recordRows) ListWorkstreamRefs(id string, f store.ListSessionRefsOptions) ([]store.SessionRefRow, error) {
	s.workstream, s.filter = id, f
	return s.rows, s.err
}

func TestRecordsAttachPreservesDefaultsAndAuthoredSource(t *testing.T) {
	for _, source := range []string{"", "agent", "proxy"} {
		rows := &recordRows{result: store.AttachRefResult{Inserted: true, Upgraded: true}}
		out, err := NewRecords(rows).Attach("own", SessionRefAttachRequest{Kind: "tesseract_revision", RefID: "revision", URI: "uri", ParentItemID: "item", Source: source})
		wantSource := source
		if wantSource == "" {
			wantSource = store.SourceAPI
		}
		wantRow := store.SessionRefRow{SessionID: "own", Kind: "tesseract_revision", RefID: "revision", URI: "uri", ParentItemID: "item", Source: wantSource}
		want := SessionRefAttachResponse{Inserted: true, Upgraded: true, Ref: SessionRefDTO{SessionID: "own", Kind: "tesseract_revision", RefID: "revision", URI: "uri", ParentItemID: "item", Source: wantSource, Relation: store.RelationReferenced}}
		if err != nil || !reflect.DeepEqual(rows.attached, []store.SessionRefRow{wantRow}) || !reflect.DeepEqual(out, want) {
			t.Fatalf("source=%q rows=%+v out=%+v err=%v", source, rows.attached, out, err)
		}
	}
	rows := &recordRows{err: errors.New("invalid source")}
	s := NewRecords(rows)
	if _, err := s.Attach("own", SessionRefAttachRequest{}); err == nil || err.Error() != "kind and ref_id are required" || len(rows.attached) != 0 {
		t.Fatalf("required validation: rows=%+v err=%v", rows.attached, err)
	}
	if _, err := s.Attach("own", SessionRefAttachRequest{Kind: "kind", RefID: "id"}); !errors.Is(err, rows.err) {
		t.Fatalf("store error=%v", err)
	}
}

func TestRecordsListsTranslateFiltersAndEmptyResults(t *testing.T) {
	for _, workstream := range []bool{false, true} {
		rows := &recordRows{}
		s := NewRecords(rows)
		query := Filters{Kind: "kind", Relation: "read", Source: "proxy"}
		var out SessionRefListResponse
		var err error
		if workstream {
			out, err = s.ListWorkstream("work", query)
		} else {
			out, err = s.ListSession("own", query)
		}
		raw, jsonErr := json.Marshal(out)
		if err != nil || jsonErr != nil || string(raw) != `{"refs":[]}` || !reflect.DeepEqual(rows.filter, store.ListSessionRefsOptions{Kind: query.Kind, Relation: query.Relation, Source: query.Source}) {
			t.Fatalf("list=%s filter=%+v err=%v/%v", raw, rows.filter, err, jsonErr)
		}
		if workstream && rows.workstream != "work" || !workstream && rows.session != "own" {
			t.Fatalf("selectors=%q/%q", rows.session, rows.workstream)
		}
		rows.err = errors.New("offline")
		if workstream {
			_, err = s.ListWorkstream("work", query)
		} else {
			_, err = s.ListSession("own", query)
		}
		if !errors.Is(err, rows.err) {
			t.Fatalf("query error=%v", err)
		}
	}
}
