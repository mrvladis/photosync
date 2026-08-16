// Package transfer moves the work-list into the Google Photos library.
//
// The shape of the run is dictated by three constraints. OneDrive files are
// placeholders, so every upload begins with a download - measured at roughly
// 12 MB/s across 24 parallel readers on this machine, which makes hydration,
// not upload, the thing to parallelise. The Photos API wants media-item
// creation serialised per user and capped at 50 items a call. And a run this
// long will be interrupted, so nothing may be held only in memory.
//
// So: fan out to hydrate and upload a chunk of files concurrently, then make
// one batchCreate call for that chunk, then commit. An interruption costs at
// most one chunk of upload work, never a completed file.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mrvladis/photosync/internal/convert"
	"github.com/mrvladis/photosync/internal/gphotos"
	"github.com/mrvladis/photosync/internal/journal"
)

// Options configures a run.
type Options struct {
	// SourceRoot is the OneDrive mount path the work-list's paths hang off.
	SourceRoot string
	// Workers is how many files are hydrated and uploaded concurrently.
	Workers int
	// MaxAttempts is how many times a file may fail before it is left alone.
	MaxAttempts int
	// FreeSpaceFloor pauses the run when the volume holding the OneDrive cache
	// drops below this many bytes free. Hydration fills that cache and macOS
	// gives us no supported way to evict it, so the guard is what stops a long
	// run from filling the disk.
	FreeSpaceFloor int64
	// Limit stops after this many files (0 = no limit). Used by the pilot.
	Limit int
	// Extensions, when set, restricts the run to those file extensions.
	Extensions []string
	// ConvertRAW renders camera RAW to HEIF before upload rather than sending
	// the RAW itself. The OneDrive original is never modified.
	ConvertRAW bool
	// ConvertQuality is the HEIF quality, 1–100. 90 is visually transparent at
	// roughly half the bytes; 100 produces files larger than the RAW.
	ConvertQuality int
	// ScratchDir holds converted files between rendering and upload. Each is
	// deleted as soon as its bytes are accepted.
	ScratchDir string
	// DailyRequestBudget caps API requests per day. The Library API allows
	// 10,000 per project per day with byte uploads counted in, which is the
	// binding constraint on a 68,000-file transfer - not bandwidth. Leave a
	// margin below the true ceiling so a retry storm cannot overshoot it.
	DailyRequestBudget int64
	// DryRun does everything except contact the API.
	DryRun bool
	// DescribeWithPath writes each file's OneDrive path into the media item's
	// description, where Google Photos shows it under the photo. Off by
	// default: it is visible on every item, and changing one's mind later means
	// an edit request per photo - a week of quota for an archive this size.
	DescribeWithPath bool
	// Progress receives human-readable status lines.
	Progress func(string)
}

func (o *Options) setDefaults() {
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 4
	}
	if o.FreeSpaceFloor <= 0 {
		o.FreeSpaceFloor = 50 << 30 // 50 GB
	}
	if o.DailyRequestBudget <= 0 {
		o.DailyRequestBudget = gphotos.DailyRequestQuota - 500
	}
	if o.ConvertQuality <= 0 {
		o.ConvertQuality = 90
	}
	if o.ScratchDir == "" {
		o.ScratchDir = filepath.Join(os.TempDir(), "photosync-convert")
	}
	if o.Progress == nil {
		o.Progress = func(string) {}
	}
}

// Stats summarises what a run did.
type Stats struct {
	Created   int
	Failed    int
	Skipped   int
	BytesSent int64
	// Converted counts RAW files rendered to HEIF, and BytesSaved the
	// difference between what they would have cost and what they did.
	Converted  int
	BytesSaved int64
	Started    time.Time
	Finished   time.Time
	PausedFor  time.Duration
	RateLimits int
	// Requests is the API spend this run added to today's allowance.
	Requests int64
	// QuotaExhausted is set when the run stopped because the day's allowance
	// ran out rather than because the work-list did.
	QuotaExhausted bool
	// SpentToday is the running daily total after this run.
	SpentToday int64
}

