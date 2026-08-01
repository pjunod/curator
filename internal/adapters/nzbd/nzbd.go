// Package nzbd implements ports.DownloadClient against nzbd's NATIVE
// `/api/v1` surface rather than its NZBGet-compat shim.
//
// Monarr can already talk to nzbd as an "nzbget" client, and that keeps
// working. This adapter exists because three things live only on the
// native API, and all three are what make the handoff visible instead of
// inferred (nzbd/docs/INTEGRATION_PLAN.md):
//
//   - **Add-time params**, so every download carries Monarr's transfer id
//     from the moment it enters the queue. Grep one id across three apps
//     and the whole story comes back.
//   - **A history cursor** (`?since_seq=`), so catching up after downtime
//     fetches what is new instead of re-reading the last 100 rows.
//   - **Post-processing events** on the SSE stream, which the push
//     subscriber consumes — completion arrives with the final directory
//     attached instead of being discovered by a 30 s poll.
//
// Compat exposes none of them. This adapter is additive: nothing here
// changes what an existing `nzbget`-typed client does.
package nzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/adapters/httpx"
	"github.com/pjunod/monarr/internal/buildinfo"
	"github.com/pjunod/monarr/internal/ports"
)

// Client speaks nzbd's native API at {url}/api/v1.
type Client struct {
	cfg  ports.ClientConfig
	http *http.Client
}

// New returns a Client.
func New(cfg ports.ClientConfig) *Client {
	cfg.URL = ports.NormalizeServiceURL(cfg.URL, ports.DefaultPortFor(cfg.Type))
	return &Client{cfg: cfg, http: httpx.NewClient(30 * time.Second)}
}

// clientHeader identifies Monarr to nzbd's client registry, which is what
// makes "is Monarr actually talking to this daemon?" answerable from
// nzbd's own UI instead of from its logs.
func clientHeader() string { return "monarr/" + buildinfo.Version }

// authorize applies the configured credential.
//
// nzbd accepts either `Bearer <token>` or `Basic user:pass` on the same
// header. A token is the better credential (no username to get wrong, and
// it is what nzbd's own docs hand out), so an empty username means the
// password field holds a token. Both are supported because the settings
// form cannot know which one an operator has.
func (c *Client) authorize(req *http.Request) {
	switch {
	case c.cfg.Username == "" && c.cfg.Password != "":
		req.Header.Set("Authorization", "Bearer "+c.cfg.Password)
	case c.cfg.Username != "":
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(c.cfg.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("X-Nzbd-Client", clientHeader())
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("nzbd: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("nzbd: authentication rejected (401)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// nzbd answers errors as {"error": "..."} — surface its words, not
		// a status code the operator then has to go look up.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("nzbd: %s (%d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("nzbd: unexpected status %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("nzbd: bad response: %w", err)
		}
	}
	return nil
}

// Add implements ports.DownloadClient.
func (c *Client) Add(ctx context.Context, downloadURL, category string) (ports.Handle, error) {
	return c.AddWithOptions(ctx, downloadURL, category, ports.AddOptions{})
}

// AddTagged implements ports.TaggedAdder: the same add, with the release name
// and Monarr's transfer id set at admit time.
//
// The param rides nzbd's existing plumbing from the queue into history and
// into the compat `Parameters` array, so the id is visible in nzbd's UI,
// on its completion event, and in its history row — without Monarr writing
// it twice or nzbd learning anything about Monarr.
func (c *Client) AddTagged(ctx context.Context, downloadURL, category, name, transfer string) (ports.Handle, error) {
	return c.AddWithOptions(ctx, downloadURL, category, ports.AddOptions{Name: name, Transfer: transfer})
}

// AddWithOptions implements ports.ConfiguredAdder and puts Monarr's release
// metadata and effective scheduler priority on the job at admission time.
func (c *Client) AddWithOptions(ctx context.Context, downloadURL, category string, options ports.AddOptions) (ports.Handle, error) {
	q := url.Values{}
	q.Set("url", downloadURL)
	q.Set("priority", strconv.Itoa(options.Priority))
	if category != "" {
		q.Set("category", category)
	}
	if options.Name != "" {
		q.Set("name", options.Name)
	}
	if options.Transfer != "" {
		// A JSON object of string→string. Keys starting with `*` are
		// nzbd's internal namespace and are refused with a 422; ours never
		// is, but the encoding is exact either way.
		params, err := json.Marshal(map[string]string{TransferParam: options.Transfer})
		if err != nil {
			return "", err
		}
		q.Set("params", string(params))
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/jobs?"+q.Encode(), &out); err != nil {
		return "", err
	}
	if out.ID <= 0 {
		return "", fmt.Errorf("nzbd: add rejected (id %d)", out.ID)
	}
	return ports.Handle(strconv.FormatInt(out.ID, 10)), nil
}

// TransferParam is the job-parameter key Monarr's transfer id travels
// under. Named in nzbd's integration contract §3.1; changing it breaks
// the ability to grep one id across both applications.
const TransferParam = "monarr-transfer"

// job is one row of `GET /api/v1/jobs`.
type job struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	Status          json.RawMessage `json:"status"`
	SizeBytes       int64           `json:"size_bytes"`
	DownloadedBytes int64           `json:"downloaded_bytes"`
}

