// Package journal is the durable state of a transfer.
//
// A full run moves tens of thousands of files over hundreds of gigabytes across
// several days, through two rate-limited APIs and a laptop that will sleep. It
// has to be interruptible at any instant and resume without re-uploading, and
// afterwards it has to be able to say precisely what happened to every single
// file. Both needs are served by the same SQLite database: the work-list, the
// per-file state machine, and the raw API responses that later become the
// report's evidence.
package journal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mrvladis/photosync/internal/inventory"
	"github.com/mrvladis/photosync/internal/match"
)

// State is a file's position in the transfer state machine.
type State string

const (
	Queued   State = "queued"   // in the work-list, nothing sent yet
	Uploaded State = "uploaded" // bytes accepted, holds an upload token
	Created  State = "created"  // media item exists in the library - done
	Failed   State = "failed"   // gave up after retries; Error explains why
	Skipped  State = "skipped"  // deliberately excluded, e.g. over the size cap
)

// File is one row of the work-list.
type File struct {
	OneDriveID  string
	Path        string
	Name        string
	Size        int64
	Kind        string
	Verdict     string // match.Status at analysis time
	Album       string
	State       State
	UploadToken string
	TokenAt     int64
	MediaItemID string
	Attempts    int
	Error       string
	StartedAt   int64
	FinishedAt  int64
	// UploadName and UploadSize describe what was actually sent, which differs
	// from Name and Size when a RAW file was converted to HEIF on the way.
	UploadName string
	UploadSize int64
}

// Journal is an open state database.
type Journal struct {
	db   *sql.DB
	path string
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS files (
    onedrive_id   TEXT PRIMARY KEY,
    path          TEXT NOT NULL,
    name          TEXT NOT NULL,
    size          INTEGER NOT NULL,
    kind          TEXT NOT NULL,
    verdict       TEXT NOT NULL,
    album         TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL,
    upload_token  TEXT NOT NULL DEFAULT '',
    token_at      INTEGER NOT NULL DEFAULT 0,
    media_item_id TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL DEFAULT 0,
    finished_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS files_state_idx ON files(state);
CREATE INDEX IF NOT EXISTS files_album_idx ON files(album);

-- Every source file the analysis saw, including the ones needing no transfer.
-- Kept apart from files so the report can describe the whole archive while the
-- work-list stays small and hot.
CREATE TABLE IF NOT EXISTS census (
    onedrive_id TEXT PRIMARY KEY,
    path        TEXT NOT NULL,
    name        TEXT NOT NULL,
    size        INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    verdict     TEXT NOT NULL,
    taken       INTEGER NOT NULL DEFAULT 0,
    modified    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS census_verdict_idx ON census(verdict);

-- What the Google side already holds under a matching name.
CREATE TABLE IF NOT EXISTS counterparts (
    onedrive_id TEXT NOT NULL,
    drive_id    TEXT NOT NULL,
    drive_path  TEXT NOT NULL,
    drive_size  INTEGER NOT NULL,
    PRIMARY KEY (onedrive_id, drive_id)
);

CREATE TABLE IF NOT EXISTS albums (
    title      TEXT PRIMARY KEY,
    album_id   TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

-- Raw API outcomes, appended and never rewritten: the report's evidence.
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          INTEGER NOT NULL,
    onedrive_id TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_kind_idx ON events(kind);

-- API requests spent per UTC day. The Library API allows 10,000 a day per
-- project and byte uploads count towards it, so the spend has to survive
-- restarts or a resumed run would blow through the ceiling.
CREATE TABLE IF NOT EXISTS api_usage (
    day      TEXT PRIMARY KEY,
    requests INTEGER NOT NULL DEFAULT 0
);

-- Drive re-encodes considered for deletion once their original is safely in the
-- library. Populated by the prune pass; deleted_at stays 0 until it acts.
CREATE TABLE IF NOT EXISTS prune (
    drive_id    TEXT PRIMARY KEY,
    drive_path  TEXT NOT NULL,
    drive_size  INTEGER NOT NULL,
    onedrive_id TEXT NOT NULL DEFAULT '',
    decision    TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    deleted_at  INTEGER NOT NULL DEFAULT 0
);
`

// Open creates or reopens the journal at path.
func Open(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Columns added after journals were already in use. SQLite has no
	// ALTER TABLE ... IF NOT EXISTS, so a duplicate-column error means the
	// migration has already run and is not a failure.
	for _, stmt := range []string{
		`ALTER TABLE files ADD COLUMN upload_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE files ADD COLUMN upload_size INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate schema: %w", err)
		}
	}
	return &Journal{db: db, path: path}, nil
}

func (j *Journal) Close() error { return j.db.Close() }
func (j *Journal) Path() string { return j.path }

// --------------------------------------------------------------------- meta

func (j *Journal) SetMeta(key string, value any) error {
	blob, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = j.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
	                    ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, string(blob))
	return err
}

