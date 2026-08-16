// Package inventory reads what each cloud client already knows about its own
// files, instead of walking the mounted filesystems.
//
// On macOS both OneDrive and Google Drive present their trees through
// FileProvider mounts whose files are dataless placeholders (blocks=0,
// flags=dataless). Walking those mounts is slow, and *reading* a file hydrates
// it - that is, downloads it. An inventory pass must therefore never open a
// file. Both clients happen to keep a complete local SQLite metadata database,
// so that is what we read: full names, sizes and paths for both trees in well
// under a second, with no network traffic and nothing hydrated.
package inventory

import (
	"path"
	"strings"
)

// Item is one file as described by a cloud client's local metadata database.
type Item struct {
	Name     string // base name as stored by the provider
	Size     int64  // exact byte size
	Path     string // tree-relative, forward slashes
	Modified int64  // unix seconds, 0 if unknown
	Taken    int64  // unix seconds from embedded media metadata, 0 if unknown
	ID       string // provider-side identifier (OneDrive resourceID / Drive file id)
}

// Ext is the lowercased extension without the dot, or "" if there is none.
func (i Item) Ext() string {
	dot := strings.LastIndexByte(i.Name, '.')
	if dot < 0 || dot == len(i.Name)-1 {
		return ""
	}
	return strings.ToLower(i.Name[dot+1:])
}

// Dir is the tree-relative directory holding the item, or "" at the tree root.
func (i Item) Dir() string {
	d := path.Dir(i.Path)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// TotalBytes sums the sizes of a slice of items.
func TotalBytes(items []Item) int64 {
	var n int64
	for _, i := range items {
		n += i.Size
	}
	return n
}