// state maps a job's status to Monarr's lifecycle, and reports the
// post-processing stage when there is one.
//
// nzbd serializes `status` as a bare string for simple states and as
// `{"post":{"stage":"unpack"}}` while post-processing runs, so this
// decodes both shapes rather than assuming either.
func (j job) state() (ports.DownloadState, string, string) {
	var simple string
	if json.Unmarshal(j.Status, &simple) == nil {
		switch simple {
		case "queued", "paused", "fetching":
			return ports.StateQueued, "", ""
		case "post_queued":
			// Downloading is finished; post-processing has not started.
			// Still "downloading" to Monarr: the payload is not importable
			// until PP says so, and a premature 'downloaded' is what makes
			// an import find a half-unpacked folder.
			return ports.StateDownloading, "post-processing queued", ""
		case "completed":
			return ports.StateCompleted, "", ""
		case "failed", "deleted":
			return ports.StateFailed, simple, ""
		default:
			return ports.StateDownloading, "", ""
		}
	}
	var post struct {
		Post struct {
			Stage string `json:"stage"`
		} `json:"post"`
	}
	if json.Unmarshal(j.Status, &post) == nil && post.Post.Stage != "" {
		return ports.StateDownloading,
			"post-processing: " + strings.ReplaceAll(post.Post.Stage, "_", " "),
			post.Post.Stage
	}
	return ports.StateDownloading, "", ""
}

