// Command photosync mirrors a OneDrive photo archive into the Google Photos
// library, and reports precisely what it did.
//
// Usage:
//
//	photosync analyse   compare both sides and build the work-list
//	photosync auth      authorise against the Google Photos API
//	photosync sync      upload the work-list (resumable; safe to interrupt)
//	photosync verify    confirm uploaded items exist in the library
//	photosync prune     delete Drive re-encodes whose originals are safe
//	photosync report    regenerate the HTML report and CSV manifest
//	photosync status    one-line summary of where the run has got to
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mrvladis/photosync/internal/convert"
	"github.com/mrvladis/photosync/internal/gphotos"
	"github.com/mrvladis/photosync/internal/inventory"
	"github.com/mrvladis/photosync/internal/journal"
	"github.com/mrvladis/photosync/internal/match"
	"github.com/mrvladis/photosync/internal/prune"
	"github.com/mrvladis/photosync/internal/report"
)

// config is what to sync and where. Nothing here is baked into the binary:
// which account, which folders, and what to call the albums are all personal to
// whoever is running it, so they come from a config file in the state directory
// (see config.example.json) and can be overridden per-command by flags.
type config struct {
	source       string
	target       string
	account      string
	stateDir     string
	albumPrefix  string
	includeSized bool
	mediaOnly    bool
}

// fileConfig is the on-disk form. Pointers so an absent key is distinguishable
// from a deliberate empty string and leaves the built-in default alone.
type fileConfig struct {
	Source         *string `json:"source"`
	Target         *string `json:"target"`
	Account        *string `json:"account"`
	AlbumPrefix    *string `json:"album_prefix"`
	IncludeResized *bool   `json:"include_resized"`
	MediaOnly      *bool   `json:"media_only"`
}

func (c *config) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.source, "source", "Pictures",
		"OneDrive subtree to mirror, relative to the OneDrive root")
	fs.StringVar(&c.target, "target", "Google Photos",
		"Drive subtree to compare against, relative to My Drive")
	fs.StringVar(&c.account, "account", "",
		"Google account whose Drive mount holds the target subtree")
	fs.StringVar(&c.stateDir, "state", defaultStateDir(),
		"directory for the journal, credentials and reports")
	fs.StringVar(&c.albumPrefix, "album-prefix", "",
		"prefix for created album names")
	fs.BoolVar(&c.includeSized, "include-resized", true,
		"upload originals for photos Google only holds as smaller re-encodes")
	fs.BoolVar(&c.mediaOnly, "media-only", true,
		"transfer photos and video only, excluding documents and backup blobs")
}

