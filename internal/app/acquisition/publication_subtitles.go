package acquisition

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/language"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Sidecars of a publication (contract section 3.4) are decided by the import
// itself, never left for a person. Subtitles are placed beside the video they
// belong to, named after it (<video stem>.<lang>[.forced][.sdh].<ext>, the
// layout Cinema and every player read). Everything else that is not media of
// the item's kind (nfo, images, text, xml, stray files) is discarded{rejected}:
// a job's publication is a closed scope, and Curator keeps what it uses.
//
// Association: a publication with one video gives every subtitle to it. With
// several (a season pack), a subtitle belongs to the video whose stem prefixes
// its own name, or whose stem (or folder) names one of the subtitle's folders
// ("Subs/<episode>/3_Spanish.srt"). A subtitle that matches no video, or
// whose video was not placed, is discarded with the reason logged. An .idx
// and its .sub travel together under one name.

// subtitleExtensions are the subtitle formats placed with the media.
var subtitleExtensions = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".sub": true, ".idx": true, ".sup": true, ".vtt": true}

func isSubtitleFile(name string) bool {
	return subtitleExtensions[strings.ToLower(filepath.Ext(name))]
}

// sidecarDecision is the outcome the import records for one sidecar file.
type sidecarDecision struct {
	outcome, discardReason, reason, libraryPath string
}

// publicationVideo is one media file of the manifest, with the library path
// it was placed as ("" when it was not placed).
type publicationVideo struct {
	rel, library string
}

// decidePublicationSidecars places the publication's subtitles and decides
// every other non-media file, returning a decision per manifest path. done
// holds the paths an earlier attempt already resolved (placed or discarded);
// they are left as they are. An error is environmental (the library could not
// take a file): nothing is decided and the import is tried again.
func (s *Service) decidePublicationSidecars(ctx context.Context, item domain.MediaItem, pub sqlite.Publication, m ports.Manifest, placed map[string]string, done, fresh map[string]bool) (map[string]sidecarDecision, error) {
	out := map[string]sidecarDecision{}
	var videos []publicationVideo
	var subs []ports.ManifestFile
	for _, f := range m.Files {
		switch {
		case done[f.Path]:
		case deliveryImportablePath(item.Kind, f.Path):
			if !isReleaseSample(f.Path) {
				videos = append(videos, publicationVideo{rel: f.Path, library: placed[f.Path]})
			}
		case item.Kind != domain.KindBook && isSubtitleFile(f.Path):
			subs = append(subs, f)
		default:
			out[f.Path] = sidecarDecision{outcome: "discarded", discardReason: "rejected", reason: "not media of this item's kind; not kept"}
		}
	}
	// Videos an earlier attempt resolved still own their subtitles.
	for _, f := range m.Files {
		if done[f.Path] && deliveryImportablePath(item.Kind, f.Path) && !isReleaseSample(f.Path) {
			videos = append(videos, publicationVideo{rel: f.Path, library: placed[f.Path]})
		}
	}
	ctx, release := s.lockImportTarget(ctx, item.ID)
	defer release()
	// A video this import placed over an earlier one at the same name takes
	// over its subtitles' names: the subtitles Curator placed for the earlier
	// file (by their placement record) go first.
	for _, v := range videos {
		if fresh[v.rel] && v.library != "" {
			s.removePlacedSubtitles(ctx, v.library, pub.ID)
		}
	}
	if len(subs) == 0 {
		return out, nil
	}
	used := map[string]bool{} // library base names taken by this import
	for _, unit := range subtitleUnits(subs) {
		video, ok := subtitleVideo(unit[0].Path, videos)
		if !ok || video.library == "" {
			why := "subtitle matches no video of this publication; not kept"
			if ok {
				why = "subtitle's video " + video.rel + " was not placed; not kept"
			}
			for _, f := range unit {
				out[f.Path] = sidecarDecision{outcome: "discarded", discardReason: "rejected", reason: why}
			}
			s.log.Info("import: subtitle not placed", "publication", pub.ID, "file", unit[0].Path, "why", why)
			continue
		}
		base := subtitleBaseName(unit[0].Path, video)
		dir := filepath.Dir(video.library)
		var name string
		var replaced []sqlite.PublicationFile
		for n := 1; ; n++ {
			name = base
			if n > 1 {
				name = base + "." + strconv.Itoa(n)
			}
			if used[strings.ToLower(filepath.Join(dir, name))] {
				continue
			}
			free, rows, err := s.subtitleNameFree(ctx, unit, dir, name, pub.ID)
			if err != nil {
				return nil, err
			}
			if free {
				replaced = rows
				break
			}
		}
		used[strings.ToLower(filepath.Join(dir, name))] = true
		for _, f := range unit {
			dest := filepath.Join(dir, name+strings.ToLower(filepath.Ext(f.Path)))
			src := filepath.Join(pub.Path, filepath.FromSlash(f.Path))
			err := placeSubtitle(ctx, src, dest, f.SHA256)
			var refused subtitleRefusal
			if errors.As(err, &refused) {
				out[f.Path] = sidecarDecision{outcome: "discarded", discardReason: "rejected", reason: refused.Error() + "; not kept"}
				s.log.Info("import: subtitle not placed", "publication", pub.ID, "file", f.Path, "why", refused.Error())
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("place subtitle %s: %w", f.Path, err)
			}
			out[f.Path] = sidecarDecision{outcome: "placed", libraryPath: dest}
		}
		// The subtitles this replaced under that name were Curator's: their
		// records say so now.
		for _, old := range replaced {
			s.supersedePlacedSubtitle(ctx, old, "replaced by a newer subtitle for this video")
		}
	}
	return out, nil
}

