package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestModelExclusionPersistence(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "model-exclusions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u := &upstream.Upstream{Name: "provider", BaseURL: "http://provider", APIKey: "key", Enabled: true}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordModelExclusion(u.ID, "glm-5", nil, 404, "model_not_found", now); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Hour)
	if err := st.RecordModelExclusion(u.ID, "glm-5", &expires, 400, "missing model", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	records, err := st.ListModelExclusions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].FailureCount != 2 || records[0].LastStatus != 400 || records[0].ExcludedUntil == nil {
		t.Fatalf("unexpected exclusion: %+v", records)
	}
	if err := st.DeleteModelExclusion(u.ID, "glm-5"); err != nil {
		t.Fatal(err)
	}
	if records, _ = st.ListModelExclusions(); len(records) != 0 {
		t.Fatalf("manual recovery did not delete exclusion: %+v", records)
	}
}

func TestDeletingUpstreamDeletesModelExclusions(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "model-exclusion-cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{Name: "provider", BaseURL: "http://provider", APIKey: "key", Enabled: true}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordModelExclusion(u.ID, "glm-5", nil, 404, "model_not_found", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(u.ID); err != nil {
		t.Fatal(err)
	}
	if records, _ := st.ListModelExclusions(); len(records) != 0 {
		t.Fatalf("upstream delete left model exclusions behind: %+v", records)
	}
}
