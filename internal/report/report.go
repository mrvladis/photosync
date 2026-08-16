// Package report turns the journal into something a person can read: a
// self-contained HTML page, and a CSV manifest with a row for every file.
//
// The HTML is the summary - what was found, what moved, what did not, and why.
// The CSV is the evidence - name, size, source path, verdict, final state, the
// media item id the library assigned, and the album it landed in - so any claim
// on the page can be checked against a specific file.
package report

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrvladis/photosync/internal/journal"
)

// Data is everything the template renders.
type Data struct {
	Generated   time.Time
	SourceRoot  string
	TargetTree  string
	AlbumPrefix string

	SourceFiles int64
	SourceBytes int64
	DriveFiles  int64
	DriveBytes  int64

	Verdicts []Row
	States   []Row
	Kinds    []Row
	Types    []TypeRow
	Albums   []AlbumRow
	Failures []FailureRow
	Skipped  []FailureRow
	Prune    PruneSummary
	Samples  []SampleRow

	ManifestPath string
	Caveats      []string
}

type Row struct {
	Label string
	Files int64
	Bytes int64
	Note  string
}

type TypeRow struct {
	Ext      string
	Kind     string
	Files    int64
	Bytes    int64
	Created  int64
	Pending  int64
	Failed   int64
	Progress float64
}

type AlbumRow struct {
	Album    string
	Files    int64
	Bytes    int64
	Created  int64
	Failed   int64
	Progress float64
}

type FailureRow struct {
	Path   string
	Size   int64
	Reason string
}

type SampleRow struct {
	Name      string
	Size      int64
	DriveSize int64
	Ratio     string
	Path      string
}

type PruneSummary struct {
	DeleteFiles  int64
	DeleteBytes  int64
	ReviewFiles  int64
	ReviewBytes  int64
	DeletedFiles int64
	DeletedBytes int64
	Reviews      []FailureRow
}

