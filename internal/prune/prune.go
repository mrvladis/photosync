// Package prune removes the low-resolution copies Google kept, but only once
// the original is demonstrably safe in the library.
//
// This is the only destructive thing photosync does, and the matching that
// justifies it is name-based, so the bar is deliberately high. A Drive
// re-encode might be the last surviving copy of a photo that has since left
// OneDrive; deleting that on a name collision would lose it for good. Anything
// short of an unambiguous one-to-one match is written to a review list for a
// human to look at instead of being deleted.
package prune

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mrvladis/photosync/internal/journal"
	"github.com/mrvladis/photosync/internal/match"
)

// Decision is what the safety rules concluded about one Drive file.
type Decision string

const (
	// Delete: unambiguous derivative whose original is confirmed in the library.
	Delete Decision = "delete"
	// Review: a human should look - ambiguous name, or larger than the
	// "original", or the original never made it to the library.
	Review Decision = "review"
)

// Candidate is one Drive file considered for deletion.
type Candidate struct {
	DriveID    string
	DrivePath  string
	DriveSize  int64
	OneDriveID string
	SourcePath string
	SourceSize int64
	Decision   Decision
	Reason     string
	DeletedAt  int64
}

// Plan classifies every Drive counterpart of a transferred original.
//
// Four conditions must all hold before a file is queued for deletion:
// exactly one OneDrive original carries that name, exactly one Drive file
// carries it, the Drive copy is strictly smaller than the original, and the
// original's media item exists in the library.
func Plan(j *journal.Journal) ([]Candidate, error) {
	db := j.DB()

	// Names are ambiguous if more than one file on either side shares them.
	sourceNames, err := nameCounts(db, `SELECT name FROM census`)
	if err != nil {
		return nil, err
	}
	driveNames, err := nameCounts(db, `SELECT drive_path FROM counterparts`)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT c.onedrive_id, c.drive_id, c.drive_path, c.drive_size,
		       s.path, s.size, f.state, COALESCE(f.media_item_id,'')
		  FROM counterparts c
		  JOIN census s ON s.onedrive_id = c.onedrive_id
		  LEFT JOIN files f ON f.onedrive_id = c.onedrive_id
		 WHERE s.verdict = 'resized'
		 ORDER BY c.drive_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var state, mediaItem sql.NullString
		if err := rows.Scan(&c.OneDriveID, &c.DriveID, &c.DrivePath, &c.DriveSize,
			&c.SourcePath, &c.SourceSize, &state, &mediaItem); err != nil {
			return nil, err
		}
		c.Decision, c.Reason = classify(c, sourceNames, driveNames, state.String, mediaItem.String)
		out = append(out, c)
	}
	return out, rows.Err()
}

func classify(c Candidate, sourceNames, driveNames map[string]int, state, mediaItem string) (Decision, string) {
	key := match.Normalise(filepath.Base(c.DrivePath))
	switch {
	case journal.State(state) != journal.Created || mediaItem == "":
		return Review, "the original is not confirmed in the Photos library yet"
	case sourceNames[key] != 1:
		return Review, fmt.Sprintf("%d OneDrive files share this name - cannot tell which original this copy belongs to", sourceNames[key])
	case driveNames[key] != 1:
		return Review, fmt.Sprintf("%d Drive files share this name", driveNames[key])
	case c.DriveSize >= c.SourceSize:
		return Review, "the Drive copy is not smaller than the OneDrive file, so it is not a re-encode"
	default:
		return Delete, fmt.Sprintf("re-encode of %s (%d → %d bytes); original is media item %s",
			c.SourcePath, c.SourceSize, c.DriveSize, mediaItem)
	}
}

func nameCounts(db *sql.DB, query string) (map[string]int, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		counts[match.Normalise(filepath.Base(p))]++
	}
	return counts, rows.Err()
}

// Save records the plan so the report can show it and so Execute has something
// to act on. Rows already marked deleted are left alone.
func Save(j *journal.Journal, plan []Candidate) error {
	tx, err := j.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
		INSERT INTO prune (drive_id,drive_path,drive_size,onedrive_id,decision,reason)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(drive_id) DO UPDATE SET
		    decision=excluded.decision, reason=excluded.reason
		 WHERE prune.deleted_at = 0`)
	if err != nil {
		return err
	}
	for _, c := range plan {
		if _, err := stmt.Exec(c.DriveID, c.DrivePath, c.DriveSize, c.OneDriveID,
			string(c.Decision), c.Reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Execute deletes the approved candidates through the Drive mount, which moves
// them to Drive's trash rather than erasing them - they stay recoverable for
// 30 days. driveRoot is the mount path the recorded paths hang off.
func Execute(j *journal.Journal, driveRoot, subtree string, dryRun bool, progress func(string)) (deleted int, freed int64, err error) {
	rows, err := j.DB().Query(`SELECT drive_id, drive_path, drive_size FROM prune
	                            WHERE decision='delete' AND deleted_at=0`)
	if err != nil {
		return 0, 0, err
	}
	type target struct {
		id   string
		path string
		size int64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.path, &t.size); err != nil {
			rows.Close()
			return 0, 0, err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, t := range targets {
		full := filepath.Join(driveRoot, filepath.FromSlash(subtree), filepath.FromSlash(t.path))
		if dryRun {
			progress("would delete " + full)
			deleted++
			freed += t.size
			continue
		}
		if err := os.Remove(full); err != nil {
			if os.IsNotExist(err) {
				// Already gone: record it rather than failing the pass.
				_ = j.Event("prune_absent", "", full)
				if _, err := j.DB().Exec(`UPDATE prune SET deleted_at=? WHERE drive_id=?`,
					time.Now().Unix(), t.id); err != nil {
					return deleted, freed, err
				}
				continue
			}
			_ = j.Event("prune_failed", "", fmt.Sprintf("%s: %v", full, err))
			progress(fmt.Sprintf("could not delete %s: %v", full, err))
			continue
		}
		if _, err := j.DB().Exec(`UPDATE prune SET deleted_at=? WHERE drive_id=?`,
			time.Now().Unix(), t.id); err != nil {
			return deleted, freed, err
		}
		_ = j.Event("pruned", t.id, full)
		deleted++
		freed += t.size
	}
	return deleted, freed, nil
}
