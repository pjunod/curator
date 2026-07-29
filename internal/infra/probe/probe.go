// Package probe opens a file on disk and measures it.
//
// The measuring itself is pure and lives in domain/mediainfo. What lives here
// is everything that touches the world: opening the file, and the optional
// hand-off to an external ffprobe for containers the native walker does not
// deep-parse. That split is deliberate — the parser is fuzzed and corpus-tested
// precisely because it never has to care where its bytes came from.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// FFprobeEnv names an external ffprobe binary. Opportunistic enrichment only:
// monarr's stock image is distroless with nothing on PATH, so this is normally
// unset and nothing degrades — AVI/TS/WMV files simply keep the quality their
// filename claims, exactly as they do today (ADR 0013 §2).
const FFprobeEnv = "MONARR_FFPROBE"

// ffprobeTimeout bounds the external call. A hung ffprobe must not hold a job
// lease open forever.
const ffprobeTimeout = 30 * time.Second

// File measures the file at path.
//
// A non-nil error alongside a partially-filled Info is normal and useful: an
// unsupported container comes back with Container set to "unsupported:<ext>"
// and mediainfo.ErrUnknownContainer, which is a fact worth recording rather
// than a failure worth retrying.
func File(path string) (mediainfo.Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return mediainfo.Info{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return mediainfo.Info{}, err
	}
	if st.IsDir() {
		return mediainfo.Info{}, fmt.Errorf("probe: %s is a directory", path)
	}

	info, err := mediainfo.Probe(f, st.Size())
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, mediainfo.ErrUnknownContainer) {
		return info, err
	}

	// The magic bytes matched neither EBML nor an ISO base-media brand. That
	// is two very different situations and they must not be recorded alike:
	//
	//   - An .avi/.ts/.wmv was never going to parse here. Mark the container
	//     so the UI can say why, and so scans stop re-reading it forever.
	//   - An .mkv that will not sniff is TRUNCATED, corrupt, still
	//     downloading, or behind a permission we did not have. Leave the
	//     container blank: it is a failure, and failures are worth retrying
	//     once whatever was wrong is fixed.
	if !mediainfo.IsNativeContainer(filepath.Ext(path)) {
		info.Container = mediainfo.Unsupported(filepath.Ext(path))
		if ext, ffErr := ffprobe(path, st.Size()); ffErr == nil {
			ext.Container = info.Container
			return ext, nil
		}
	}
	return info, err
}

// ffprobe shells out when a binary is configured or happens to be on PATH.
func ffprobe(path string, size int64) (mediainfo.Info, error) {
	bin := os.Getenv(FFprobeEnv)
	if bin == "" {
		found, err := exec.LookPath("ffprobe")
		if err != nil {
			return mediainfo.Info{}, err
		}
		bin = found
	}
	ctx, cancel := context.WithTimeout(context.Background(), ffprobeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin,
		"-v", "quiet", "-print_format", "json", "-show_streams", "-show_format", path).Output()
	if err != nil {
		return mediainfo.Info{}, err
	}
	return ParseFFprobe(out, size)
}

// ffprobeOutput is the slice of ffprobe's JSON we map onto Info. Deliberately
// partial: only the fields the native prober also produces, so the two paths
// cannot drift into recording different vocabularies for the same file.
type ffprobeOutput struct {
	Streams []struct {
		CodecType     string `json:"codec_type"`
		CodecName     string `json:"codec_name"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		BitsPerRaw    string `json:"bits_per_raw_sample"`
		ColorTransfer string `json:"color_transfer"`
		FieldOrder    string `json:"field_order"`
		Channels      int    `json:"channels"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// ParseFFprobe maps ffprobe's JSON onto the same Info the native walkers
// produce. Exported so a test can pin the mapping without needing ffmpeg
// installed on whatever machine is running the suite.
func ParseFFprobe(raw []byte, size int64) (mediainfo.Info, error) {
	var out ffprobeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return mediainfo.Info{}, err
	}
	var info mediainfo.Info
	for _, st := range out.Streams {
		switch st.CodecType {
		case "video":
			if info.Video != nil {
				continue // first video track wins, same as the native walkers
			}
			v := &mediainfo.VideoInfo{
				Codec:  normalizeCodec(st.CodecName),
				Width:  st.Width,
				Height: st.Height,
				// ffprobe reports "progressive", "unknown", or one of several
				// interlaced orders (tt/bb/tb/bt); anything definite that is
				// not progressive is interlacing.
				Interlaced: st.FieldOrder != "" && st.FieldOrder != "progressive" &&
					st.FieldOrder != "unknown",
			}
			if d, err := strconv.Atoi(st.BitsPerRaw); err == nil && d > 0 && d <= 16 {
				v.BitDepth = d
			} else {
				v.BitDepth = 8
			}
			switch st.ColorTransfer {
			case "smpte2084":
				v.HDR = append(v.HDR, mediainfo.HDR10)
			case "arib-std-b67":
				v.HDR = append(v.HDR, mediainfo.HLG)
			}
			info.Video = v
		case "audio":
			info.Audio = append(info.Audio, mediainfo.AudioInfo{
				Codec: normalizeCodec(st.CodecName), Channels: st.Channels,
			})
		}
	}
	if secs, err := strconv.ParseFloat(out.Format.Duration, 64); err == nil && secs > 0 {
		info.DurationMS = int64(secs * 1000)
		if size > 0 && info.DurationMS > 0 {
			info.BitrateKbps = size * 8 / info.DurationMS
		}
	}
	if info.Video == nil {
		return info, errors.New("ffprobe: no video stream")
	}
	return info, nil
}

// normalizeCodec maps ffmpeg's codec names onto the vocabulary the native
// prober emits.
func normalizeCodec(name string) string {
	switch strings.ToLower(name) {
	case "h264", "avc":
		return "h264"
	case "hevc", "h265":
		return "hevc"
	case "mpeg2video":
		return "mpeg2"
	case "vc1":
		return "vc1"
	case "truehd", "mlp":
		return "truehd"
	case "dts", "dca":
		return "dts"
	case "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_s16be", "pcm_s24be":
		return "pcm"
	}
	return strings.ToLower(name)
}