// Build reads the journal and assembles the report data.
func Build(j *journal.Journal, sourceRoot, targetTree, albumPrefix string, driveFiles, driveBytes int64) (Data, error) {
	db := j.DB()
	d := Data{
		Generated:   time.Now(),
		SourceRoot:  sourceRoot,
		TargetTree:  targetTree,
		AlbumPrefix: albumPrefix,
		DriveFiles:  driveFiles,
		DriveBytes:  driveBytes,
	}

	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size),0) FROM census`).
		Scan(&d.SourceFiles, &d.SourceBytes); err != nil {
		return d, err
	}

	verdictNotes := map[string]string{
		"present": "already in Google Drive at the identical byte size",
		"resized": "Google holds a smaller re-encode under the same name, not the original",
		"missing": "no counterpart under that name anywhere in the Google Photos tree",
	}
	var err error
	if d.Verdicts, err = rowsFrom(db,
		`SELECT verdict, COUNT(*), COALESCE(SUM(size),0) FROM census GROUP BY verdict ORDER BY 3 DESC`,
		verdictNotes); err != nil {
		return d, err
	}

	stateNotes := map[string]string{
		"created": "media item confirmed in the Google Photos library",
		"queued":  "in the work-list, not yet sent",
		"failed":  "attempted and rejected - see the failures table",
		"skipped": "deliberately excluded, with a stated reason",
	}
	if d.States, err = rowsFrom(db,
		`SELECT state, COUNT(*), COALESCE(SUM(size),0) FROM files GROUP BY state ORDER BY 2 DESC`,
		stateNotes); err != nil {
		return d, err
	}

	if d.Kinds, err = rowsFrom(db,
		`SELECT kind, COUNT(*), COALESCE(SUM(size),0) FROM census GROUP BY kind ORDER BY 3 DESC`,
		nil); err != nil {
		return d, err
	}

	if d.Types, err = typeRows(db); err != nil {
		return d, err
	}
	if d.Albums, err = albumRows(db); err != nil {
		return d, err
	}
	if d.Failures, err = failureRows(db, `state='failed'`, 300); err != nil {
		return d, err
	}
	if d.Skipped, err = failureRows(db, `state='skipped'`, 300); err != nil {
		return d, err
	}
	if d.Samples, err = sampleRows(db); err != nil {
		return d, err
	}
	if d.Prune, err = pruneSummary(db); err != nil {
		return d, err
	}
	return d, nil
}

func rowsFrom(db *sql.DB, query string, notes map[string]string) ([]Row, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Label, &r.Files, &r.Bytes); err != nil {
			return nil, err
		}
		r.Note = notes[r.Label]
		out = append(out, r)
	}
	return out, rows.Err()
}

// typeRows aggregates the archive by file extension. SQLite has no extension
// function and the usual rtrim/replace idiom for one is unreadable, so the
// grouping is done here instead.
func typeRows(db *sql.DB) ([]TypeRow, error) {
	byExt := map[string]*TypeRow{}
	get := func(ext, kind string) *TypeRow {
		if t, ok := byExt[ext]; ok {
			return t
		}
		t := &TypeRow{Ext: ext, Kind: kind}
		byExt[ext] = t
		return t
	}

	rows, err := db.Query(`SELECT name, kind, size FROM census`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, kind string
		var size int64
		if err := rows.Scan(&name, &kind, &size); err != nil {
			rows.Close()
			return nil, err
		}
		t := get(extOf(name), kind)
		t.Files++
		t.Bytes += size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Overlay per-extension transfer progress from the work-list.
	prog, err := db.Query(`SELECT name, kind, state FROM files`)
	if err != nil {
		return nil, err
	}
	for prog.Next() {
		var name, kind, state string
		if err := prog.Scan(&name, &kind, &state); err != nil {
			prog.Close()
			return nil, err
		}
		t := get(extOf(name), kind)
		switch journal.State(state) {
		case journal.Created:
			t.Created++
		case journal.Failed:
			t.Failed++
		case journal.Queued, journal.Uploaded:
			t.Pending++
		}
	}
	prog.Close()
	if err := prog.Err(); err != nil {
		return nil, err
	}

	out := make([]TypeRow, 0, len(byExt))
	for _, t := range byExt {
		if total := t.Created + t.Pending + t.Failed; total > 0 {
			t.Progress = 100 * float64(t.Created) / float64(total)
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

func extOf(name string) string {
	if e := filepath.Ext(name); e != "" {
		return strings.ToLower(e[1:])
	}
	return "(none)"
}

func albumRows(db *sql.DB) ([]AlbumRow, error) {
	rows, err := db.Query(`
		SELECT album, COUNT(*), COALESCE(SUM(size),0),
		       SUM(state='created'), SUM(state='failed')
		  FROM files GROUP BY album ORDER BY 3 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlbumRow
	for rows.Next() {
		var a AlbumRow
		if err := rows.Scan(&a.Album, &a.Files, &a.Bytes, &a.Created, &a.Failed); err != nil {
			return nil, err
		}
		if a.Files > 0 {
			a.Progress = 100 * float64(a.Created) / float64(a.Files)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func failureRows(db *sql.DB, where string, limit int) ([]FailureRow, error) {
	rows, err := db.Query(`SELECT path, size, error FROM files WHERE ` + where +
		` ORDER BY size DESC LIMIT ` + strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FailureRow
	for rows.Next() {
		var f FailureRow
		if err := rows.Scan(&f.Path, &f.Size, &f.Reason); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// sampleRows illustrates the re-encode finding with the starkest examples.
func sampleRows(db *sql.DB) ([]SampleRow, error) {
	rows, err := db.Query(`
		SELECT s.name, s.size, c.drive_size, s.path
		  FROM census s JOIN counterparts c ON c.onedrive_id = s.onedrive_id
		 WHERE s.verdict='resized' AND c.drive_size < s.size
		 ORDER BY (s.size - c.drive_size) DESC LIMIT 12`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SampleRow
	for rows.Next() {
		var s SampleRow
		if err := rows.Scan(&s.Name, &s.Size, &s.DriveSize, &s.Path); err != nil {
			return nil, err
		}
		if s.DriveSize > 0 {
			s.Ratio = fmt.Sprintf("%.0f×", float64(s.Size)/float64(s.DriveSize))
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func pruneSummary(db *sql.DB) (PruneSummary, error) {
	var p PruneSummary
	rows, err := db.Query(`SELECT decision, COUNT(*), COALESCE(SUM(drive_size),0),
	                              SUM(deleted_at>0), COALESCE(SUM(CASE WHEN deleted_at>0 THEN drive_size ELSE 0 END),0)
	                         FROM prune GROUP BY decision`)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var decision string
		var n, b, delN, delB int64
		if err := rows.Scan(&decision, &n, &b, &delN, &delB); err != nil {
			rows.Close()
			return p, err
		}
		switch decision {
		case "delete":
			p.DeleteFiles, p.DeleteBytes = n, b
		case "review":
			p.ReviewFiles, p.ReviewBytes = n, b
		}
		p.DeletedFiles += delN
		p.DeletedBytes += delB
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}

	rev, err := db.Query(`SELECT drive_path, drive_size, reason FROM prune
	                       WHERE decision='review' ORDER BY drive_size DESC LIMIT 200`)
	if err != nil {
		return p, err
	}
	defer rev.Close()
	for rev.Next() {
		var f FailureRow
		if err := rev.Scan(&f.Path, &f.Size, &f.Reason); err != nil {
			return p, err
		}
		p.Reviews = append(p.Reviews, f)
	}
	return p, rev.Err()
}

// Manifest writes one CSV row per source file: the auditable record behind
// every number on the HTML page.
func Manifest(j *journal.Journal, path string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"name", "onedrive_path", "bytes", "kind", "verdict",
		"drive_counterparts", "drive_counterpart_paths", "drive_counterpart_bytes",
		"transfer_state", "album", "media_item_id", "attempts", "error",
		"date_taken", "modified", "uploaded_at",
	}); err != nil {
		return 0, err
	}

	// One row per source file. A file can have several Drive counterparts, so
	// those are folded into a single cell rather than fanning the join out into
	// duplicate rows - the manifest's row count must equal the file count.
	rows, err := j.DB().Query(`
		SELECT s.name, s.path, s.size, s.kind, s.verdict, s.taken, s.modified,
		       (SELECT COUNT(*) FROM counterparts c WHERE c.onedrive_id = s.onedrive_id),
		       COALESCE((SELECT GROUP_CONCAT(c.drive_path, ' | ') FROM counterparts c
		                  WHERE c.onedrive_id = s.onedrive_id), ''),
		       COALESCE((SELECT GROUP_CONCAT(c.drive_size, ' | ') FROM counterparts c
		                  WHERE c.onedrive_id = s.onedrive_id), ''),
		       COALESCE(f.state,''), COALESCE(f.album,''), COALESCE(f.media_item_id,''),
		       COALESCE(f.attempts,0), COALESCE(f.error,''), COALESCE(f.finished_at,0)
		  FROM census s
		  LEFT JOIN files f ON f.onedrive_id = s.onedrive_id
		 ORDER BY s.path`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var name, path, kind, verdict, drivePaths, driveSizes, state, album, mediaID, errMsg string
		var size, taken, modified, driveCount, attempts, finished int64
		if err := rows.Scan(&name, &path, &size, &kind, &verdict, &taken, &modified,
			&driveCount, &drivePaths, &driveSizes,
			&state, &album, &mediaID, &attempts, &errMsg, &finished); err != nil {
			return n, err
		}
		if err := w.Write([]string{
			name, path, strconv.FormatInt(size, 10), kind, verdict,
			strconv.FormatInt(driveCount, 10), drivePaths, driveSizes,
			state, album, mediaID, strconv.FormatInt(attempts, 10), errMsg,
			stamp(taken), stamp(modified), stamp(finished),
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func stamp(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}

// Write renders the HTML report.
func Write(d Data, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sort.SliceStable(d.Albums, func(i, j int) bool { return d.Albums[i].Bytes > d.Albums[j].Bytes })
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pageTemplate.Execute(f, d)
}
