package inventory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DriveFSDir holds one subdirectory of client state per signed-in account,
// named by an opaque numeric account id.
func DriveFSDir() string {
	return filepath.Join(os.Getenv("HOME"), "Library/Application Support/Google/DriveFS")
}

// DriveMount is the root of an account's My Drive FileProvider mount.
func DriveMount(account string) string {
	return filepath.Join(os.Getenv("HOME"),
		"Library/CloudStorage/GoogleDrive-"+account, "My Drive")
}

// Drive reads every live file under subtree (a path relative to My Drive) from
// the DriveFS metadata cache.
//
// DriveFS names its account directories by an opaque numeric id that maps to no
// visible account property, so rather than guess we try each one and keep the
// database whose cached tree actually contains the requested path. The account
// argument is used only to locate the mount for later file access.
func Drive(subtree, workDir string) ([]Item, string, error) {
	entries, err := os.ReadDir(DriveFSDir())
	if err != nil {
		return nil, "", fmt.Errorf("read DriveFS state: %w", err)
	}
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() || !isNumeric(e.Name()) {
			continue
		}
		db := filepath.Join(DriveFSDir(), e.Name(), "metadata_sqlite_db")
		if _, err := os.Stat(db); err == nil {
			candidates = append(candidates, db)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("no DriveFS account databases under %s", DriveFSDir())
	}

	parts := splitPath(subtree)
	var lastErr error
	for _, cand := range candidates {
		account := filepath.Base(filepath.Dir(cand))
		db, err := snapshot(cand, filepath.Join(workDir, "drivefs-"+account))
		if err != nil {
			lastErr = err
			continue
		}
		items, err := driveWalk(db, parts)
		db.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return items, account, nil
	}
	return nil, "", fmt.Errorf("%q not found in any DriveFS account cache: %w", subtree, lastErr)
}

type driveRow struct {
	title    string
	size     int64
	isFolder bool
	id       string
	modified int64
}

func driveWalk(db *sql.DB, parts []string) ([]Item, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty subtree")
	}

	children := map[int64][]int64{}
	rows, err := db.Query(`SELECT item_stable_id, parent_stable_id FROM stable_parents`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var child, parent int64
		if err := rows.Scan(&child, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		children[parent] = append(children[parent], child)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	items := map[int64]driveRow{}
	rows, err = db.Query(`
		SELECT stable_id, local_title, file_size, is_folder, trashed,
		       is_tombstone, id, modified_date
		  FROM items`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sid int64
		var title, id sql.NullString
		var size, modified sql.NullInt64
		var isFolder, trashed, tombstone sql.NullInt64
		if err := rows.Scan(&sid, &title, &size, &isFolder, &trashed, &tombstone, &id, &modified); err != nil {
			rows.Close()
			return nil, err
		}
		if trashed.Int64 != 0 || tombstone.Int64 != 0 || !title.Valid {
			continue
		}
		items[sid] = driveRow{
			title:    title.String,
			size:     size.Int64,
			isFolder: isFolder.Int64 != 0,
			id:       id.String,
			modified: modified.Int64 / 1000, // DriveFS stores milliseconds
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Descend the named chain. Drive permits duplicate folder names anywhere,
	// so match the whole path rather than trusting a unique leaf name.
	var frontier []int64
	for sid, r := range items {
		if r.isFolder && r.title == parts[0] {
			frontier = append(frontier, sid)
		}
	}
	for _, name := range parts[1:] {
		var next []int64
		for _, sid := range frontier {
			for _, child := range children[sid] {
				if r, ok := items[child]; ok && r.isFolder && r.title == name {
					next = append(next, child)
				}
			}
		}
		frontier = next
	}
	if len(frontier) == 0 {
		return nil, fmt.Errorf("path %q not present in this account's cache", strings.Join(parts, "/"))
	}

	type frame struct {
		sid    int64
		prefix string
	}
	var out []Item
	for _, root := range frontier {
		stack := []frame{{root, ""}}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, child := range children[f.sid] {
				r, ok := items[child]
				if !ok {
					continue
				}
				rel := r.title
				if f.prefix != "" {
					rel = f.prefix + "/" + r.title
				}
				if r.isFolder {
					stack = append(stack, frame{child, rel})
					continue
				}
				out = append(out, Item{
					Name:     r.title,
					Size:     r.size,
					Path:     rel,
					Modified: r.modified,
					ID:       r.id,
				})
			}
		}
	}
	return out, nil
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
