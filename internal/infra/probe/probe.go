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
	"github.com/pjunod/monarr/internal/domain/mediainfo/dovi"
)

// FFprobeEnv names an external ffprobe binary. Opportunistic enrichment only:
// The stock distroless image bundles ffprobe on PATH. Standalone installations
// without it keep the filename-derived quality for unsupported containers
// (ADR 0013 §2).
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

// NativeFile uses only the bounded in-process container parser. Recovery never
// starts an optional external process while inspecting a handoff.
func NativeFile(path string) (mediainfo.Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return mediainfo.Info{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return mediainfo.Info{}, err
	}
	if !st.Mode().IsRegular() {
		return mediainfo.Info{}, fmt.Errorf("probe: not a regular file")
	}
	return mediainfo.Probe(f, st.Size())
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
	// -show_frames over the first packet of each stream adds the per-frame
	// side data — the Dolby Vision RPU and HDR10+ metadata — that no stream
	// header carries. One decoded frame per stream, bounded by the timeout.
	out, err := exec.CommandContext(ctx, bin,
		"-v", "quiet", "-print_format", "json", "-show_streams", "-show_format",
		"-show_frames", "-read_intervals", "%+#1", path).Output()
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
		Index         int    `json:"index"`
		SideData      []struct {
			Type       string `json:"side_data_type"`
			Profile    int    `json:"dv_profile"`
			Level      int    `json:"dv_level"`
			RPU        int    `json:"rpu_present_flag"`
			EL         int    `json:"el_present_flag"`
			BL         int    `json:"bl_present_flag"`
			CompatID   int    `json:"dv_bl_signal_compatibility_id"`
			VersionMaj int    `json:"dv_version_major"`
			VersionMin int    `json:"dv_version_minor"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Frames []ffprobeFrame `json:"frames"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// ffprobeFrame is one decoded frame's side data. FFmpeg decodes the RPU
// itself and prints its fields; they are mapped onto dovi.Header and dovi.NLQ
// so classification runs through the same functions the native parser uses.
type ffprobeFrame struct {
	MediaType   string `json:"media_type"`
	StreamIndex int    `json:"stream_index"`
	SideData    []struct {
		Type                string `json:"side_data_type"`
		VDRRPUProfile       int    `json:"vdr_rpu_profile"`
		BLFullRange         int    `json:"bl_video_full_range_flag"`
		ELSpatialResampling int    `json:"el_spatial_resampling_filter_flag"`
		DisableResidual     int    `json:"disable_residual_flag"`
		VDRBitDepth         int    `json:"vdr_bit_depth"`
		CoefDataType        int    `json:"coef_data_type"`
		CoefLog2Denom       int    `json:"coef_log2_denom"`
		Components          []struct {
			NLQOffset uint64 `json:"nlq_offset"`
			VDRInMax  uint64 `json:"vdr_in_max"`
			Slope     uint64 `json:"linear_deadzone_slope"`
			Threshold uint64 `json:"linear_deadzone_threshold"`
		} `json:"components"`
	} `json:"side_data_list"`
}

// FFmpeg side-data type names.
const (
	ffDoViConfig   = "DOVI configuration record"
	ffDoViMetadata = "Dolby Vision Metadata"
	ffHDR10Plus    = "HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"
)

// doviFromFrame maps FFmpeg's decoded RPU onto dovi.RPU. FFmpeg prints the
// quantizer as single fixed-point values (integer part shifted above the
// coef_log2_denom fraction bits); they are split back into the bitstream's
// form for dovi.NLQ. The CM version and level 6 block are not printed, so
// they stay unknown on this path.
func doviFromFrame(f ffprobeFrame) (rpu *dovi.RPU, hdr10Plus bool) {
	for _, sd := range f.SideData {
		switch sd.Type {
		case ffHDR10Plus:
			hdr10Plus = true
		case ffDoViMetadata:
			h := dovi.Header{
				VDRRPUProfile:       sd.VDRRPUProfile,
				BLFullRange:         sd.BLFullRange == 1,
				ELSpatialResampling: sd.ELSpatialResampling == 1,
				DisableResidual:     sd.DisableResidual == 1,
				VDRBitDepth:         sd.VDRBitDepth,
			}
			r := dovi.RPU{Profile: h.Profile()}
			switch {
			case h.DisableResidual:
				r.ELType = dovi.ELNone
			case sd.CoefDataType == 0 && sd.CoefLog2Denom > 0 && sd.CoefLog2Denom <= 32 && len(sd.Components) == 3:
				var n dovi.NLQ
				shift := uint(sd.CoefLog2Denom)
				mask := uint64(1)<<shift - 1
				for c, comp := range sd.Components {
					n.Offset[c] = comp.NLQOffset
					n.VDRInMaxInt[c], n.VDRInMax[c] = comp.VDRInMax>>shift, comp.VDRInMax&mask
					n.SlopeInt[c], n.Slope[c] = comp.Slope>>shift, comp.Slope&mask
					n.ThresholdInt[c], n.Threshold[c] = comp.Threshold>>shift, comp.Threshold&mask
				}
				r.ELType = n.ELType()
			}
			rpu = &r
		}
	}
	return rpu, hdr10Plus
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
			var cfg *dovi.Config
			for _, sd := range st.SideData {
				if sd.Type == ffDoViConfig {
					cfg = &dovi.Config{
						VersionMajor: sd.VersionMaj, VersionMinor: sd.VersionMin,
						Profile: sd.Profile, Level: sd.Level,
						RPU: sd.RPU == 1, EL: sd.EL == 1, BL: sd.BL == 1,
						BLCompatID: sd.CompatID,
					}
				}
			}
			var rpu *dovi.RPU
			var hdr10Plus bool
			for _, fr := range out.Frames {
				if fr.MediaType == "video" && fr.StreamIndex == st.Index {
					rpu, hdr10Plus = doviFromFrame(fr)
					break
				}
			}
			v.ApplyDolbyVision(cfg, rpu, hdr10Plus)
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
	info.ProberRevision = mediainfo.ProberRevision
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
