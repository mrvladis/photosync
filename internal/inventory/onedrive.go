package inventory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OneDriveDB is the personal-account sync database maintained by the macOS
// OneDrive client.
func OneDriveDB() string {
	return filepath.Join(os.Getenv("HOME"),
		"Library/Application Support/OneDrive/settings/Personal/SyncEngineDatabase.db")
}

// OneDriveMount is the root of the personal OneDrive FileProvider mount.
func OneDriveMount() string {
	return filepath.Join(os.Getenv("HOME"), "Library/CloudStorage/OneDrive-Personal")
}

// OneDrive reads every live file under subtree (a path relative to the OneDrive
// root, e.g. "Pictures/Samsung Gallery") from the client's own database.
//
// Note that od_ClientFile_Records carries hash columns, but they are empty in
// practice - on this account 6 of 171,194 rows have a serverHashDigest. Content
// hashing is not an option here, which is why match.Compare works on size.
func OneDrive(subtree, workDir string) ([]Item, error) {
	db, err := snapshot(OneDriveDB(), filepath.Join(workDir, "onedrive"))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	folders, err := oneDriveFolders(db)
	if err != nil {
		return nil, err
	}

	prefix := strings.Trim(subtree, "/")
	rows, err := db.Query(`
		SELECT resourceID, fileName, size, parentResourceID,
		       mediaDateTaken, lastChange, locallyDeleted, serverDeleted
		  FROM od_ClientFile_Records`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var id, name, parent string
		var size, taken, changed sql.NullInt64
		var localDel, serverDel sql.NullInt64
		if err := rows.Scan(&id, &name, &size, &parent, &taken, &changed, &localDel, &serverDel); err != nil {
			return nil, err
		}
		if localDel.Int64 != 0 || serverDel.Int64 != 0 {
			continue
		}
		dir, ok := folders.path(parent)
		if !ok {
			continue
		}
		if dir != prefix && !strings.HasPrefix(dir, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(dir, prefix), "/")
		if rel != "" {
			rel += "/"
		}
		items = append(items, Item{
			Name:     name,
			Size:     size.Int64,
			Path:     rel + name,
			Modified: changed.Int64,
			Taken:    taken.Int64,
			ID:       id,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no files found under OneDrive subtree %q", subtree)
	}
	return items, nil
}

// folderTree resolves OneDrive's parent-pointer folder records into paths.
type folderTree struct {
	parent map[string]string
	name   map[string]string
	cache  map[string]string
}

func oneDriveFolders(db *sql.DB) (*folderTree, error) {
	rows, err := db.Query(`SELECT resourceID, parentResourceID, folderName
	                         FROM od_ClientFolder_Records`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	t := &folderTree{
		parent: map[string]string{},
		name:   map[string]string{},
		cache:  map[string]string{},
	}
	for rows.Next() {
		var id, parent, name string
		if err := rows.Scan(&id, &parent, &name); err != nil {
			return nil, err
		}
		t.parent[id], t.name[id] = parent, name
	}
	return t, rows.Err()
}

// path walks parent pointers to the tree root. It is iterative and bounded so a
// corrupt database with a parent cycle cannot hang the inventory.
func (t *folderTree) path(id string) (string, bool) {
	if p, ok := t.cache[id]; ok {
		return p, true
	}
	if _, ok := t.name[id]; !ok {
		return "", false
	}
	const maxDepth = 128
	var chain []string
	seen := map[string]bool{}
	for cur := id; ; {
		if seen[cur] || len(chain) > maxDepth {
			return "", false
		}
		seen[cur] = true
		chain = append(chain, t.name[cur])
		parent := t.parent[cur]
		// A folder whose parent is not itself a folder record is a tree root.
		if _, isFolder := t.name[parent]; !isFolder {
			break
		}
		if cached, ok := t.cache[parent]; ok {
			chain = append(chain, cached)
			break
		}
		cur = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	out := strings.Join(chain, "/")
	t.cache[id] = out
	return out, true
}
