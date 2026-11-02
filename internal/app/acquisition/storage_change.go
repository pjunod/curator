package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type StorageChangeRequest struct {
	Root             string `json:"root"`
	ExpectedIdentity string `json:"expectedIdentity"`
	ObservedIdentity string `json:"observedIdentity"`
	ReviewToken      string `json:"reviewToken,omitempty"`
}
type StorageChangeReview struct {
	StorageChangeRequest
	ExpectedDescription string `json:"expectedDescription"`
	ObservedDescription string `json:"observedDescription"`
	ExistingPayloads    int    `json:"existingPayloads"`
	ExistingRequests    int    `json:"existingRequests"`
	InFlightRequests    int    `json:"inFlightRequests"`
	ActiveImports       int    `json:"activeImports"`
}
type storageChangePlan struct {
	review                   StorageChangeReview
	generations, orphanPaths []string
	root                     CompletedRoot
}

func displayStorageIdentity(identity string) string {
	parts := strings.Split(identity, "\x00")
	description := strings.Join(parts, " · ")
	for _, part := range parts {
		if strings.HasPrefix(part, "/dev/") {
			description += " (device path; stable UUID unavailable)"
			break
		}
	}
	return description
}

func (s *Service) PreviewCompletedStorageChange(ctx context.Context, request StorageChangeRequest) (StorageChangeReview, error) {
	s.completedMu.Lock()
	defer s.completedMu.Unlock()
	ctx, releaseTargets, err := s.lockAllImportTargets(ctx, 15*time.Second)
	if err != nil {
		return StorageChangeReview{}, err
	}
	defer releaseTargets()
	ctx, release := s.lockStorageDecision(ctx)
	defer release()
	plan, err := s.completedStorageChangePlan(ctx, request)
	return plan.review, err
}
func (s *Service) AcceptCompletedStorageChange(ctx context.Context, request StorageChangeRequest) error {
	if request.ReviewToken == "" {
		return fmt.Errorf("review the affected storage and requests before accepting this change")
	}
	s.completedMu.Lock()
	defer s.completedMu.Unlock()
	ctx, releaseTargets, err := s.lockAllImportTargets(ctx, 15*time.Second)
	if err != nil {
		return err
	}
	defer releaseTargets()
	ctx, release := s.lockStorageDecision(ctx)
	defer release()
	plan, err := s.completedStorageChangePlan(ctx, request)
	if err != nil {
		return err
	}
	if plan.review.ReviewToken != request.ReviewToken {
		return fmt.Errorf("affected storage custody changed; refresh the review")
	}
	if plan.review.ActiveImports > 0 {
		return fmt.Errorf("wait for or cancel active imports in this root before accepting replacement storage")
	}
	observed, err := observeStorageIdentity(plan.root.Path)
	if err != nil || observed != request.ObservedIdentity {
		return fmt.Errorf("observed storage changed before acceptance; scan and review again")
	}
	accepted := CompletedRoot{Path: plan.root.Path, StorageIdentity: observed, ObservedStorageIdentity: observed, CheckedAt: time.Now().UTC(), Error: "storage change accepted; scan required before managing files", Entries: []CompletedEntry{}}
	raw, err := json.Marshal(accepted)
	if err != nil {
		return err
	}
	if err := s.db.AcceptCompletedStorageChange(ctx, plan.root.Path, request.ExpectedIdentity, observed, request.ReviewToken, string(raw), plan.generations, plan.orphanPaths); err != nil {
		return err
	}
	// The accepted root is durable even if scanning is interrupted. Its pending
	// scan error protects operations until the ordinary inventory pass succeeds.
	if err := s.scanCompletedLocked(ctx); err != nil {
		s.log.Warn("accepted replacement storage awaits complete scan", "root", plan.root.Path, "err", err)
	}
	return nil
}
func (s *Service) completedStorageChangePlan(ctx context.Context, request StorageChangeRequest) (storageChangePlan, error) {
	var plan storageChangePlan
	if !filepath.IsAbs(request.Root) || request.ExpectedIdentity == "" || request.ObservedIdentity == "" || request.ExpectedIdentity == request.ObservedIdentity {
		return plan, fmt.Errorf("select an exact expected-versus-observed storage change")
	}
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return plan, err
	}
	receipts, err := s.db.CompletedReceipts(ctx)
	if err != nil {
		return plan, err
	}
	roots, err := s.completedRoots(ctx, receipts)
	if err != nil {
		return plan, err
	}
	rootPath := canonicalCompletedPath(request.Root)
	configured := false
	for _, root := range roots {
		if root == rootPath {
			configured = true
		}
	}
	if !configured {
		return plan, fmt.Errorf("storage root is no longer configured")
	}
	found := false
	for _, root := range inventory.Roots {
		if root.Path == rootPath {
			plan.root = root
			found = true
		}
	}
	if !found || plan.root.StorageIdentity != request.ExpectedIdentity || plan.root.ObservedStorageIdentity != request.ObservedIdentity {
		return plan, fmt.Errorf("storage comparison changed; scan and review again")
	}
	observed, err := observeStorageIdentity(rootPath)
	if err != nil || observed != request.ObservedIdentity {
		return plan, fmt.Errorf("observed storage is unavailable or changed; scan and review again")
	}
	libraryRoots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return plan, err
	}
	for _, library := range libraryRoots {
		path := canonicalCompletedPath(library.Path)
		if within(rootPath, path) || within(path, rootPath) {
			return plan, fmt.Errorf("completed folder overlaps a library root; correct the path configuration")
		}
	}
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return plan, err
	}
	byClient := map[int64]ports.ClientConfig{}
	for _, client := range clients {
		byClient[client.ID] = client
	}
	references, err := s.db.DownloadStorageReferences(ctx)
	if err != nil {
		return plan, err
	}
	generations := map[string]bool{}
	var selected []sqlite.DownloadStorageReference
	for _, reference := range references {
		cfg := byClient[reference.ClientID]
		path := reference.ImportPath
		if path == "" && reference.SavePath != "" {
			path = ports.MapRemotePath(cfg.PathMappings, reference.SavePath)
		}
		affected := path != "" && within(rootPath, canonicalCompletedPath(path))
		if path == "" {
			for _, mapping := range cfg.PathMappings {
				local := canonicalCompletedPath(mapping.Local)
				if within(rootPath, local) || within(local, rootPath) {
					affected = true
				}
			}
		}
		if !affected {
			continue
		}
		generations[reference.GenerationKey] = true
		selected = append(selected, reference)
		if reference.State != "failed" && reference.State != "imported" {
			plan.review.InFlightRequests++
		}
		if s.ImportRunning(reference.ID) {
			plan.review.ActiveImports++
		}
	}
	var receiptPaths []string
	for _, receipt := range receipts {
		if within(rootPath, canonicalCompletedPath(receipt.Path)) {
			generations[receipt.GenerationKey] = true
			receiptPaths = append(receiptPaths, receipt.GenerationKey+"\x00"+receipt.Path)
		}
	}
	for _, entry := range plan.root.Entries {
		if within(rootPath, canonicalCompletedPath(entry.Path)) {
			plan.orphanPaths = append(plan.orphanPaths, entry.Path)
		}
	}
	// Unassociated old Runner publications must also retain their exact path
	// fence; they have no download generation yet and cannot inherit new trust.
	before := int64(0)
	for {
		page, next, err := s.db.DeliveryReceiptPage(ctx, before, 100)
		if err != nil {
			return plan, err
		}
		for _, receipt := range page {
			if receipt.MappedPath != "" && within(rootPath, canonicalCompletedPath(receipt.MappedPath)) {
				if receipt.GenerationKey != "" {
					generations[receipt.GenerationKey] = true
				} else {
					plan.orphanPaths = append(plan.orphanPaths, receipt.MappedPath)
				}
			}
		}
		if next == 0 {
			break
		}
		before = next
	}
	delete(generations, "")
	for generation := range generations {
		plan.generations = append(plan.generations, generation)
	}
	sort.Strings(plan.generations)
	sort.Strings(receiptPaths)
	sort.Strings(plan.orphanPaths)
	uniquePaths := plan.orphanPaths[:0]
	for _, path := range plan.orphanPaths {
		if len(uniquePaths) == 0 || uniquePaths[len(uniquePaths)-1] != path {
			uniquePaths = append(uniquePaths, path)
		}
	}
	plan.orphanPaths = uniquePaths
	request.Root = rootPath
	request.ReviewToken = ""
	plan.review.StorageChangeRequest = request
	plan.review.ExpectedDescription = displayStorageIdentity(request.ExpectedIdentity)
	plan.review.ObservedDescription = displayStorageIdentity(observed)
	plan.review.ExistingPayloads = len(plan.root.Entries)
	plan.review.ExistingRequests = len(selected)
	type payloadIdentity struct{ Path, Fingerprint string }
	identities := make([]payloadIdentity, 0, len(plan.root.Entries))
	for _, entry := range plan.root.Entries {
		identities = append(identities, payloadIdentity{entry.Path, entry.Fingerprint})
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Path < identities[j].Path })
	raw, err := json.Marshal(struct {
		Request                     StorageChangeRequest
		Generations, Paths, Orphans []string
		Entries                     []payloadIdentity
		Requests, InFlight, Imports int
	}{request, plan.generations, receiptPaths, plan.orphanPaths, identities, plan.review.ExistingRequests, plan.review.InFlightRequests, plan.review.ActiveImports})
	if err != nil {
		return plan, err
	}
	digest := sha256.Sum256(raw)
	plan.review.ReviewToken = hex.EncodeToString(digest[:])
	return plan, nil
}

var observeLegacyStorageIdentity = legacyStorageIdentity

// Strengthening an old device-path observation is an identity-format upgrade
// only when the SAME live raw mount observation still exactly matches it.
// A changed raw source never acquires the old identity's authority.
func canStrengthenStorageIdentity(path, expected, observed string) bool {
	old, current := strings.Split(expected, "\x00"), strings.Split(observed, "\x00")
	if len(old) != 4 || len(current) != 4 || old[0] != "linux-v1" || current[0] != "linux-v1" || !strings.HasPrefix(old[2], "/dev/") || !strings.HasPrefix(current[2], "UUID=") || old[1] != current[1] || old[3] != current[3] {
		return false
	}
	raw, err := observeLegacyStorageIdentity(path)
	return err == nil && raw == expected
}