func (j *Journal) Meta(key string, into any) error {
	var raw string
	if err := j.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&raw); err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), into)
}

// ------------------------------------------------------------------ analysis

// Record writes an analysis pass: the full census, the Google-side
// counterparts, and the work-list. Existing work-list rows keep their state, so
// re-analysing a partly transferred archive is safe and does not resend files.
func (j *Journal) Record(all []match.Result, work []match.Result, album func(inventory.Item) string) error {
	tx, err := j.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM census`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM counterparts`); err != nil {
		return err
	}

	censusStmt, err := tx.Prepare(`INSERT INTO census
		(onedrive_id,path,name,size,kind,verdict,taken,modified) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	cpStmt, err := tx.Prepare(`INSERT OR IGNORE INTO counterparts
		(onedrive_id,drive_id,drive_path,drive_size) VALUES(?,?,?,?)`)
	if err != nil {
		return err
	}
	for _, r := range all {
		if _, err := censusStmt.Exec(r.Item.ID, r.Item.Path, r.Item.Name, r.Item.Size,
			match.Kind(r.Item), string(r.Status), r.Item.Taken, r.Item.Modified); err != nil {
			return err
		}
		for _, c := range r.Counterparts {
			if _, err := cpStmt.Exec(r.Item.ID, c.ID, c.Path, c.Size); err != nil {
				return err
			}
		}
	}

	// Keep state for rows already in flight; refresh their descriptive columns.
	workStmt, err := tx.Prepare(`
		INSERT INTO files (onedrive_id,path,name,size,kind,verdict,album,state)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(onedrive_id) DO UPDATE SET
		    path=excluded.path, name=excluded.name, size=excluded.size,
		    kind=excluded.kind, verdict=excluded.verdict, album=excluded.album`)
	if err != nil {
		return err
	}
	for _, r := range work {
		if _, err := workStmt.Exec(r.Item.ID, r.Item.Path, r.Item.Name, r.Item.Size,
			match.Kind(r.Item), string(r.Status), album(r.Item), string(Queued)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ----------------------------------------------------------------- work-list

// Pending returns up to limit files still needing work, album by album so that
// each batchCreate call can target a single album as the API requires.
//
// Rows that have already burned their attempts are excluded. Without that a
// chunk whose batchCreate keeps failing would be handed back on every pass and
// the run would spin on it forever.
// exts, when non-empty, restricts the result to those lowercase extensions
// (without the dot) - used to steer a pilot run at the formats whose acceptance
// is uncertain rather than whatever happens to sort first.
func (j *Journal) Pending(limit, maxAttempts int, exts []string) ([]File, error) {
	q := `SELECT onedrive_id,path,name,size,kind,verdict,album,state,
	             upload_token,token_at,media_item_id,attempts,error,started_at,finished_at,
	             upload_name,upload_size
	        FROM files
	       WHERE state IN ('queued','uploaded') AND attempts < ?`
	args := []any{maxAttempts}
	if len(exts) > 0 {
		q += ` AND ` + extClause(exts, &args)
	}
	q += ` ORDER BY album, path LIMIT ?`
	args = append(args, limit)

	rows, err := j.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFiles(rows)
}

// extClause builds a parameterised suffix test. SQLite has no extension
// function, and LIKE with a user-supplied pattern would let a stray wildcard
// widen the match, so the comparison is an explicit lowercased suffix.
func extClause(exts []string, args *[]any) string {
	var parts []string
	for _, e := range exts {
		e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e == "" {
			continue
		}
		parts = append(parts, `LOWER(SUBSTR(name, LENGTH(name)-?)) = ?`)
		*args = append(*args, len(e), "."+e)
	}
	if len(parts) == 0 {
		return "1=1"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// Retryable returns files that failed but are worth another attempt.
func (j *Journal) Retryable(maxAttempts, limit int) ([]File, error) {
	rows, err := j.db.Query(`
		SELECT onedrive_id,path,name,size,kind,verdict,album,state,
		       upload_token,token_at,media_item_id,attempts,error,started_at,finished_at,
		       upload_name,upload_size
		  FROM files
		 WHERE state='failed' AND attempts < ?
		 ORDER BY album, path
		 LIMIT ?`, maxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFiles(rows)
}

func scanFiles(rows *sql.Rows) ([]File, error) {
	var out []File
	for rows.Next() {
		var f File
		var state string
		if err := rows.Scan(&f.OneDriveID, &f.Path, &f.Name, &f.Size, &f.Kind, &f.Verdict,
			&f.Album, &state, &f.UploadToken, &f.TokenAt, &f.MediaItemID,
			&f.Attempts, &f.Error, &f.StartedAt, &f.FinishedAt,
			&f.UploadName, &f.UploadSize); err != nil {
			return nil, err
		}
		f.State = State(state)
		out = append(out, f)
	}
	return out, rows.Err()
}

// MarkUploaded stores the token returned for a file's bytes, along with the name
// and size actually sent - which differ from the source when the file was
// converted on the way. Tokens expire a day after issue, so the timestamp is
// kept alongside and checked before use.
func (j *Journal) MarkUploaded(id, token, uploadName string, uploadSize int64) error {
	_, err := j.db.Exec(`UPDATE files
	    SET state=?, upload_token=?, token_at=?, error='',
	        upload_name=?, upload_size=?,
	        started_at=CASE started_at WHEN 0 THEN ? ELSE started_at END
	  WHERE onedrive_id=?`,
		string(Uploaded), token, time.Now().Unix(), uploadName, uploadSize,
		time.Now().Unix(), id)
	return err
}

// MarkCreated records the media item the library assigned. This is the only
// state that means "done".
func (j *Journal) MarkCreated(id, mediaItemID string) error {
	_, err := j.db.Exec(`UPDATE files
	    SET state=?, media_item_id=?, finished_at=?, error='', upload_token=''
	  WHERE onedrive_id=?`,
		string(Created), mediaItemID, time.Now().Unix(), id)
	return err
}

// MarkFailed records an attempt that did not work, incrementing the counter the
// retry policy reads.
func (j *Journal) MarkFailed(id, reason string) error {
	_, err := j.db.Exec(`UPDATE files
	    SET state=?, attempts=attempts+1, error=?, upload_token=''
	  WHERE onedrive_id=?`, string(Failed), reason, id)
	return err
}

// NoteBatchFailure records that a batchCreate covering these files did not
// succeed. The upload token is deliberately kept - the bytes are already on
// Google's side and re-sending them would cost both bandwidth and a slice of
// the day's request quota - but the attempt is counted so a permanently failing
// batch eventually drops out of Pending instead of being retried forever.
func (j *Journal) NoteBatchFailure(ids []string, reason string, maxAttempts int) error {
	tx, err := j.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	bump, err := tx.Prepare(`UPDATE files SET attempts = attempts + 1, error = ?
	                          WHERE onedrive_id = ?`)
	if err != nil {
		return err
	}
	exhaust, err := tx.Prepare(`UPDATE files SET state = 'failed', upload_token = ''
	                             WHERE onedrive_id = ? AND attempts >= ?`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := bump.Exec(reason, id); err != nil {
			return err
		}
		if _, err := exhaust.Exec(id, maxAttempts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkSkipped excludes a file from the run with a stated reason.
func (j *Journal) MarkSkipped(id, reason string) error {
	_, err := j.db.Exec(`UPDATE files SET state=?, error=?, finished_at=?
	                      WHERE onedrive_id=?`,
		string(Skipped), reason, time.Now().Unix(), id)
	return err
}

// Requeue returns failed files to the queue, e.g. after the cause was fixed.
func (j *Journal) Requeue(resetAttempts bool) (int64, error) {
	q := `UPDATE files SET state='queued', error='' WHERE state='failed'`
	if resetAttempts {
		q = `UPDATE files SET state='queued', error='', attempts=0 WHERE state='failed'`
	}
	res, err := j.db.Exec(q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// -------------------------------------------------------------------- albums

func (j *Journal) Album(title string) (string, bool, error) {
	var id string
	err := j.db.QueryRow(`SELECT album_id FROM albums WHERE title=?`, title).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return id, err == nil, err
}

func (j *Journal) SaveAlbum(title, albumID string) error {
	_, err := j.db.Exec(`INSERT INTO albums(title,album_id,created_at) VALUES(?,?,?)
	                     ON CONFLICT(title) DO UPDATE SET album_id=excluded.album_id`,
		title, albumID, time.Now().Unix())
	return err
}

// -------------------------------------------------------------------- events

// Event appends an immutable record of something the API told us.
func (j *Journal) Event(kind, oneDriveID, detail string) error {
	_, err := j.db.Exec(`INSERT INTO events(at,onedrive_id,kind,detail) VALUES(?,?,?,?)`,
		time.Now().Unix(), oneDriveID, kind, detail)
	return err
}

// ----------------------------------------------------------------- api quota

// quotaDay is the key under which a day's request spend is recorded.
//
// Google resets the Library API quota at midnight Pacific, so that is the day
// boundary used here. Keying on UTC instead would roll our counter over at 5pm
// Pacific, mid-window: the journal would report a fresh allowance while Google
// still held the old one, and the run would spend the evening collecting 429s.
var quotaZone = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		// No tzdata: fall back to a fixed -08:00, which still puts the rollover
		// far from any hour this is likely to be run at.
		return time.FixedZone("PST", -8*3600)
	}
	return loc
}()

func quotaDay(t time.Time) string { return t.In(quotaZone).Format("2006-01-02") }

// SpendRequests records n API requests against today's allowance and returns the
// new running total.
func (j *Journal) SpendRequests(n int64) (int64, error) {
	day := quotaDay(time.Now())
	if _, err := j.db.Exec(`INSERT INTO api_usage(day,requests) VALUES(?,?)
	                        ON CONFLICT(day) DO UPDATE SET requests = requests + excluded.requests`,
		day, n); err != nil {
		return 0, err
	}
	return j.RequestsToday()
}

// RequestsToday is how much of today's allowance has been spent.
func (j *Journal) RequestsToday() (int64, error) {
	var n int64
	err := j.db.QueryRow(`SELECT COALESCE(requests,0) FROM api_usage WHERE day=?`,
		quotaDay(time.Now())).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// --------------------------------------------------------------------- stats

// Counts is a state histogram with the bytes each state accounts for.
type Counts struct {
	Files map[string]int64
	Bytes map[string]int64
}

func (j *Journal) StateCounts() (Counts, error) {
	return j.histogram(`SELECT state, COUNT(*), COALESCE(SUM(size),0) FROM files GROUP BY state`)
}

func (j *Journal) VerdictCounts() (Counts, error) {
	return j.histogram(`SELECT verdict, COUNT(*), COALESCE(SUM(size),0) FROM census GROUP BY verdict`)
}

func (j *Journal) KindCounts() (Counts, error) {
	return j.histogram(`SELECT kind, COUNT(*), COALESCE(SUM(size),0) FROM census GROUP BY kind`)
}

func (j *Journal) histogram(query string) (Counts, error) {
	c := Counts{Files: map[string]int64{}, Bytes: map[string]int64{}}
	rows, err := j.db.Query(query)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var n, b int64
		if err := rows.Scan(&key, &n, &b); err != nil {
			return c, err
		}
		c.Files[key], c.Bytes[key] = n, b
	}
	return c, rows.Err()
}

// DB exposes the handle for the report package's bespoke queries.
func (j *Journal) DB() *sql.DB { return j.db }
