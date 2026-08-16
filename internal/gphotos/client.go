package gphotos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	apiBase = "https://photoslibrary.googleapis.com/v1"

	// MaxBatch is the API's ceiling on media items per batchCreate call.
	MaxBatch = 50
	// MaxPhotoBytes and MaxVideoBytes are Google Photos' documented per-file
	// caps. Anything larger is skipped with a reason rather than failed.
	MaxPhotoBytes = 200 << 20
	MaxVideoBytes = 10 << 30

	// resumableThreshold is where we stop sending a file in one shot, and
	// chunkSize is how much goes in each request after that.
	//
	// Both are set high because requests, not bytes, are the scarce resource:
	// every chunk costs one of the day's 10,000. A single-request upload has no
	// resume, so the threshold is a trade - at 128 MB it covers every photo and
	// all but the longest videos in one request each, while the handful of files
	// above it still get chunked recovery. Chunks must be a multiple of the
	// granularity the server reports; 64 MB is a multiple of every granularity
	// Google has used, and is re-checked at runtime regardless.
	resumableThreshold = 128 << 20
	chunkSize          = 64 << 20

	// TokenTTL is how long an upload token stays usable. Google documents one
	// day; we treat them as stale well before that so a token is never spent
	// on a batchCreate that will reject it.
	TokenTTL = 20 * time.Hour
)

// Client is a Google Photos Library API client.
//
// It counts every request it makes. The Library API allows 10,000 requests per
// project per day and byte uploads count against that same allowance, so the
// request count - not bandwidth - is what actually paces a large transfer.
type Client struct {
	auth     *Auth
	http     *http.Client
	requests atomic.Int64
}

func New(auth *Auth) *Client {
	return &Client{
		auth: auth,
		http: &http.Client{Timeout: 30 * time.Minute},
	}
}

// Requests is the number of API requests this client has made.
func (c *Client) Requests() int64 { return c.requests.Load() }

// DailyRequestQuota is the documented Library API allowance: 10,000 requests
// per project per day, uploads included.
const DailyRequestQuota = 10_000

// APIError is a non-success response from the API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("photos API HTTP %d: %s", e.Status, truncate(e.Body, 400))
}

// Retryable reports whether another attempt could plausibly succeed. Rate
// limits and server-side faults are worth retrying; a rejected file is not.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Retryable reports whether err is worth another attempt at any level.
func Retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// Transport failures - dropped connections, DNS blips, timeouts - are all
	// transient by nature.
	return err != nil && !errors.Is(err, context.Canceled)
}

func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	tok, err := c.auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	c.requests.Add(1)
	return c.http.Do(req)
}

// EstimateRequests predicts how many API requests a file will cost, so a run can
// stay inside the daily allowance instead of discovering the ceiling by being
// throttled. Small files are one request; large ones are a resumable session
// start plus a request per chunk.
func EstimateRequests(size int64) int64 {
	if size <= resumableThreshold {
		return 1
	}
	chunks := (size + chunkSize - 1) / chunkSize
	return 1 + chunks
}

// readError converts a non-2xx response into an APIError.
func readError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
}

// ---------------------------------------------------------------- byte upload

// Upload sends a file's bytes and returns the upload token that names them.
// The token is not yet a photo - MediaItems turns tokens into library entries.
func (c *Client) Upload(ctx context.Context, path, filename, mime string, size int64) (string, error) {
	if size <= resumableThreshold {
		return c.uploadRaw(ctx, path, filename, mime)
	}
	return c.uploadResumable(ctx, path, filename, mime, size)
}