// subtitleUnits groups subtitle files: an .idx and the .sub of the same stem
// in the same folder are one unit; every other file is its own. Order is
// stable (manifest paths sorted) so a retry names files the same way.
func subtitleUnits(files []ports.ManifestFile) [][]ports.ManifestFile {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	pairs := map[string][]ports.ManifestFile{}
	var order []string
	for _, f := range files {
		ext := strings.ToLower(path.Ext(f.Path))
		key := f.Path
		if ext == ".idx" || ext == ".sub" {
			key = strings.ToLower(strings.TrimSuffix(f.Path, path.Ext(f.Path))) + "\x00vobsub"
		}
		if _, seen := pairs[key]; !seen {
			order = append(order, key)
		}
		pairs[key] = append(pairs[key], f)
	}
	units := make([][]ports.ManifestFile, 0, len(order))
	for _, key := range order {
		units = append(units, pairs[key])
	}
	return units
}

// subtitleVideo finds the video a subtitle belongs to: the only one, or the
// one its name or folders identify unambiguously.
func subtitleVideo(rel string, videos []publicationVideo) (publicationVideo, bool) {
	if len(videos) == 1 {
		return videos[0], true
	}
	stem := strings.ToLower(pathStem(rel))
	dirs := strings.Split(strings.ToLower(path.Dir(rel)), "/")
	best, bestLen, ties := publicationVideo{}, 0, 0
	for _, v := range videos {
		vstem := strings.ToLower(pathStem(v.rel))
		score := 0
		if strings.HasPrefix(stem, vstem) {
			score = len(vstem)
		}
		vdir := strings.ToLower(path.Dir(v.rel))
		for i, d := range dirs {
			if d == vstem || (vdir != "." && strings.Join(dirs[:i+1], "/") == vdir) {
				score = max(score, len(vstem))
			}
		}
		switch {
		case score == 0:
		case score > bestLen:
			best, bestLen, ties = v, score, 1
		case score == bestLen:
			ties++
		}
	}
	return best, bestLen > 0 && ties == 1
}

// subtitleBaseName is <video library stem>[.<lang>][.forced][.sdh], from what
// the subtitle's own name and then its folders say. An unknown language adds
// nothing (the player treats it as undetermined).
func subtitleBaseName(rel string, video publicationVideo) string {
	stem := pathStem(rel)
	vstem := pathStem(video.rel)
	tokens := subtitleTokens(stem)
	afterStem := false
	if strings.HasPrefix(strings.ToLower(stem), strings.ToLower(vstem)) {
		tokens = subtitleTokens(stem[len(vstem):])
		afterStem = true
	}
	lang, forced, sdh := subtitleTags(tokens, afterStem)
	for dir := path.Dir(rel); lang == "" && dir != "." && dir != "/"; dir = path.Dir(dir) {
		if !strings.EqualFold(path.Base(dir), vstem) {
			lang, _, _ = subtitleTags(subtitleTokens(path.Base(dir)), false)
		}
	}
	base := pathStem(filepath.Base(video.library))
	if lang != "" {
		base += "." + lang
	}
	if forced {
		base += ".forced"
	}
	if sdh {
		base += ".sdh"
	}
	return base
}

