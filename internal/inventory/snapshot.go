package inventory

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// snapshot copies a live client database, plus its -wal and -shm sidecars, into
// dir and opens the copy.
//
// Two reasons for the copy. We must not hold a lock on a database the running
// client is writing to, and the sidecars matter: DriveFS routinely carries a
// write-ahead log larger than the database itself, so a naive read of the main
// file alone would return a stale tree. The copy is opened read-write on
// purpose - it is ours to modify, and SQLite has to be able to replay the WAL
// to show us the client's current state.
func snapshot(src, dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, filepath.Base(src))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(src+suffix, dst+suffix); err != nil {
			if os.IsNotExist(err) && suffix != "" {
				continue // sidecars are optional
			}
			return nil, fmt.Errorf("snapshot %s%s: %w", src, suffix, err)
		}
	}
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open snapshot %s: %w", dst, err)
	}
	return db, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
