package acquisition

import (
	"context"
	"crypto/sha256"
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
func (s *Service) decidePublicationSidecars(ctx context.Context, item domain.MediaItem, pub sqlite.Publication, m ports.Manifest, placed map[string]string, done map[string]bool) (map[string]sidecarDecision, error) {
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
	if len(subs) == 0 {
		return out, nil
	}
	ctx, release := s.lockImportTarget(ctx, item.ID)
	defer release()
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
		name := base
		for n := 2; used[strings.ToLower(filepath.Join(filepath.Dir(video.library), name))]; n++ {
			name = base + "." + strconv.Itoa(n)
		}
		used[strings.ToLower(filepath.Join(filepath.Dir(video.library), name))] = true
		for _, f := range unit {
			dest := filepath.Join(filepath.Dir(video.library), name+strings.ToLower(filepath.Ext(f.Path)))
			src := filepath.Join(pub.Path, filepath.FromSlash(f.Path))
			err := placeSubtitle(ctx, src, dest, f.SHA256)
			if errors.Is(err, errSubtitleDigest) {
				out[f.Path] = sidecarDecision{outcome: "discarded", discardReason: "rejected", reason: "subtitle bytes differ from the manifest; not kept"}
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("place subtitle %s: %w", f.Path, err)
			}
			out[f.Path] = sidecarDecision{outcome: "placed", libraryPath: dest}
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
	nameTokens := subtitleTokens(stem)
	whole := false
	if strings.HasPrefix(strings.ToLower(stem), strings.ToLower(vstem)) {
		nameTokens = subtitleTokens(stem[len(vstem):])
		whole = true
	}
	lang, forced, sdh := "", false, false
	for _, t := range nameTokens {
		switch subtitleFlagOf(t) {
		case "forced":
			forced = true
		case "sdh":
			sdh = true
		}
	}
	// A name that is only a language ("en.srt", "2_English.srt") or a
	// remainder after the video's stem may use a two-letter code; anywhere
	// else only a spelled-out code or name counts, so a title word such as
	// "It" is never read as Italian.
	short := whole || len(withoutFlags(nameTokens)) == 1
	lang = subtitleLanguage(nameTokens, short)
	if lang == "" {
		dir := path.Dir(rel)
		for dir != "." && dir != "/" && lang == "" {
			if !strings.EqualFold(path.Base(dir), vstem) {
				tokens := subtitleTokens(path.Base(dir))
				lang = subtitleLanguage(tokens, len(withoutFlags(tokens)) == 1)
			}
			dir = path.Dir(dir)
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

// subtitleLanguage reads the last language token (languages trail the name:
// "Show.S01E01.eng.forced").
func subtitleLanguage(tokens []string, allowShort bool) string {
	for i := len(tokens) - 1; i >= 0; i-- {
		t := strings.ToLower(tokens[i])
		if subtitleFlag(t) || len(t) < 2 || (len(t) == 2 && !allowShort) || !language.Known(t) {
			continue
		}
		if code := language.Canonical(t); code != "" && language.Valid(code) {
			return code
		}
	}
	return ""
}

// subtitleFlagOf names the flag a token marks: "forced" (forced or foreign
// parts only) or "sdh" (hearing impaired: sdh, cc, hi), else "".
func subtitleFlagOf(t string) string {
	switch strings.ToLower(t) {
	case "forced", "foreign":
		return "forced"
	case "sdh", "cc", "hi":
		return "sdh"
	}
	return ""
}

func subtitleFlag(t string) bool { return subtitleFlagOf(t) != "" }

func withoutFlags(tokens []string) []string {
	var out []string
	for _, t := range tokens {
		if !subtitleFlag(t) {
			out = append(out, t)
		}
	}
	return out
}

func subtitleTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) })
}

func pathStem(p string) string {
	base := path.Base(filepath.ToSlash(p))
	return strings.TrimSuffix(base, path.Ext(base))
}

var errSubtitleDigest = errors.New("subtitle bytes differ from the manifest")

// placeSubtitle copies one subtitle into the library: written to a scratch
// name beside it while its digest is computed, compared with the manifest's,
// synced, then renamed into place (replacing an older subtitle of the same
// name; the name carries the video's own stem). The bytes are small; nothing
// is read back.
func placeSubtitle(ctx context.Context, src, dest, want string) error {
	if err := rejectSymlinks(src, false); err != nil {
		return err
	}
	if err := rejectSymlinks(filepath.Dir(dest), false); err != nil {
		return err
	}
	if err := rejectSymlinks(dest, true); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
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
		return fail(errSubtitleDigest)
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