// Runner executes a transfer.
type Runner struct {
	j      *journal.Journal
	client *gphotos.Client
	opt    Options
}

func New(j *journal.Journal, client *gphotos.Client, opt Options) *Runner {
	opt.setDefaults()
	return &Runner{j: j, client: client, opt: opt}
}

// Run works the queue until it is empty, the limit is reached, or ctx is done.
//
// The return value is named so the deferred finish stamp lands in the value the
// caller receives rather than in a copy made at the point of return.
func (r *Runner) Run(ctx context.Context) (stats Stats, err error) {
	stats = Stats{Started: time.Now()}
	defer func() { stats.Finished = time.Now() }()

	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			return stats, nil // an interrupted run is a normal outcome
		}
		if r.opt.Limit > 0 && processed >= r.opt.Limit {
			return stats, nil
		}

		waited, err := r.awaitDiskSpace(ctx)
		stats.PausedFor += waited
		if err != nil {
			return stats, err
		}

		batchSize := gphotos.MaxBatch
		if r.opt.Limit > 0 && r.opt.Limit-processed < batchSize {
			batchSize = r.opt.Limit - processed
		}
		chunk, err := r.nextChunk(batchSize)
		if err != nil {
			return stats, err
		}
		if len(chunk) == 0 {
			return stats, nil
		}

		// Refuse to start a chunk we cannot finish inside today's allowance.
		// Stopping cleanly leaves the work-list intact for tomorrow; pressing on
		// would spend the remainder on 429s and half-finished uploads.
		cost := chunkCost(chunk)
		spent, err := r.j.RequestsToday()
		if err != nil {
			return stats, err
		}
		stats.SpentToday = spent
		if !r.opt.DryRun && spent+cost > r.opt.DailyRequestBudget {
			stats.QuotaExhausted = true
			return stats, nil
		}
		processed += len(chunk)

		if err := r.processChunk(ctx, chunk, &stats); err != nil {
			return stats, err
		}
		if !r.opt.DryRun {
			used := r.client.Requests() - stats.Requests
			stats.Requests += used
			if total, err := r.j.SpendRequests(used); err == nil {
				stats.SpentToday = total
			}
		}
	}
}

// chunkCost is the API spend a chunk will incur: one upload sequence per file
// plus the single batchCreate that turns the tokens into library entries.
func chunkCost(chunk []journal.File) int64 {
	cost := int64(1)
	for _, f := range chunk {
		cost += gphotos.EstimateRequests(f.Size)
	}
	return cost
}

// nextChunk takes the next run of pending files that share an album, since a
// batchCreate call can only file items into one album.
func (r *Runner) nextChunk(max int) ([]journal.File, error) {
	pending, err := r.j.Pending(max, r.opt.MaxAttempts, r.opt.Extensions)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		// Nothing queued: give previously failed files another go.
		pending, err = r.j.Retryable(r.opt.MaxAttempts, max)
		if err != nil || len(pending) == 0 {
			return nil, err
		}
	}
	album := pending[0].Album
	var chunk []journal.File
	for _, f := range pending {
		if f.Album != album {
			break
		}
		chunk = append(chunk, f)
	}
	return chunk, nil
}