// load applies <state>/config.json underneath any flag the user actually typed.
// Precedence is flags, then the config file, then the built-in defaults - so a
// one-off `--source` overrides the file without editing it.
func (c *config) load(fs *flag.FlagSet) error {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	raw, err := os.ReadFile(filepath.Join(c.stateDir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return fmt.Errorf("parse %s/config.json: %w", c.stateDir, err)
	}
	set := func(name string, dst *string, src *string) {
		if src != nil && !explicit[name] {
			*dst = *src
		}
	}
	setBool := func(name string, dst *bool, src *bool) {
		if src != nil && !explicit[name] {
			*dst = *src
		}
	}
	set("source", &c.source, fc.Source)
	set("target", &c.target, fc.Target)
	set("account", &c.account, fc.Account)
	set("album-prefix", &c.albumPrefix, fc.AlbumPrefix)
	setBool("include-resized", &c.includeSized, fc.IncludeResized)
	setBool("media-only", &c.mediaOnly, fc.MediaOnly)
	return nil
}

// requireAccount fails early with an actionable message rather than letting a
// missing account surface later as a confusing "path not found".
func (c config) requireAccount() error {
	if c.account != "" {
		return nil
	}
	return fmt.Errorf("no Google account set - put one in %s/config.json "+
		"(see config.example.json) or pass -account", c.stateDir)
}

func (c config) journalPath() string  { return filepath.Join(c.stateDir, "photosync.db") }
func (c config) secretPath() string   { return filepath.Join(c.stateDir, "client_secret.json") }
func (c config) tokenPath() string    { return filepath.Join(c.stateDir, "token.json") }
func (c config) reportPath() string   { return filepath.Join(c.stateDir, "report.html") }
func (c config) manifestPath() string { return filepath.Join(c.stateDir, "manifest.csv") }

func defaultStateDir() string {
	return filepath.Join(os.Getenv("HOME"), ".photosync")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "analyse", "analyze":
		err = cmdAnalyse(ctx, os.Args[2:])
	case "auth":
		err = cmdAuth(ctx, os.Args[2:])
	case "sync", "upload":
		err = cmdSync(ctx, os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "prune":
		err = cmdPrune(ctx, os.Args[2:])
	case "report":
		err = cmdReport(ctx, os.Args[2:])
	case "status":
		err = cmdStatus(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`photosync - mirror a OneDrive photo archive into Google Photos

  analyse   compare both sides and build the work-list
  auth      authorise against the Google Photos API
  sync      upload the work-list; resumable, safe to interrupt
  verify    read uploaded items back from the library
  prune     delete Drive re-encodes whose originals are safely uploaded
  report    regenerate the HTML report and CSV manifest
  status    where the run has got to

Run a command with -h to see its flags.
Configuration lives in ~/.photosync/config.json; see config.example.json.
`)
}

// ------------------------------------------------------------------ analyse

func cmdAnalyse(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("analyse", flag.ExitOnError)
	var c config
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.load(fs); err != nil {
		return err
	}

	work := filepath.Join(c.stateDir, "snapshots")
	defer os.RemoveAll(work)

	fmt.Printf("Reading the OneDrive sync database (no files are downloaded)…\n")
	source, err := inventory.OneDrive(c.source, work)
	if err != nil {
		return err
	}
	fmt.Printf("  %s files, %s under %s\n",
		humanCount(int64(len(source))), humanBytes(inventory.TotalBytes(source)), c.source)

	fmt.Printf("Reading the Google Drive metadata cache…\n")
	target, account, err := inventory.Drive(c.target, work)
	if err != nil {
		return err
	}
	fmt.Printf("  %s files, %s under %s (DriveFS account %s)\n",
		humanCount(int64(len(target))), humanBytes(inventory.TotalBytes(target)), c.target, account)

	results := match.Compare(source, target)
	opts := match.Options{IncludeResized: c.includeSized, MediaOnly: c.mediaOnly}
	list := match.Worklist(results, opts)

	byStatus := map[match.Status]struct {
		n int64
		b int64
	}{}
	for _, r := range results {
		e := byStatus[r.Status]
		e.n++
		e.b += r.Item.Size
		byStatus[r.Status] = e
	}
	fmt.Println()
	for _, s := range []match.Status{match.Present, match.Resized, match.Missing} {
		e := byStatus[s]
		fmt.Printf("  %-8s %9s  %10s\n", s, humanCount(e.n), humanBytes(e.b))
	}

	var listBytes int64
	for _, r := range list {
		listBytes += r.Item.Size
	}
	fmt.Printf("\n  work-list: %s files, %s\n", humanCount(int64(len(list))), humanBytes(listBytes))

	// The Library API's 10,000-requests-a-day ceiling, not bandwidth, is what
	// sets the calendar for a transfer this size - so say so up front.
	var requests int64
	for _, r := range list {
		requests += gphotos.EstimateRequests(r.Item.Size)
	}
	requests += int64(len(list))/gphotos.MaxBatch + 1
	days := (requests + gphotos.DailyRequestQuota - 1) / gphotos.DailyRequestQuota
	fmt.Printf("  about %s API requests, so at least %d days at the Library API's "+
		"10,000-a-day limit\n", humanCount(requests), days)
	if dupes := match.Duplicates(results); len(dupes) > 0 {
		var extra int64
		for _, g := range dupes {
			extra += int64(len(g) - 1)
		}
		fmt.Printf("  (%s duplicate copies collapse into these uploads)\n", humanCount(extra))
	}

	j, err := journal.Open(c.journalPath())
	if err != nil {
		return err
	}
	defer j.Close()

	albumFor := albumNamer(c.albumPrefix)
	if err := j.Record(results, list, albumFor); err != nil {
		return err
	}
	for k, v := range map[string]any{
		"source":          c.source,
		"target":          c.target,
		"account":         c.account,
		"album_prefix":    c.albumPrefix,
		"include_resized": c.includeSized,
		"media_only":      c.mediaOnly,
		"drive_files":     len(target),
		"drive_bytes":     inventory.TotalBytes(target),
		"analysed_at":     time.Now().Unix(),
	} {
		if err := j.SetMeta(k, v); err != nil {
			return err
		}
	}

	fmt.Printf("\nJournal written to %s\n", j.Path())
	return writeReport(j, c)
}

// albumNamer maps a OneDrive folder to the album its files land in. The folder
// path is used rather than the leaf name, because leaf names repeat across the
// archive and an album per unique folder keeps the original structure legible.
func albumNamer(prefix string) func(inventory.Item) string {
	return func(it inventory.Item) string {
		dir := it.Dir()
		if dir == "" {
			dir = "(root)"
		}
		if prefix != "" {
			return prefix + "/" + dir
		}
		return dir
	}
}

// --------------------------------------------------------------------- auth

func cmdAuth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	var c config
	c.bind(fs)
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.load(fs); err != nil {
		return err
	}

	secret, err := gphotos.LoadClientSecret(c.secretPath())
	if err != nil {
		return fmt.Errorf("%w\n\nCreate an OAuth client in the Google Cloud console "+
			"(APIs & Services → Credentials → OAuth client ID → Desktop app), enable the "+
			"Photos Library API, and save the downloaded JSON to %s", err, c.secretPath())
	}
	auth, err := gphotos.NewAuth(secret, c.tokenPath())
	if err != nil {
		return err
	}
	if err := auth.Authorize(ctx, !*noBrowser); err != nil {
		return err
	}
	fmt.Printf("Authorised. Credentials stored in %s\n", c.tokenPath())
	return nil
}

// --------------------------------------------------------------------- sync

func cmdSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	var c config
	c.bind(fs)
	workers := fs.Int("workers", 8, "concurrent hydrate+upload workers")
	limit := fs.Int("limit", 0, "stop after this many files (0 = the whole work-list)")
	dryRun := fs.Bool("dry-run", false, "do everything except contact the API")
	floorGB := fs.Int64("free-space-floor-gb", 50, "pause when the disk drops below this many GB free")
	retry := fs.Bool("retry-failed", false, "return failed files to the queue before starting")
	convertRAW := fs.Bool("convert-raw", true,
		"render camera RAW (ARW, CR2, DNG…) to HEIF before upload; the OneDrive original is untouched")
	quality := fs.Int("quality", 90, "HEIF quality for converted RAW, 1-100")
	only := fs.String("ext", "",
		"restrict the run to these comma-separated extensions, e.g. arw,cr2,dng")
	describe := fs.Bool("describe-with-path", false,
		"write each file's OneDrive path into the photo's description in Google Photos")
	budget := fs.Int64("daily-requests", gphotos.DailyRequestQuota-500,
		"API requests to spend per day (the Library API allows 10,000 per project per day)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.load(fs); err != nil {
		return err
	}

	j, err := journal.Open(c.journalPath())
	if err != nil {
		return err
	}
	defer j.Close()

	if *retry {
		n, err := j.Requeue(true)
		if err != nil {
			return err
		}
		fmt.Printf("Requeued %d failed files\n", n)
	}

	var client *gphotos.Client
	if !*dryRun {
		secret, err := gphotos.LoadClientSecret(c.secretPath())
		if err != nil {
			return fmt.Errorf("%w - run `photosync auth` first", err)
		}
		auth, err := gphotos.NewAuth(secret, c.tokenPath())
		if err != nil {
			return err
		}
		if !auth.HasToken() {
			return fmt.Errorf("not authorised - run `photosync auth` first")
		}
		client = gphotos.New(auth)
	}

	var exts []string
	if *only != "" {
		exts = strings.Split(*only, ",")
	}
	if *convertRAW && !convert.Available() {
		return fmt.Errorf("RAW conversion needs `sips`, which is missing; re-run with -convert-raw=false")
	}
	runner := newRunner(j, client, c, *workers, *limit, *dryRun, *describe, *convertRAW,
		*floorGB, *budget, *quality, exts)
	fmt.Printf("Transferring to the Google Photos library. Interrupt at any time - "+
		"progress is journalled in %s and `sync` resumes where it stopped.\n\n", j.Path())

	stats, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	elapsed := stats.Finished.Sub(stats.Started)
	fmt.Printf("\n%s uploaded, %d failed, %d skipped, %s sent in %s",
		humanCount(int64(stats.Created)), stats.Failed, stats.Skipped,
		humanBytes(stats.BytesSent), elapsed.Round(time.Second))
	if elapsed > 0 && stats.BytesSent > 0 {
		fmt.Printf(" (%s/s)", humanBytes(int64(float64(stats.BytesSent)/elapsed.Seconds())))
	}
	fmt.Println()
	if stats.Converted > 0 {
		fmt.Printf("Converted %s RAW files to HEIF, saving %s.\n",
			humanCount(int64(stats.Converted)), humanBytes(stats.BytesSaved))
	}
	if stats.RateLimits > 0 {
		fmt.Printf("Rate limited %d times; the run backed off and continued.\n", stats.RateLimits)
	}
	if stats.PausedFor > 0 {
		fmt.Printf("Paused %s waiting for disk space.\n", stats.PausedFor.Round(time.Second))
	}
	if !*dryRun {
		fmt.Printf("API requests: %s this run, %s of today's %s budget spent.\n",
			humanCount(stats.Requests), humanCount(stats.SpentToday), humanCount(*budget))
	}
	if stats.QuotaExhausted {
		fmt.Printf("\nStopped: today's API allowance is spent. Google resets it daily - " +
			"run `photosync sync` again tomorrow and it continues from here.\n")
	}
	return writeReport(j, c)
}