// historyEntry is one row of `GET /api/v1/history`.
type historyEntry struct {
	Job      int64  `json:"job"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	FinalDir string `json:"final_dir"`
	Seq      int64  `json:"seq"`
}

// Statuses implements ports.DownloadClient: the live queue plus recent
// history, which is where a finished job lives (nzbd retires completed
// jobs out of the queue, NZBGet-style).
func (c *Client) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	var queue struct {
		Jobs []job `json:"jobs"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/jobs", &queue); err != nil {
		return nil, err
	}
	out := make([]ports.DownloadStatus, 0, len(queue.Jobs)+16)
	for _, j := range queue.Jobs {
		st, msg, stage := j.state()
		progress := 0.0
		if j.SizeBytes > 0 {
			progress = float64(j.DownloadedBytes) / float64(j.SizeBytes)
		}
		out = append(out, ports.DownloadStatus{
			Handle:   ports.Handle(strconv.FormatInt(j.ID, 10)),
			Name:     j.Name,
			State:    st,
			Progress: progress,
			Message:  msg,
			Stage:    stage,
		})
	}

	var hist struct {
		Entries []historyEntry `json:"entries"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/history?limit=100", &hist); err != nil {
		return nil, err
	}
	for _, h := range hist.Entries {
		out = append(out, statusOfHistory(h))
	}
	return out, nil
}

// statusOfHistory maps one history row onto Monarr's lifecycle.
//
// The status string is nzbd's verbatim: `SUCCESS` for a clean
// post-processing run, `PAR_FAILURE`/`UNPACK_FAILURE`/`SCRIPT_FAILURE`
// for a failed one, and the pre-PP terminals `FAILURE/HEALTH` and
// `FAILURE/FETCH` for downloads that never reached post-processing. They
// all share a SUCCESS/FAILURE prefix, so matching the prefix is stable
// against nzbd adding a new terminal reason — matching the exact strings
// would not be.
func statusOfHistory(h historyEntry) ports.DownloadStatus {
	st := ports.DownloadStatus{
		Handle:   ports.Handle(strconv.FormatInt(h.Job, 10)),
		Name:     h.Name,
		Progress: 1,
		SavePath: h.FinalDir,
	}
	switch {
	case strings.HasPrefix(h.Status, "SUCCESS"):
		st.State = ports.StateCompleted
	case h.Status == "DELETED" || strings.HasPrefix(h.Status, "DELETED"):
		// Someone removed the job in nzbd.
		//
		// This was StateFailed+Blameless, which fixed the blocklist and left
		// the louder half in place: the failure path also runs an automatic
		// re-search, so deleting a job in nzbd made Monarr grab another copy
		// of the same film within seconds. Blameless was never going to stop
		// that — it only ever guarded the blocklist.
		st.State = ports.StateRemoved
		st.Message = "removed in nzbd"
		st.Blameless = true
	default:
		st.State = ports.StateFailed
		st.Message = h.Status
	}
	return st
}

// Remove implements ports.DownloadClient.
//
// A job can be in the queue or already retired into history, and the
// caller does not know which, so this tries the queue and falls back —
// the same two-step the compat adapter does, against the native routes.
func (c *Client) Remove(ctx context.Context, h ports.Handle, deleteData bool) error {
	id, err := strconv.ParseInt(string(h), 10, 64)
	if err != nil {
		return fmt.Errorf("nzbd: bad handle %q", h)
	}
	action := "delete"
	if deleteData {
		action = "delete-files"
	}
	queueErr := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/jobs/%d/actions/%s", id, action), nil)
	if queueErr == nil {
		return nil
	}
	if err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/history/%d/actions/%s", id, action), nil); err != nil {
		// Report the queue error too: "no such job" from both routes and
		// "connection refused" from both are very different problems, and
		// only reporting the second hides which one happened.
		return fmt.Errorf("nzbd: remove failed (queue: %v; history: %w)", queueErr, err)
	}
	return nil
}

// statusDto is the slice of `GET /api/v1/status` Monarr reads: the version
// (which is also how Test knows it is talking to nzbd) and the queue-hold
// reasons.
type statusDto struct {
	Version        string `json:"version"`
	DownloadPaused bool   `json:"download_paused"`
	DiskLow        bool   `json:"disk_low"`
	QuotaReached   bool   `json:"quota_reached"`
	BlockedServers []int  `json:"blocked_servers"`
	HealthAbort    bool   `json:"health_abort"`
}

// Test implements ports.DownloadClient.
func (c *Client) Test(ctx context.Context) error {
	_, err := c.status(ctx)
	return err
}

// Capacity implements ports.CapacityReporter: why nzbd is not downloading,
// when it is up and downloading nothing.
//
// One small GET, on the health check's own timer. The plan hoped to reuse
// the snapshot the queue poll already fetches — but that poll reads /jobs
// and /history, not /status, and threading a status fetch through the
// queue-refresh path to save one request a minute would couple two things
// that have no other reason to know about each other.
func (c *Client) Capacity(ctx context.Context) (ports.Capacity, error) {
	st, err := c.status(ctx)
	if err != nil {
		return ports.Capacity{}, err
	}
	return ports.Capacity{
		Version:        st.Version,
		DiskLow:        st.DiskLow,
		QuotaReached:   st.QuotaReached,
		BlockedServers: len(st.BlockedServers),
		HealthAbort:    st.HealthAbort,
		Paused:         st.DownloadPaused,
	}, nil
}

func (c *Client) status(ctx context.Context) (statusDto, error) {
	var st statusDto
	if err := c.do(ctx, http.MethodGet, "/api/v1/status", &st); err != nil {
		return st, err
	}
	if st.Version == "" {
		// A 200 from something that is not nzbd (a reverse proxy's index
		// page, another app on that port) would otherwise pass the test
		// and fail mysteriously at the first grab.
		return st, fmt.Errorf("nzbd: %s answered, but not like nzbd (no version in /api/v1/status)", c.cfg.URL)
	}
	return st, nil
}