func (r *Runner) processChunk(ctx context.Context, chunk []journal.File, stats *Stats) error {
	albumID, err := r.albumID(ctx, chunk[0].Album)
	if err != nil {
		// Losing the album is not worth losing the photos over: file them into
		// the library unalbumed and say so.
		r.opt.Progress(fmt.Sprintf("album %q unavailable (%v) - uploading without one", chunk[0].Album, err))
		_ = r.j.Event("album_failed", "", fmt.Sprintf("%s: %v", chunk[0].Album, err))
		albumID = ""
	}

	// What was sent may differ from what is on disk: a converted RAW goes up as
	// a HEIF, under a .heic name, at a fraction of the size.
	type uploaded struct {
		file  journal.File
		token string
		name  string
		size  int64
	}

	var (
		mu    sync.Mutex // guards ready and the counters below
		ready []uploaded
		sem   = make(chan struct{}, r.opt.Workers)
		wg    sync.WaitGroup
	)

	for _, f := range chunk {
		// A token from an earlier interrupted run is reusable while it is fresh.
		// A token from an interrupted run is reusable while it is fresh - but
		// only if it names the same bytes we would send now. A token minted
		// before RAW conversion was enabled points at the RAW, so reusing it
		// would file the RAW in the library and silently ignore the setting.
		if f.State == journal.Uploaded && f.UploadToken != "" &&
			time.Since(time.Unix(f.TokenAt, 0)) < gphotos.TokenTTL &&
			!r.staleToken(f) {
			name, size := f.UploadName, f.UploadSize
			if name == "" {
				name, size = f.Name, f.Size
			}
			ready = append(ready, uploaded{f, f.UploadToken, name, size})
			continue
		}
		if reason := r.sizeVeto(f); reason != "" {
			if !r.opt.DryRun {
				if err := r.j.MarkSkipped(f.OneDriveID, reason); err != nil {
					return err
				}
			}
			stats.Skipped++
			continue
		}

		wg.Add(1)
		go func(f journal.File) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			token, sent, err := r.uploadOne(ctx, f)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				if !r.opt.DryRun {
					_ = r.j.MarkFailed(f.OneDriveID, err.Error())
					_ = r.j.Event("upload_failed", f.OneDriveID, err.Error())
				}
				mu.Lock()
				stats.Failed++
				if gphotos.IsRateLimit(err) {
					stats.RateLimits++
				}
				mu.Unlock()
				return
			}
			if !r.opt.DryRun {
				if err := r.j.MarkUploaded(f.OneDriveID, token, sent.name, sent.size); err != nil {
					r.opt.Progress("journal write failed: " + err.Error())
					return
				}
			}
			mu.Lock()
			ready = append(ready, uploaded{f, token, sent.name, sent.size})
			stats.BytesSent += sent.size
			if sent.converted {
				stats.Converted++
				stats.BytesSaved += f.Size - sent.size
			}
			mu.Unlock()
		}(f)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil
	}
	if len(ready) == 0 {
		return nil
	}

	items := make([]gphotos.NewMediaItem, 0, len(ready))
	for _, u := range ready {
		item := gphotos.NewMediaItem{FileName: u.name, UploadToken: u.token}
		if r.opt.DescribeWithPath {
			item.Description = u.file.Path
		}
		items = append(items, item)
	}

	results, err := r.createWithRetry(ctx, albumID, items, stats)
	if err != nil {
		// The bytes survive: the tokens stay in the journal, so a later pass
		// retries only the batchCreate. Counting the attempt is what stops that
		// retry from becoming an endless loop.
		ids := make([]string, 0, len(ready))
		for _, u := range ready {
			ids = append(ids, u.file.OneDriveID)
			_ = r.j.Event("batch_failed", u.file.OneDriveID, err.Error())
		}
		if !r.opt.DryRun {
			if noteErr := r.j.NoteBatchFailure(ids, err.Error(), r.opt.MaxAttempts); noteErr != nil {
				return noteErr
			}
		}
		r.opt.Progress("batchCreate failed, will retry later: " + err.Error())
		return nil
	}

	byName := map[string][]uploaded{}
	for _, u := range ready {
		byName[u.name] = append(byName[u.name], u)
	}
	created := 0
	for i, res := range results {
		// Results come back in request order; fall back to name if the API
		// ever reorders them.
		u := ready[i]
		if res.FileName != "" && res.FileName != u.name {
			if cands := byName[res.FileName]; len(cands) > 0 {
				u = cands[0]
			}
		}
		if res.OK {
			if !r.opt.DryRun {
				if err := r.j.MarkCreated(u.file.OneDriveID, res.MediaItemID); err != nil {
					return err
				}
				_ = r.j.Event("created", u.file.OneDriveID, res.MediaItemID)
			}
			stats.Created++
			created++
		} else {
			status := res.Status
			if status == "" {
				status = "library rejected the item without a message"
			}
			if !r.opt.DryRun {
				if err := r.j.MarkFailed(u.file.OneDriveID, status); err != nil {
					return err
				}
				_ = r.j.Event("create_rejected", u.file.OneDriveID, status)
			}
			stats.Failed++
		}
	}

	r.opt.Progress(fmt.Sprintf("%-52s %2d/%d ok   (total %s files, %s)",
		trimAlbum(chunk[0].Album), created, len(chunk),
		humanCount(int64(stats.Created)), humanBytes(stats.BytesSent)))
	return nil
}