// ------------------------------------------------------------------- verify

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var c config
	c.bind(fs)
	sample := fs.Int("sample", 200,
		"how many uploaded items to read back (0 = all - one request each, "+
			"so verifying a full archive costs days of quota)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.load(fs); err != nil {
		return err
	}

	j, err := journal.Open(c.journalPath())
	if err != nil {
		return err
	}
	defer j.Close()

	secret, err := gphotos.LoadClientSecret(c.secretPath())
	if err != nil {
		return err
	}
	auth, err := gphotos.NewAuth(secret, c.tokenPath())
	if err != nil {
		return err
	}
	client := gphotos.New(auth)

	q := `SELECT onedrive_id, name, media_item_id FROM files
	       WHERE state='created' AND media_item_id != '' ORDER BY RANDOM()`
	if *sample > 0 {
		q += fmt.Sprintf(" LIMIT %d", *sample)
	}
	rows, err := j.DB().Query(q)
	if err != nil {
		return err
	}
	type check struct{ id, name, media string }
	var checks []check
	for rows.Next() {
		var ck check
		if err := rows.Scan(&ck.id, &ck.name, &ck.media); err != nil {
			rows.Close()
			return err
		}
		checks = append(checks, ck)
	}
	rows.Close()

	if len(checks) == 0 {
		fmt.Println("Nothing uploaded yet - nothing to verify.")
		return nil
	}

	// Each read-back is an API request drawn from the same daily allowance the
	// transfer needs, so it is budgeted and recorded exactly like an upload.
	spent, err := j.RequestsToday()
	if err != nil {
		return err
	}
	remaining := int(gphotos.DailyRequestQuota - 500 - spent)
	if remaining <= 0 {
		return fmt.Errorf("today's API allowance is already spent (%s requests); "+
			"verify tomorrow", humanCount(spent))
	}
	if len(checks) > remaining {
		fmt.Printf("Only %s requests left in today's allowance - verifying that many of %d.\n",
			humanCount(int64(remaining)), len(checks))
		checks = checks[:remaining]
	}
	fmt.Printf("Reading back %d media items from the library…\n", len(checks))
	before := client.Requests()
	defer func() {
		if _, err := j.SpendRequests(client.Requests() - before); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not record API spend: "+err.Error())
		}
	}()

	var ok, missing int
	for _, ck := range checks {
		if ctx.Err() != nil {
			break
		}
		item, err := client.MediaItem(ctx, ck.media)
		if err != nil || item["id"] == nil {
			missing++
			_ = j.Event("verify_missing", ck.id, fmt.Sprint(err))
			fmt.Printf("  missing: %s (%s)\n", ck.name, ck.media)
			continue
		}
		ok++
	}
	fmt.Printf("\n%d of %d confirmed present in the library", ok, len(checks))
	if missing > 0 {
		fmt.Printf("; %d could not be read back", missing)
	}
	fmt.Println(".")
	return nil
}

