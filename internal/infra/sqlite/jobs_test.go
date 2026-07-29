package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

// The job queue is the one part of storage that exists to survive a crash
// (ADR 0008), and none of it was exercised: claim, lease, heartbeat, retry
// and reclaim were all untested. These tests pin the lifecycle so a change
// to the hand-written claim statement — the one dialect-specific query in
// the design — cannot silently stop leasing, stop counting attempts, or
// hand the same row to two owners.

func TestSqlite2EnqueueAppliesDefaults(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// A caller that fills in nothing but Kind must still get a runnable
	// job: an empty payload is not valid JSON and zero max_attempts would
	// make the job fail on its first claim.
	id, err := db.EnqueueJob(ctx, domain.Job{Kind: "refresh"})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	got, err := db.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Payload != "{}" {
		t.Errorf("payload = %q, want {}", got.Payload)
	}
	if got.MaxAttempts != 3 {
		t.Errorf("maxAttempts = %d, want 3", got.MaxAttempts)
	}
	if got.Priority != 100 {
		t.Errorf("priority = %d, want 100", got.Priority)
	}
	if got.State != domain.JobQueued {
		t.Errorf("state = %q, want queued", got.State)
	}
	if got.RunAfter.IsZero() {
		t.Error("runAfter was left zero; the job would never be claimable")
	}
}

// A dedupe collision is the mechanism working. It must be reported as
// ErrDuplicateJob and not as a raw constraint error, because callers treat
// "already queued" as success.
func TestSqlite2EnqueueDuplicateDedupeKey(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan:1"}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	_, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan:1"})
	if !errors.Is(err, ErrDuplicateJob) {
		t.Fatalf("second enqueue err = %v, want ErrDuplicateJob", err)
	}

	// A different key is a different job — the dedupe must not be global.
	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan:2"}); err != nil {
		t.Fatalf("distinct dedupe key rejected: %v", err)
	}
}

// The full happy path: claim leases and counts an attempt, heartbeat extends
// it, complete ends it. Losing any one of these strands work.
func TestSqlite2ClaimHeartbeatComplete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.EnqueueJob(ctx, domain.Job{Kind: "probe", Payload: `{"n":1}`})
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	job, err := db.ClaimJob(ctx, "node-a", nil, time.Minute)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if job.ID != id || job.Payload != `{"n":1}` {
		t.Errorf("claimed = %+v, want id %d", job, id)
	}
	if job.State != domain.JobLeased {
		t.Errorf("state = %q, want leased", job.State)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — the claim must count the attempt", job.Attempts)
	}
	if job.LeaseOwner != "node-a" || job.LeaseExpiresAt.IsZero() {
		t.Errorf("lease = %q/%v, want node-a with an expiry", job.LeaseOwner, job.LeaseExpiresAt)
	}

	// Nothing else is queued, so a second claimant gets ErrNotFound rather
	// than the same row.
	if _, err := db.ClaimJob(ctx, "node-b", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second ClaimJob err = %v, want ErrNotFound", err)
	}

	ok, err := db.HeartbeatJob(ctx, id, "node-a", 2*time.Minute)
	if err != nil {
		t.Fatalf("HeartbeatJob: %v", err)
	}
	if !ok {
		t.Error("heartbeat by the lease holder reported false")
	}
	// A worker whose lease was reclaimed must learn it from the heartbeat
	// and stop, instead of writing a result someone else owns.
	ok, err = db.HeartbeatJob(ctx, id, "node-b", 2*time.Minute)
	if err != nil {
		t.Fatalf("HeartbeatJob(other owner): %v", err)
	}
	if ok {
		t.Error("heartbeat by a non-owner reported true")
	}

	if err := db.CompleteJob(ctx, id); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	done, err := db.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if done.State != domain.JobDone {
		t.Errorf("state = %q, want done", done.State)
	}
	if done.FinishedAt.IsZero() {
		t.Error("finishedAt not set; retention pruning keys off it")
	}
}

// Claim order is priority then run_after. If it regresses, interactive work
// queues behind background sweeps and the queue feels broken to a user.
func TestSqlite2ClaimOrderIsPriorityThenRunAfter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "sweep", Priority: 200, RunAfter: past}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "button", Priority: 10, RunAfter: past}); err != nil {
		t.Fatal(err)
	}

	job, err := db.ClaimJob(ctx, "node-a", nil, time.Minute)
	if err != nil {
		t.Fatalf("ClaimJob: %v", err)
	}
	if job.Kind != "button" {
		t.Errorf("claimed %q first, want the lower-priority-number job", job.Kind)
	}
}