// createWithRetry performs the one serialised call per chunk. The API asks for
// no parallel writes per user and a 30-second floor after a 429, so this is
// deliberately the slow, careful part of the pipeline.
func (r *Runner) createWithRetry(ctx context.Context, albumID string, items []gphotos.NewMediaItem, stats *Stats) ([]gphotos.CreateResult, error) {
	var lastErr error
	for attempt := 0; attempt < r.opt.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := gphotos.Backoff(attempt, gphotos.IsRateLimit(lastErr))
			r.opt.Progress(fmt.Sprintf("batchCreate retry %d in %s (%v)", attempt, delay.Round(time.Second), lastErr))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if r.opt.DryRun {
			out := make([]gphotos.CreateResult, len(items))
			for i, it := range items {
				out[i] = gphotos.CreateResult{FileName: it.FileName, MediaItemID: "dry-run", OK: true}
			}
			return out, nil
		}
		results, err := r.client.MediaItems(ctx, albumID, items)
		if err == nil {
			return results, nil
		}
		if gphotos.IsRateLimit(err) {
			stats.RateLimits++
		}
		lastErr = err
		if !gphotos.Retryable(err) {
			break
		}
	}
	return nil, lastErr
}

// sent describes the bytes that actually went to the library, which differ from
// the source file when a RAW was rendered to HEIF on the way.
type sent struct {
	name      string
	size      int64
	converted bool
}

// uploadOne hydrates a file from OneDrive, optionally converts it, and sends it.
func (r *Runner) uploadOne(ctx context.Context, f journal.File) (string, sent, error) {
	path := filepath.Join(r.opt.SourceRoot, filepath.FromSlash(f.Path))
	st, err := os.Stat(path)
	if err != nil {
		return "", sent{}, fmt.Errorf("not readable in OneDrive: %w", err)
	}
	if st.Size() != f.Size {
		return "", sent{}, fmt.Errorf("size changed since analysis (%d → %d); re-run analyse", f.Size, st.Size())
	}

	out := sent{name: f.Name, size: f.Size}
	if r.opt.DryRun {
		if r.opt.ConvertRAW && convert.IsRAW(f.Name) {
			out.name, out.converted = convert.HEIFName(f.Name), true
		}
		return "dry-run-token", out, nil
	}

	// Render RAW to HEIF. A failure here is not fatal: sending the original RAW
	// is a worse outcome than intended but far better than losing the photo, so
	// it falls back and records why.
	if r.opt.ConvertRAW && convert.IsRAW(f.Name) {
		res, err := convert.ToHEIF(ctx, path, r.opt.ScratchDir, r.opt.ConvertQuality)
		if err != nil {
			_ = r.j.Event("convert_failed", f.OneDriveID, err.Error())
			r.opt.Progress(fmt.Sprintf("could not convert %s (%v) - uploading the RAW instead", f.Name, err))
		} else {
			defer os.Remove(res.Path)
			path = res.Path
			out = sent{name: res.Name, size: res.Size, converted: true}
			_ = r.j.Event("converted", f.OneDriveID,
				fmt.Sprintf("%d → %d bytes at q%d", res.OriginalSize, res.Size, r.opt.ConvertQuality))
		}
	}

	var lastErr error
	for attempt := 0; attempt < r.opt.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := gphotos.Backoff(attempt, gphotos.IsRateLimit(lastErr))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", out, ctx.Err()
			}
		}
		token, err := r.client.Upload(ctx, path, out.name, mimeType(out.name), out.size)
		if err == nil {
			return token, out, nil
		}
		lastErr = err
		if !gphotos.Retryable(err) {
			break
		}
	}
	return "", out, lastErr
}