// -------------------------------------------------------------------- prune

func cmdPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	var c config
	c.bind(fs)
	apply := fs.Bool("apply", false, "actually delete; without this the plan is only shown")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.load(fs); err != nil {
		return err
	}

	j, err := journal.Open(c.journalPath())
	if err != nil {
		return err
	}
	defer j.Close()

	plan, err := prune.Plan(j)
	if err != nil {
		return err
	}
	if err := prune.Save(j, plan); err != nil {
		return err
	}

	var del, review int
	var delBytes, reviewBytes int64
	for _, p := range plan {
		if p.Decision == prune.Delete {
			del++
			delBytes += p.DriveSize
		} else {
			review++
			reviewBytes += p.DriveSize
		}
	}
	fmt.Printf("Compressed Drive copies considered: %s\n", humanCount(int64(len(plan))))
	fmt.Printf("  safe to delete:  %s  (%s)\n", humanCount(int64(del)), humanBytes(delBytes))
	fmt.Printf("  held for review: %s  (%s)\n", humanCount(int64(review)), humanBytes(reviewBytes))

	if !*apply {
		fmt.Printf("\nNothing deleted. Re-run with -apply to delete the %s safe copies.\n", humanCount(int64(del)))
		fmt.Println("Deletions go to Drive's trash and stay recoverable for 30 days.")
		return writeReport(j, c)
	}
	if del == 0 {
		fmt.Println("\nNothing meets the deletion criteria.")
		return writeReport(j, c)
	}

	if err := c.requireAccount(); err != nil {
		return err
	}
	root := inventory.DriveMount(c.account)
	n, freed, err := prune.Execute(j, root, c.target, false, func(s string) { fmt.Println("  " + s) })
	if err != nil {
		return err
	}
	fmt.Printf("\nDeleted %s files, freeing %s. They are in Drive's trash for 30 days.\n",
		humanCount(int64(n)), humanBytes(freed))
	return writeReport(j, c)
}