// subtitleTags reads a subtitle name's tokens. The language is the last
// token that is not a flag, and only when it is a language: its full English
// name always; an ISO 639 code (two or three letters, or a tag such as
// fr-CA, reduced to its language) only after the video's stem or when it is
// the name's only token — a code-shaped title word ("It", "Ice") elsewhere is
// not a language. "hi" is SDH beside another language and Hindi alone.
func subtitleTags(tokens []string, afterStem bool) (lang string, forced, sdh bool) {
	var rest []string
	hi := false
	for _, t := range tokens {
		switch strings.ToLower(t) {
		case "forced", "foreign":
			forced = true
		case "sdh", "cc":
			sdh = true
		case "hi":
			hi = true
			rest = append(rest, t)
		default:
			rest = append(rest, t)
		}
	}
	codesAllowed := afterStem || len(rest) == 1 || (len(rest) == 2 && hi)
	// The last token that is not "hi", then "hi" itself as Hindi.
	for i := len(rest) - 1; i >= 0; i-- {
		if strings.EqualFold(rest[i], "hi") {
			continue
		}
		if code := languageOfToken(rest[i], codesAllowed); code != "" {
			return code, forced, sdh || hi
		}
		break
	}
	if hi && len(rest) == 1 && codesAllowed {
		return "hi", forced, sdh
	}
	return "", forced, sdh || (hi && len(rest) > 1)
}

// languageOfToken reads one token as a language: a full English name, or
// (when codes are allowed) an ISO 639-1/-2 code or a language-region tag.
func languageOfToken(t string, codesAllowed bool) string {
	lower := strings.ToLower(t)
	if code := language.Canonical(lower); code != "" && language.Valid(code) && strings.EqualFold(language.Display(code), lower) {
		return code
	}
	if !codesAllowed {
		return ""
	}
	primary := lower
	if i := strings.IndexByte(lower, '-'); i > 0 {
		region := lower[i+1:]
		if len(region) != 2 && len(region) != 4 {
			return ""
		}
		primary = lower[:i]
	}
	if len(primary) != 2 && len(primary) != 3 || !language.Known(primary) {
		return ""
	}
	code := language.Canonical(primary)
	if !language.Valid(code) || (len(primary) == 2 && code != primary) {
		return ""
	}
	return code
}

// subtitleTokens splits a name on everything but letters, digits and
// hyphens (so "fr-CA" stays one tag), dropping number-only tokens
// ("2_English" is English).
func subtitleTokens(s string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' }) {
		t = strings.Trim(t, "-")
		if t == "" || strings.IndexFunc(t, func(r rune) bool { return !unicode.IsDigit(r) }) < 0 {
			continue
		}
		out = append(out, t)
	}
	return out
}

func pathStem(p string) string {
	base := path.Base(filepath.ToSlash(p))
	return strings.TrimSuffix(base, path.Ext(base))
}

// subtitleRefusal is a subtitle that cannot be placed for a reason of its
// own (missing or unreadable in the publication, a symlink, bytes that are
// not the manifest's, a library folder that is a symlink): it is discarded
// with the reason, never retried.
type subtitleRefusal struct{ why string }

func (r subtitleRefusal) Error() string { return r.why }