// staleToken reports whether a stored upload token was produced under different
// conversion settings than the ones now in force, and so must be discarded.
func (r *Runner) staleToken(f journal.File) bool {
	if !convert.IsRAW(f.Name) {
		return false
	}
	wasConverted := f.UploadName != "" && f.UploadName != f.Name
	return wasConverted != r.opt.ConvertRAW
}

// sizeVeto returns a reason if the library will not accept the file's size.
func (r *Runner) sizeVeto(f journal.File) string {
	switch f.Kind {
	case "image":
		if f.Size > gphotos.MaxPhotoBytes {
			return fmt.Sprintf("photo is %s, over the library's 200 MB limit", humanBytes(f.Size))
		}
	case "video":
		if f.Size > gphotos.MaxVideoBytes {
			return fmt.Sprintf("video is %s, over the library's 10 GB limit", humanBytes(f.Size))
		}
	}
	return ""
}

func (r *Runner) albumID(ctx context.Context, title string) (string, error) {
	if title == "" {
		return "", nil
	}
	if id, ok, err := r.j.Album(title); err != nil {
		return "", err
	} else if ok {
		return id, nil
	}
	if r.opt.DryRun {
		return "dry-run-album", nil
	}
	id, err := r.client.CreateAlbum(ctx, title)
	if err != nil {
		return "", err
	}
	return id, r.j.SaveAlbum(title, id)
}

// awaitDiskSpace blocks while the cache volume is too full to keep hydrating.
func (r *Runner) awaitDiskSpace(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	warned := false
	for {
		free, err := freeBytes(r.opt.SourceRoot)
		if err != nil || free >= r.opt.FreeSpaceFloor {
			if warned {
				r.opt.Progress("disk space recovered, resuming")
			}
			return time.Since(start), nil
		}
		if !warned {
			r.opt.Progress(fmt.Sprintf(
				"paused: only %s free, below the %s floor. Free space in Finder "+
					"(right-click OneDrive files → Remove Download) and the run continues on its own.",
				humanBytes(free), humanBytes(r.opt.FreeSpaceFloor)))
			warned = true
		}
		select {
		case <-time.After(time.Minute):
		case <-ctx.Done():
			return time.Since(start), nil
		}
	}
}

func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// mimeType maps an extension to a content type, covering the camera formats the
// standard table misses.
func mimeType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := extraMIME[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

var extraMIME = map[string]string{
	".heic": "image/heic",
	".heif": "image/heif",
	".arw":  "image/x-sony-arw",
	".cr2":  "image/x-canon-cr2",
	".cr3":  "image/x-canon-cr3",
	".nef":  "image/x-nikon-nef",
	".dng":  "image/x-adobe-dng",
	".orf":  "image/x-olympus-orf",
	".raf":  "image/x-fuji-raf",
	".rw2":  "image/x-panasonic-rw2",
	".mts":  "video/mp2t",
	".m2ts": "video/mp2t",
	".mpg":  "video/mpeg",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".avi":  "video/x-msvideo",
	".wmv":  "video/x-ms-wmv",
	".mkv":  "video/x-matroska",
}

// trimAlbum keeps progress lines aligned by eliding the middle of long album
// names rather than letting them push the counts off the edge of the terminal.
func trimAlbum(s string) string {
	const width = 52
	if len(s) <= width {
		return s
	}
	return s[:20] + "…" + s[len(s)-(width-21):]
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