// ------------------------------------------------------------- report/status

func cmdReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	var c config
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.load(fs); err != nil {
		return err
	}
	j, err := journal.Open(c.journalPath())
	if err != nil {
		return err
	}
	defer j.Close()
	return writeReport(j, c)
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var c config
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.load(fs); err != nil {
		return err
	}
	j, err := journal.Open(c.journalPath())
	if err != nil {
		return err
	}
	defer j.Close()

	verdicts, err := j.VerdictCounts()
	if err != nil {
		return err
	}
	states, err := j.StateCounts()
	if err != nil {
		return err
	}
	fmt.Println("Archive:")
	for _, k := range sortedKeys(verdicts.Files) {
		fmt.Printf("  %-9s %9s  %10s\n", k, humanCount(verdicts.Files[k]), humanBytes(verdicts.Bytes[k]))
	}
	if len(states.Files) == 0 {
		fmt.Println("\nNo transfer started yet.")
		return nil
	}
	fmt.Println("\nTransfer:")
	var done, total, doneBytes, totalBytes int64
	for _, k := range sortedKeys(states.Files) {
		fmt.Printf("  %-9s %9s  %10s\n", k, humanCount(states.Files[k]), humanBytes(states.Bytes[k]))
		total += states.Files[k]
		totalBytes += states.Bytes[k]
		if k == string(journal.Created) {
			done, doneBytes = states.Files[k], states.Bytes[k]
		}
	}
	if total > 0 {
		fmt.Printf("\n  %.1f%% complete by file, %.1f%% by bytes\n",
			100*float64(done)/float64(total), 100*float64(doneBytes)/float64(totalBytes))
	}
	return nil
}

func writeReport(j *journal.Journal, c config) error {
	var driveFiles, driveBytes int64
	_ = j.Meta("drive_files", &driveFiles)
	_ = j.Meta("drive_bytes", &driveBytes)

	n, err := report.Manifest(j, c.manifestPath())
	if err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	data, err := report.Build(j, c.source, c.target, c.albumPrefix, driveFiles, driveBytes)
	if err != nil {
		return err
	}
	data.ManifestPath = c.manifestPath()
	if err := report.Write(data, c.reportPath()); err != nil {
		return err
	}
	fmt.Printf("\nReport:   %s\nManifest: %s  (%s rows)\n",
		c.reportPath(), c.manifestPath(), humanCount(int64(n)))
	return nil
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func humanCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