// placeSubtitle copies one subtitle into the library: written to a scratch
// name beside it while its digest is computed, compared with the manifest's,
// synced, then renamed into place. The caller chose a name that is free or
// held by a subtitle Curator placed (subtitleNameFree). The bytes are small;
// nothing is read back.
func placeSubtitle(ctx context.Context, src, dest, want string) error {
	if err := rejectSymlinks(src, false); err != nil {
		return subtitleRefusal{"the subtitle cannot be read from the publication: " + err.Error()}
	}
	if err := rejectSymlinks(filepath.Dir(dest), false); err != nil {
		return subtitleRefusal{"the video's library folder cannot take it: " + err.Error()}
	}
	if err := rejectSymlinks(dest, true); err != nil {
		return subtitleRefusal{"its library name is not a plain file: " + err.Error()}
	}
	in, err := os.Open(src)
	if err != nil {
		return subtitleRefusal{"the subtitle cannot be read from the publication: " + err.Error()}
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(filepath.Dir(dest), ".monarr-sub-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	fail := func(e error) error {
		_ = out.Close()
		_ = os.Remove(tmp)
		return e
	}
	digest := sha256.New()
	if _, err = io.Copy(io.MultiWriter(out, digest), &contextReader{ctx: ctx, r: in}); err != nil {
		return fail(err)
	}
	if want != "" && hex.EncodeToString(digest.Sum(nil)) != want {
		return fail(subtitleRefusal{"subtitle bytes differ from the manifest"})
	}
	if err = out.Sync(); err != nil {
		return fail(err)
	}
	if err = out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = applyMode(tmp, fileMode()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncPath(filepath.Dir(dest))
}

// removePlacedSubtitles deletes the subtitles Curator placed beside a library
// video (its "<stem>." names), when that video is replaced. Only files with a
// placement record are candidates, and only while they still hold the bytes
// Curator placed: a subtitle someone else put there, or one changed since,
// stays. except skips one publication's own subtitles (a re-run of the same
// import).
func (s *Service) removePlacedSubtitles(ctx context.Context, video string, except int64) {
	prefix := strings.TrimSuffix(video, filepath.Ext(video)) + "."
	rows, err := s.db.PlacedLibraryFilesWithPrefix(ctx, prefix)
	if err != nil {
		s.log.Warn("import: placed subtitles of a replaced video could not be listed", "video", video, "err", err)
		return
	}
	for _, f := range rows {
		if f.PublicationID == except || !isSubtitleFile(f.LibraryPath) || filepath.Dir(f.LibraryPath) != filepath.Dir(video) {
			continue
		}
		info, err := os.Lstat(f.LibraryPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() != f.Bytes {
			continue
		}
		if digest, _, err := fileDigest(ctx, f.LibraryPath); err != nil || digest != f.PlacedSHA256 {
			continue
		}
		if err := os.Remove(f.LibraryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("import: could not remove a replaced video's subtitle", "path", f.LibraryPath, "err", err)
			continue
		}
		_ = syncPath(filepath.Dir(f.LibraryPath))
		s.supersedePlacedSubtitle(ctx, f, "its video "+filepath.Base(video)+" was replaced")
		s.log.Info("import: removed the subtitle of a replaced video", "path", f.LibraryPath, "video", video)
	}
}

// supersedePlacedSubtitle records that a subtitle Curator placed is no
// longer in the library because something newer took its place: its
// publication file is discarded{superseded}, so cleanup retires it instead
// of finding its library copy gone.
func (s *Service) supersedePlacedSubtitle(ctx context.Context, f sqlite.PublicationFile, why string) {
	f.Outcome, f.DiscardReason, f.Reason = "discarded", "superseded", why
	if err := s.db.SetPublicationFileOutcome(ctx, f); err != nil && !errors.Is(err, sqlite.ErrNotFound) {
		s.log.Warn("import: replaced subtitle's record not updated", "publication", f.PublicationID, "file", f.Path, "err", err)
	}
}

// subtitleNameFree reports whether a subtitle unit may take <dir>/<name>.<ext>
// for each of its files: nothing is there, or what is there is a subtitle
// Curator placed (its placed record and bytes) — then it is replaced, and the
// records are returned so they can be marked superseded. A file Curator did
// not place is never overwritten: the caller takes the next suffix.
func (s *Service) subtitleNameFree(ctx context.Context, unit []ports.ManifestFile, dir, name string, self int64) (bool, []sqlite.PublicationFile, error) {
	var replaced []sqlite.PublicationFile
	for _, f := range unit {
		dest := filepath.Join(dir, name+strings.ToLower(filepath.Ext(f.Path)))
		info, err := os.Lstat(dest)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, nil, err
		}
		if !info.Mode().IsRegular() {
			return false, nil, nil
		}
		rows, err := s.db.PlacedLibraryFilesWithPrefix(ctx, dest)
		if err != nil {
			return false, nil, err
		}
		ours := false
		for _, r := range rows {
			if r.LibraryPath != dest {
				continue
			}
			if r.PublicationID == self && r.Path == f.Path {
				ours = true // this import's own earlier attempt
				break
			}
			if digest, _, derr := fileDigest(ctx, dest); derr == nil && digest == r.PlacedSHA256 {
				ours = true
				replaced = append(replaced, r)
				break
			}
		}
		if !ours {
			return false, nil, nil
		}
	}
	return true, replaced, nil
}

// redecideLeftoverSidecars decides the sidecars an earlier build left for
// Resolve in a publication that already imported (migration 0076 made them
// pending): subtitles go beside the video the publication placed, the rest
// are discarded. The placed video is not touched. An error leaves them
// pending for the next pass.
func (s *Service) redecideLeftoverSidecars(ctx context.Context, pub sqlite.Publication) error {
	if pub.State != "imported" && pub.State != "notified" && pub.State != "import_failed" {
		return nil
	}
	rows, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return err
	}
	pending := map[string]sqlite.PublicationFile{}
	done, placed := map[string]bool{}, map[string]string{}
	var m ports.Manifest
	var placedRows []sqlite.PublicationFile
	for _, r := range rows {
		m.Files = append(m.Files, ports.ManifestFile{Path: r.Path, Bytes: r.Bytes, SHA256: r.SHA256})
		switch r.Outcome {
		case "pending":
			if r.RemovedAt.IsZero() {
				pending[r.Path] = r
			}
		case "placed":
			placedRows = append(placedRows, r)
			done[r.Path] = true
		default:
			done[r.Path] = true
		}
	}
	if len(pending) == 0 {
		return nil
	}
	decisions := map[string]sidecarDecision{}
	if pub.DownloadID != 0 {
		dl, err := s.db.GetDownload(ctx, pub.DownloadID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, sqlite.ErrNotFound) {
			return err
		}
		if err == nil {
			item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
			if err != nil {
				return err
			}
			// Only a video that is still this publication's placement owns
			// subtitles: one upgraded or deleted since takes none.
			for _, r := range placedRows {
				if s.stillPlacedHere(ctx, r) {
					placed[r.Path] = r.LibraryPath
				}
			}
			if decisions, err = s.decidePublicationSidecars(ctx, item, pub, m, placed, done, nil); err != nil {
				return err
			}
		}
	}
	for path, f := range pending {
		d, ok := decisions[path]
		if isPublicationMedia(path) {
			// Only an import decides a media file (a full or unmounted library
			// holds the publication in discovered, so it never reaches here).
			// This publication's import has ended — imported, or failed with
			// its want grabbed again — so media it left pending (an earlier
			// build's import, or a stall that ended the import before it
			// reached the file) was not selected and is not kept, exactly as
			// the import itself records an unselected file.
			f.Outcome, f.DiscardReason, f.Reason, f.LibraryPath = "discarded", "rejected", "its publication's import ended without placing it; not kept", ""
			if err := s.db.SetPublicationFileOutcome(ctx, f); err != nil {
				return err
			}
			continue
		}
		if !ok {
			// No video to go with, no download row, or not a sidecar: the
			// publication is a closed scope and this file is not kept.
			d = sidecarDecision{outcome: "discarded", discardReason: "rejected", reason: "left undecided by an earlier build; not kept"}
		}
		f.Outcome, f.DiscardReason, f.Reason, f.LibraryPath = d.outcome, d.discardReason, d.reason, d.libraryPath
		if d.outcome == "placed" {
			f.PlacedSHA256 = f.SHA256
		}
		if err := s.db.SetPublicationFileOutcome(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// isPublicationMedia reports a path that is media of any kind Curator
// imports (video or book), whatever the publication's item.
func isPublicationMedia(path string) bool {
	return filename.IsVideo(path) || filename.IsBook(path)
}

// stillPlacedHere reports that a placed publication file's library video is
// still the object its placement published (by the placement journal's
// record): not upgraded, replaced or deleted since.
func (s *Service) stillPlacedHere(ctx context.Context, f sqlite.PublicationFile) bool {
	if f.LibraryPath == "" {
		return false
	}
	info, err := os.Lstat(f.LibraryPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != f.Bytes {
		return false
	}
	p, ok, err := s.db.CommittedPlacementForTarget(ctx, f.LibraryPath, "")
	return err == nil && ok && p.SHA256 == f.SHA256 && sameContentGeneration(p.PublishedFileGeneration, info)
}