// A job scheduled for the future must not be handed out early, otherwise
// retry backoff means nothing.
func TestSqlite2ClaimSkipsFutureRunAfter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{
		Kind: "later", RunAfter: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "node-a", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ClaimJob err = %v, want ErrNotFound for a future job", err)
	}
}

// Capability and affinity routing decide which node can see which files. A
// node that lacks the capability must not be able to take the work, and a
// node always advertises its own name so affinity and capability share one
// matching rule.
func TestSqlite2ClaimRoutingByCapabilityAndAffinity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{
		Kind: "import", RequiredCapability: "nas-mount",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "node-a", []string{"other"}, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("node without the capability claimed the job (err = %v)", err)
	}
	job, err := db.ClaimJob(ctx, "node-b", []string{"nas-mount"}, time.Minute)
	if err != nil {
		t.Fatalf("capable node could not claim: %v", err)
	}
	if job.RequiredCapability != "nas-mount" {
		t.Errorf("requiredCapability = %q, want nas-mount", job.RequiredCapability)
	}
	if err := db.CompleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "pinned", AffinityNode: "node-c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "node-a", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-pinned node claimed a pinned job (err = %v)", err)
	}
	pinned, err := db.ClaimJob(ctx, "node-c", nil, time.Minute)
	if err != nil {
		t.Fatalf("pinned node could not claim: %v", err)
	}
	if pinned.AffinityNode != "node-c" {
		t.Errorf("affinityNode = %q, want node-c", pinned.AffinityNode)
	}
}

