// Package match decides whether a OneDrive file already exists on the Google
// side, and at what fidelity.
//
// No content hash is available on both sides. OneDrive's sync database leaves
// its hash columns empty, DriveFS buries an MD5 inside an opaque protobuf, and
// computing either locally would mean downloading 618 GB of placeholder files.
// Identity is therefore (normalised name, exact byte size).
//
// For a photo archive that pairing is strong: two different photographs sharing
// both a camera filename and an exact byte count is vanishingly unlikely. More
// to the point, the size half is what catches the case this tool exists for -
// a re-encoded copy carries the original's name at a fraction of its bytes, and
// is not the original.
package match

import (
	"regexp"
	"strings"

	"github.com/mrvladis/photosync/internal/inventory"
)

// Status is the verdict for one source file.
type Status string

const (
	// Present: same name and same byte size on the Google side. Nothing to do.
	Present Status = "present"
	// Resized: same name, different size. Google holds a derivative - the old
	// "High quality" re-encode - rather than the original.
	Resized Status = "resized"
	// Missing: no counterpart by name at all.
	Missing Status = "missing"
)

// Drive appends " (1)", " (2)" … on a name collision, and Google Photos exports
// use "-1" and "_1" variants. Fold those away before comparing, so a copy that
// was merely renamed is not reported as a missing original.
var collisionSuffix = regexp.MustCompile(`(?:[ _-]?\(\d{1,2}\)|[-_]\d{1,2})$`)

// Normalise folds a filename to its comparison key.
func Normalise(name string) string {
	name = strings.ToLower(name)
	stem, ext := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		stem, ext = name[:dot], name[dot:]
	}
	return collisionSuffix.ReplaceAllString(stem, "") + ext
}

// Result pairs a source item with its verdict and whatever the Google side holds
// under the same name.
type Result struct {
	Item         inventory.Item
	Status       Status
	Counterparts []inventory.Item
}

// Key is the deduplication key: files sharing it are byte-for-byte
// interchangeable as far as this tool can tell, so one upload serves them all.
type Key struct {
	Name string
	Size int64
}

func (r Result) Key() Key { return Key{Normalise(r.Item.Name), r.Item.Size} }

// Kind classifies an item for reporting and for the media-only filter.
func Kind(it inventory.Item) string {
	switch {
	case imageExts[it.Ext()]:
		return "image"
	case videoExts[it.Ext()]:
		return "video"
	default:
		return "other"
	}
}

// IsMedia reports whether an item is a photo or video original.
func IsMedia(it inventory.Item) bool { return Kind(it) != "other" }

var imageExts = set(
	"jpg", "jpeg", "png", "heic", "heif", "gif", "bmp", "tif", "tiff", "webp", "avif",
	"arw", "cr2", "cr3", "nef", "dng", "orf", "raf", "rw2", "srw", "pef",
)

var videoExts = set(
	"mp4", "mov", "m4v", "avi", "mts", "m2ts", "m2t", "mpg", "mpeg", "wmv",
	"3gp", "3g2", "mkv", "asf", "divx", "mod", "mmv", "tod",
)

// Deliberately absent: .lrv and .thm, the low-resolution proxy and thumbnail a
// camera writes beside the real clip. They are not originals worth a second home.

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// Compare classifies every source item against the target tree.
func Compare(source, target []inventory.Item) []Result {
	byName := make(map[string][]inventory.Item, len(target))
	for _, it := range target {
		k := Normalise(it.Name)
		byName[k] = append(byName[k], it)
	}

	out := make([]Result, 0, len(source))
	for _, it := range source {
		candidates := byName[Normalise(it.Name)]
		switch {
		case len(candidates) == 0:
			out = append(out, Result{Item: it, Status: Missing})
		default:
			var exact []inventory.Item
			for _, c := range candidates {
				if c.Size == it.Size {
					exact = append(exact, c)
				}
			}
			if len(exact) > 0 {
				out = append(out, Result{Item: it, Status: Present, Counterparts: exact})
			} else {
				out = append(out, Result{Item: it, Status: Resized, Counterparts: candidates})
			}
		}
	}
	return out
}

// Options selects what the transfer should actually carry.
type Options struct {
	// IncludeResized uploads originals for files whose only Google copy is a
	// smaller re-encode.
	IncludeResized bool
	// MediaOnly restricts the work-list to photos and video.
	MediaOnly bool
}

// Worklist is the set of files to transfer, deduplicated by Key so a group of
// identical files is uploaded once. Order follows the input for determinism.
func Worklist(results []Result, opt Options) []Result {
	seen := map[Key]bool{}
	var out []Result
	for _, r := range results {
		if r.Status == Present {
			continue
		}
		if r.Status == Resized && !opt.IncludeResized {
			continue
		}
		if opt.MediaOnly && !IsMedia(r.Item) {
			continue
		}
		if k := r.Key(); !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// Duplicates groups source items that share a Key, i.e. the copies a single
// upload will satisfy. Only groups with more than one member are returned.
func Duplicates(results []Result) map[Key][]inventory.Item {
	groups := map[Key][]inventory.Item{}
	for _, r := range results {
		k := r.Key()
		groups[k] = append(groups[k], r.Item)
	}
	for k, v := range groups {
		if len(v) < 2 {
			delete(groups, k)
		}
	}
	return groups
}