func (c *Client) uploadRaw(ctx context.Context, path, filename, mime string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/uploads", f)
	if err != nil {
		return "", err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Goog-Upload-Content-Type", mime)
	req.Header.Set("X-Goog-Upload-Protocol", "raw")
	req.Header.Set("X-Goog-Upload-File-Name", filename)

	resp, err := c.do(ctx, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", readError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("upload returned an empty token")
	}
	return token, nil
}

// uploadResumable sends a large file in chunks. Each chunk is retried on its
// own, and the server is asked for its current offset after a failure so a
// resumed upload continues rather than restarts.
func (c *Client) uploadResumable(ctx context.Context, path, filename, mime string, size int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/uploads", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Length", "0")
	req.Header.Set("X-Goog-Upload-Command", "start")
	req.Header.Set("X-Goog-Upload-Content-Type", mime)
	req.Header.Set("X-Goog-Upload-Protocol", "resumable")
	req.Header.Set("X-Goog-Upload-Raw-Size", strconv.FormatInt(size, 10))
	req.Header.Set("X-Goog-Upload-File-Name", filename)

	resp, err := c.do(ctx, req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", readError(resp)
	}
	sessionURL := resp.Header.Get("X-Goog-Upload-URL")
	if sessionURL == "" {
		return "", fmt.Errorf("resumable upload start returned no session URL")
	}
	// Every chunk but the last must be a whole number of granularity units.
	chunk := int64(chunkSize)
	if g, err := strconv.ParseInt(resp.Header.Get("X-Goog-Upload-Chunk-Granularity"), 10, 64); err == nil && g > 0 {
		if rounded := chunk / g * g; rounded > 0 {
			chunk = rounded
		} else {
			chunk = g
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var offset int64
	for offset < size {
		n := chunk
		if remaining := size - offset; remaining < n {
			n = remaining
		}
		last := offset+n >= size

		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
		cmd := "upload"
		if last {
			cmd = "upload, finalize"
		}
		chunkReq, err := http.NewRequestWithContext(ctx, http.MethodPost, sessionURL,
			io.LimitReader(f, n))
		if err != nil {
			return "", err
		}
		chunkReq.ContentLength = n
		chunkReq.Header.Set("X-Goog-Upload-Command", cmd)
		chunkReq.Header.Set("X-Goog-Upload-Offset", strconv.FormatInt(offset, 10))

		chunkResp, err := c.do(ctx, chunkReq)
		if err != nil {
			// Ask the server where it actually got to before deciding what to resend.
			at, qerr := c.queryOffset(ctx, sessionURL)
			if qerr != nil {
				return "", fmt.Errorf("chunk at %d: %w (offset query also failed: %v)", offset, err, qerr)
			}
			offset = at
			continue
		}
		if chunkResp.StatusCode != http.StatusOK {
			err := readError(chunkResp)
			chunkResp.Body.Close()
			return "", err
		}
		if last {
			body, readErr := io.ReadAll(chunkResp.Body)
			chunkResp.Body.Close()
			if readErr != nil {
				return "", readErr
			}
			token := strings.TrimSpace(string(body))
			if token == "" {
				return "", fmt.Errorf("finalised upload returned an empty token")
			}
			return token, nil
		}
		chunkResp.Body.Close()
		offset += n
	}
	return "", fmt.Errorf("resumable upload ended without finalising")
}

func (c *Client) queryOffset(ctx context.Context, sessionURL string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sessionURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Length", "0")
	req.Header.Set("X-Goog-Upload-Command", "query")
	resp, err := c.do(ctx, req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, readError(resp)
	}
	return strconv.ParseInt(resp.Header.Get("X-Goog-Upload-Size-Received"), 10, 64)
}

// -------------------------------------------------------------- media items

// NewMediaItem pairs an upload token with the filename the library should show.
type NewMediaItem struct {
	FileName    string
	UploadToken string
	Description string
}

// CreateResult is the per-item outcome of a batchCreate call.
type CreateResult struct {
	FileName    string
	MediaItemID string
	Status      string // the API's message, e.g. "Success" or a rejection
	OK          bool
}

type batchCreateRequest struct {
	AlbumID       string              `json:"albumId,omitempty"`
	NewMediaItems []newMediaItemInner `json:"newMediaItems"`
}

type newMediaItemInner struct {
	Description     string `json:"description,omitempty"`
	SimpleMediaItem struct {
		FileName    string `json:"fileName"`
		UploadToken string `json:"uploadToken"`
	} `json:"simpleMediaItem"`
}

// MediaItems turns up to MaxBatch upload tokens into library entries, optionally
// filing them in an album.
//
// The API reports success per item, not per call, so a 200 response can still
// contain rejections. Every item's status is returned for the journal to record.
func (c *Client) MediaItems(ctx context.Context, albumID string, items []NewMediaItem) ([]CreateResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > MaxBatch {
		return nil, fmt.Errorf("batch of %d exceeds the API limit of %d", len(items), MaxBatch)
	}
	body := batchCreateRequest{AlbumID: albumID}
	for _, it := range items {
		var inner newMediaItemInner
		inner.Description = it.Description
		inner.SimpleMediaItem.FileName = it.FileName
		inner.SimpleMediaItem.UploadToken = it.UploadToken
		body.NewMediaItems = append(body.NewMediaItems, inner)
	}

	var parsed struct {
		NewMediaItemResults []struct {
			UploadToken string `json:"uploadToken"`
			Status      struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"status"`
			MediaItem struct {
				ID       string `json:"id"`
				Filename string `json:"filename"`
			} `json:"mediaItem"`
		} `json:"newMediaItemResults"`
	}
	if err := c.postJSON(ctx, apiBase+"/mediaItems:batchCreate", body, &parsed); err != nil {
		return nil, err
	}

	byToken := make(map[string]string, len(items))
	for _, it := range items {
		byToken[it.UploadToken] = it.FileName
	}
	out := make([]CreateResult, 0, len(parsed.NewMediaItemResults))
	for _, r := range parsed.NewMediaItemResults {
		name := r.MediaItem.Filename
		if name == "" {
			name = byToken[r.UploadToken]
		}
		// Code 0 with a media item id is the API's way of saying success.
		ok := r.MediaItem.ID != "" && r.Status.Code == 0
		out = append(out, CreateResult{
			FileName:    name,
			MediaItemID: r.MediaItem.ID,
			Status:      r.Status.Message,
			OK:          ok,
		})
	}
	return out, nil
}

// CreateAlbum makes an album this application owns and can add to.
func (c *Client) CreateAlbum(ctx context.Context, title string) (string, error) {
	body := map[string]any{"album": map[string]string{"title": title}}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, apiBase+"/albums", body, &parsed); err != nil {
		return "", err
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("album creation returned no id")
	}
	return parsed.ID, nil
}

// MediaItem reads back one item this application created - the verification
// pass's only window into the library.
func (c *Client) MediaItem(ctx context.Context, id string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/mediaItems/"+id, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	var out map[string]any
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (c *Client) postJSON(ctx context.Context, endpoint string, body, out any) error {
	blob, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(blob))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ------------------------------------------------------------------- retries

// Backoff computes the delay before attempt n (0-based), with jitter so that a
// batch of workers throttled at the same moment does not resume in lockstep.
// Rate limiting gets a longer floor: the API documents a 30-second minimum
// before retrying a 429.
func Backoff(attempt int, rateLimited bool) time.Duration {
	base := time.Second * time.Duration(math.Pow(2, float64(attempt)))
	if rateLimited && base < 30*time.Second {
		base = 30 * time.Second
	}
	if base > 10*time.Minute {
		base = 10 * time.Minute
	}
	jitter := time.Duration(rand.Int63n(int64(base / 4)))
	return base + jitter
}

// IsRateLimit reports whether err is the API asking us to slow down.
func IsRateLimit(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