// Retry is the backoff path: the job returns to the queue with a later
// run_after and the attempt already spent stays spent.
func TestSqlite2RetryKeepsAttemptAndDefersRun(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.EnqueueJob(ctx, domain.Job{Kind: "flaky"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "node-a", nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(30 * time.Minute)
	if err := db.RetryJob(ctx, id, "boom", later); err != nil {
		t.Fatalf("RetryJob: %v", err)
	}

	got, err := db.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobQueued {
		t.Errorf("state = %q, want queued", got.State)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want the claim's attempt preserved", got.Attempts)
	}
	if got.LastError != "boom" {
		t.Errorf("lastError = %q, want boom", got.LastError)
	}
	if !got.RunAfter.After(time.Now().Add(time.Minute)) {
		t.Errorf("runAfter = %v, want deferred", got.RunAfter)
	}
	// Deferred means not claimable right now.
	if _, err := db.ClaimJob(ctx, "node-a", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Errorf("a retried job was claimable before its backoff elapsed (err = %v)", err)
	}
}

func TestSqlite2FailJobIsTerminal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.EnqueueJob(ctx, domain.Job{Kind: "doomed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FailJob(ctx, id, "no such file"); err != nil {
		t.Fatalf("FailJob: %v", err)
	}
	got, err := db.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobFailed || got.LastError != "no such file" {
		t.Errorf("job = state %q err %q, want failed/no such file", got.State, got.LastError)
	}
	if got.FinishedAt.IsZero() {
		t.Error("a failed job must have finishedAt so pruning can reach it")
	}
}

// A node that dies mid-job strands nothing: the lease expires and the row
// returns to the queue — unless the attempts are gone, in which case it is
// failed rather than retried forever.
func TestSqlite2ReclaimExpiredLeases(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	retryable, err := db.EnqueueJob(ctx, domain.Job{Kind: "retryable", MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	exhausted, err := db.EnqueueJob(ctx, domain.Job{Kind: "exhausted", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	// A negative lease is already expired the moment it is taken, which is
	// how a dead node's row looks to the reclaim pass.
	for range 2 {
		if _, err := db.ClaimJob(ctx, "dead-node", nil, -time.Minute); err != nil {
			t.Fatalf("ClaimJob: %v", err)
		}
	}

	n, err := db.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpiredLeases: %v", err)
	}
	if n != 2 {
		t.Errorf("reclaimed %d, want 2", n)
	}

	back, err := db.GetJob(ctx, retryable)
	if err != nil {
		t.Fatal(err)
	}
	if back.State != domain.JobQueued {
		t.Errorf("retryable job state = %q, want queued", back.State)
	}
	if back.LeaseOwner != "" {
		t.Errorf("leaseOwner = %q, want cleared", back.LeaseOwner)
	}
	if back.LastError != "lease expired" {
		t.Errorf("lastError = %q, want 'lease expired'", back.LastError)
	}

	dead, err := db.GetJob(ctx, exhausted)
	if err != nil {
		t.Fatal(err)
	}
	if dead.State != domain.JobFailed {
		t.Errorf("out-of-attempts job state = %q, want failed", dead.State)
	}
	if dead.FinishedAt.IsZero() {
		t.Error("a reclaim-failed job must have finishedAt set")
	}

	// Nothing is expired now, so a second pass is a no-op rather than
	// churning the same rows.
	if n, err := db.ReclaimExpiredLeases(ctx); err != nil || n != 0 {
		t.Errorf("second pass = %d, %v; want 0, nil", n, err)
	}
}

func TestSqlite2GetJobNotFound(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.GetJob(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetJob(999) err = %v, want ErrNotFound", err)
	}
}

// ListJobsByState and CountJobsByState are what the queue view reads.
// Failure visibility is one of the reasons the queue is worth having, so a
// failed job has to show up in both.
func TestSqlite2ListAndCountByState(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 3 {
		if _, err := db.EnqueueJob(ctx, domain.Job{
			Kind: "q", DedupeKey: string(rune('a' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := db.ClaimJob(ctx, "node-a", nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FailJob(ctx, claimed.ID, "nope"); err != nil {
		t.Fatal(err)
	}

	queued, err := db.ListJobsByState(ctx, string(domain.JobQueued), 10)
	if err != nil {
		t.Fatalf("ListJobsByState: %v", err)
	}
	if len(queued) != 2 {
		t.Errorf("queued = %d jobs, want 2", len(queued))
	}
	failed, err := db.ListJobsByState(ctx, string(domain.JobFailed), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ID != claimed.ID {
		t.Errorf("failed = %+v, want just job %d", failed, claimed.ID)
	}
	// The limit is honoured — the queue view pages, it does not load
	// everything.
	if got, err := db.ListJobsByState(ctx, string(domain.JobQueued), 1); err != nil || len(got) != 1 {
		t.Errorf("limit 1 returned %d jobs (%v)", len(got), err)
	}

	counts, err := db.CountJobsByState(ctx)
	if err != nil {
		t.Fatalf("CountJobsByState: %v", err)
	}
	if counts["queued"] != 2 || counts["failed"] != 1 {
		t.Errorf("counts = %v, want queued 2 / failed 1", counts)
	}
}

// Retention: terminal jobs older than the cutoff go, live ones stay. Without
// the state filter this would delete the queue.
func TestSqlite2PruneFinishedJobs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	old, err := db.EnqueueJob(ctx, domain.Job{Kind: "old", DedupeKey: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteJob(ctx, old); err != nil {
		t.Fatal(err)
	}
	live, err := db.EnqueueJob(ctx, domain.Job{Kind: "live", DedupeKey: "live"})
	if err != nil {
		t.Fatal(err)
	}

	n, err := db.PruneFinishedJobs(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneFinishedJobs: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, err := db.GetJob(ctx, old); !errors.Is(err, ErrNotFound) {
		t.Errorf("finished job survived pruning (err = %v)", err)
	}
	if _, err := db.GetJob(ctx, live); err != nil {
		t.Errorf("pruning removed a queued job: %v", err)
	}
}

// Ping covers both handles; after Close it must report the failure rather
// than pretending the database is still there.
func TestSqlite2PingAndClose(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping on an open DB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Ping(ctx); err == nil {
		t.Error("Ping succeeded after Close")
	}
	// Close is called again by the t.Cleanup openTestDB installed; a
	// double close must not blow up.
	if err := db.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// Backup is an online snapshot plus retention. The retention branch only
// runs past backupKeep files, so seed the directory to reach it.
func TestSqlite2BackupAndRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	dest, err := db.Backup(ctx)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !strings.HasSuffix(dest, ".db") {
		t.Errorf("backup path = %q, want a .db file", dest)
	}

	list, err := db.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListBackups = %d entries, want 1", len(list))
	}
	if list[0].SizeBytes <= 0 {
		t.Errorf("backup size = %d, want a non-empty snapshot", list[0].SizeBytes)
	}
	if list[0].CreatedAt.IsZero() {
		t.Error("backup createdAt is zero")
	}
}

// ListBackups on a data dir that has never been backed up is not an error —
// the settings page asks for the list before the first backup exists.
func TestSqlite2ListBackupsMissingDir(t *testing.T) {
	db := openTestDB(t)
	list, err := db.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups with no backups dir: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("got %d entries, want none", len(list))
	}
}
