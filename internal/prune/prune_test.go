package prune

import (
	"path/filepath"
	"testing"

	"github.com/mrvladis/photosync/internal/journal"
)

// The prune rules are the only place photosync deletes anything, so each way a
// deletion can be wrong gets its own case.
func TestPlanOnlyDeletesUnambiguousReencodes(t *testing.T) {
	j := newJournal(t)

	// Clean case: one original, one smaller Drive copy, upload confirmed.
	addSource(t, j, "od-clean", "DCIM/trip/clean.jpg", "clean.jpg", 4_000_000)
	addCounterpart(t, j, "od-clean", "gd-clean", "2019/06/clean.jpg", 400_000)
	addTransferred(t, j, "od-clean", "clean.jpg", 4_000_000, journal.Created, "media-1")

	// Never uploaded: the original is not safe yet.
	addSource(t, j, "od-pending", "DCIM/trip/pending.jpg", "pending.jpg", 4_000_000)
	addCounterpart(t, j, "od-pending", "gd-pending", "2019/06/pending.jpg", 400_000)
	addTransferred(t, j, "od-pending", "pending.jpg", 4_000_000, journal.Queued, "")

	// Two OneDrive files share a name: we cannot tell which original the Drive
	// copy belongs to, so the copy might be the last of a different photo.
	addSource(t, j, "od-dup-a", "DCIM/a/img_4821.jpg", "img_4821.jpg", 5_144_576)
	addSource(t, j, "od-dup-b", "DCIM/b/img_4821.jpg", "img_4821.jpg", 6_225_920)
	addCounterpart(t, j, "od-dup-a", "gd-dup", "2016/07/img_4821.jpg", 618_148)
	addTransferred(t, j, "od-dup-a", "img_4821.jpg", 5_144_576, journal.Created, "media-2")

	// The Drive copy is larger, so it is not a re-encode of this file at all.
	addSource(t, j, "od-bigger", "DCIM/trip/bigger.jpg", "bigger.jpg", 100_000)
	addCounterpart(t, j, "od-bigger", "gd-bigger", "2019/06/bigger.jpg", 900_000)
	addTransferred(t, j, "od-bigger", "bigger.jpg", 100_000, journal.Created, "media-3")

	plan, err := Plan(j)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]Decision{}
	for _, c := range plan {
		got[c.DriveID] = c.Decision
	}
	want := map[string]Decision{
		"gd-clean":   Delete,
		"gd-pending": Review,
		"gd-dup":     Review,
		"gd-bigger":  Review,
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %q, want %q", id, got[id], w)
		}
	}
	if len(plan) != len(want) {
		t.Errorf("plan has %d candidates, want %d", len(plan), len(want))
	}
}

// A file whose Google copy is the same size is "present", not "resized", and
// must never enter the deletion plan at all.
func TestPlanIgnoresExactMatches(t *testing.T) {
	j := newJournal(t)
	addSourceVerdict(t, j, "od-same", "DCIM/trip/same.jpg", "same.jpg", 1000, "present")
	addCounterpart(t, j, "od-same", "gd-same", "2019/06/same.jpg", 1000)

	plan, err := Plan(j)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("identical copies must not be deletion candidates, got %d", len(plan))
	}
}

func TestSaveKeepsAlreadyDeletedRows(t *testing.T) {
	j := newJournal(t)
	addSource(t, j, "od-1", "DCIM/trip/x.jpg", "x.jpg", 2000)
	addCounterpart(t, j, "od-1", "gd-1", "2019/06/x.jpg", 200)
	addTransferred(t, j, "od-1", "x.jpg", 2000, journal.Created, "media-1")

	plan, err := Plan(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(j, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := j.DB().Exec(`UPDATE prune SET deleted_at=1 WHERE drive_id='gd-1'`); err != nil {
		t.Fatal(err)
	}
	// Re-planning must not resurrect a row that has already been acted on.
	if err := Save(j, plan); err != nil {
		t.Fatal(err)
	}
	var deletedAt int64
	if err := j.DB().QueryRow(`SELECT deleted_at FROM prune WHERE drive_id='gd-1'`).
		Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if deletedAt != 1 {
		t.Fatalf("deleted_at was reset to %d", deletedAt)
	}
}

// ------------------------------------------------------------------ helpers

func newJournal(t *testing.T) *journal.Journal {
	t.Helper()
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func addSource(t *testing.T, j *journal.Journal, id, path, name string, size int64) {
	addSourceVerdict(t, j, id, path, name, size, "resized")
}

func addSourceVerdict(t *testing.T, j *journal.Journal, id, path, name string, size int64, verdict string) {
	t.Helper()
	if _, err := j.DB().Exec(
		`INSERT INTO census (onedrive_id,path,name,size,kind,verdict) VALUES (?,?,?,?,?,?)`,
		id, path, name, size, "image", verdict); err != nil {
		t.Fatal(err)
	}
}

func addCounterpart(t *testing.T, j *journal.Journal, sourceID, driveID, drivePath string, size int64) {
	t.Helper()
	if _, err := j.DB().Exec(
		`INSERT INTO counterparts (onedrive_id,drive_id,drive_path,drive_size) VALUES (?,?,?,?)`,
		sourceID, driveID, drivePath, size); err != nil {
		t.Fatal(err)
	}
}

func addTransferred(t *testing.T, j *journal.Journal, id, name string, size int64,
	state journal.State, mediaItem string) {
	t.Helper()
	if _, err := j.DB().Exec(
		`INSERT INTO files (onedrive_id,path,name,size,kind,verdict,album,state,media_item_id)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		id, "DCIM/trip/"+name, name, size, "image", "resized", "trip",
		string(state), mediaItem); err != nil {
		t.Fatal(err)
	}
}
