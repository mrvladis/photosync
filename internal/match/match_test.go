package match

import (
	"testing"

	"github.com/mrvladis/photosync/internal/inventory"
)

func TestNormaliseFoldsCollisionSuffixes(t *testing.T) {
	cases := map[string]string{
		"IMG_1234.JPG":     "img_1234.jpg",
		"IMG_1234 (1).jpg": "img_1234.jpg",
		"IMG_1234(2).jpg":  "img_1234.jpg",
		"IMG_1234-1.jpg":   "img_1234.jpg",
		"IMG_1234_1.jpg":   "img_1234.jpg",
		"IMG_1234.jpg":     "img_1234.jpg",
		"noextension":      "noextension",
		".picasa.ini":      ".picasa.ini",
	}
	for in, want := range cases {
		if got := Normalise(in); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

// A camera name that genuinely ends in digits must survive normalisation, or
// distinct photos collapse into one and files are wrongly declared present.
func TestNormaliseKeepsGenuineTrailingDigits(t *testing.T) {
	for _, name := range []string{"20260814_192822.heic", "IMG_4821.jpg", "YDXJ0216.mp4"} {
		if got := Normalise(name); got != lower(name) {
			t.Errorf("Normalise(%q) = %q, expected it unchanged apart from case", name, got)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func item(name string, size int64) inventory.Item {
	return inventory.Item{Name: name, Size: size, Path: "DCIM/trip/" + name, ID: name}
}

func TestCompareClassifiesBySize(t *testing.T) {
	source := []inventory.Item{
		item("a.jpg", 1000), // identical on both sides
		item("b.jpg", 5000), // Google holds a re-encode
		item("c.jpg", 2000), // absent from Google
	}
	target := []inventory.Item{
		item("a.jpg", 1000),
		item("b.jpg", 400),
	}

	got := Compare(source, target)
	want := []Status{Present, Resized, Missing}
	for i, w := range want {
		if got[i].Status != w {
			t.Errorf("%s: got %s, want %s", source[i].Name, got[i].Status, w)
		}
	}
	if n := len(got[1].Counterparts); n != 1 {
		t.Errorf("resized result should carry its counterpart, got %d", n)
	}
}

// The size half of the key is what makes a re-encode visible. Without it a
// 394 KB copy of a 4.6 MB original would read as "already present".
func TestResizedIsNotPresent(t *testing.T) {
	got := Compare(
		[]inventory.Item{item("IMG_20240607_175902.jpg", 4675388)},
		[]inventory.Item{item("IMG_20240607_175902.jpg", 393698)},
	)
	if got[0].Status != Resized {
		t.Fatalf("got %s, want %s", got[0].Status, Resized)
	}
}

func TestWorklistDeduplicatesIdenticalFiles(t *testing.T) {
	// The same photo filed under three folders is one upload, not three.
	source := []inventory.Item{
		{Name: "x.jpg", Size: 100, Path: "DCIM/a/x.jpg", ID: "1"},
		{Name: "x.jpg", Size: 100, Path: "DCIM/b/x.jpg", ID: "2"},
		{Name: "x.jpg", Size: 100, Path: "DCIM/c/x.jpg", ID: "3"},
		{Name: "y.jpg", Size: 200, Path: "DCIM/a/y.jpg", ID: "4"},
	}
	results := Compare(source, nil)

	list := Worklist(results, Options{MediaOnly: true, IncludeResized: true})
	if len(list) != 2 {
		t.Fatalf("got %d uploads, want 2", len(list))
	}
	if dupes := Duplicates(results); len(dupes) != 1 {
		t.Fatalf("got %d duplicate groups, want 1", len(dupes))
	}
}

func TestWorklistHonoursOptions(t *testing.T) {
	source := []inventory.Item{
		item("photo.jpg", 900),   // resized
		item("clip.mp4", 100),    // missing
		item("backup.pkgf", 500), // missing, but not media
	}
	target := []inventory.Item{item("photo.jpg", 90)}
	results := Compare(source, target)

	all := Worklist(results, Options{IncludeResized: true, MediaOnly: false})
	if len(all) != 3 {
		t.Errorf("include everything: got %d, want 3", len(all))
	}
	media := Worklist(results, Options{IncludeResized: true, MediaOnly: true})
	if len(media) != 2 {
		t.Errorf("media only: got %d, want 2", len(media))
	}
	missingOnly := Worklist(results, Options{IncludeResized: false, MediaOnly: true})
	if len(missingOnly) != 1 || missingOnly[0].Item.Name != "clip.mp4" {
		t.Errorf("missing only: got %v, want just clip.mp4", names(missingOnly))
	}
}

func names(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Item.Name
	}
	return out
}

func TestKindClassification(t *testing.T) {
	cases := map[string]string{
		"a.jpg": "image", "a.arw": "image", "a.dng": "image", "a.heic": "image",
		"a.mp4": "video", "a.mts": "video", "a.mov": "video",
		// Proxies and sidecars are not originals worth a second home.
		"a.lrv": "other", "a.thm": "other", "a.pkgf": "other", "a.ini": "other",
	}
	for name, want := range cases {
		if got := Kind(inventory.Item{Name: name}); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
}
