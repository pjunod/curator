package mediainfo

import (
	"errors"
	"io"
	"math"
	"strings"
)

// mkvBudget caps how many bytes an MKV probe may read. Everything we want
// lives in Segment→Info and Segment→Tracks, which muxers put in the first few
// hundred KiB; 8 MiB is generous headroom for a file with a large SeekHead or
// a fat CodecPrivate, and small enough that sweeping a NAS stays cheap.
const mkvBudget = 8 << 20

// EBML element IDs, marker bit included (that is how they appear on the wire).
const (
	idEBMLHeader = 0x1A45DFA3
	idSegment    = 0x18538067

	idSeekHead     = 0x114D9B74
	idSeek         = 0x4DBB
	idSeekID       = 0x53AB
	idSeekPosition = 0x53AC

	idInfo           = 0x1549A966
	idTimestampScale = 0x2AD7B1
	idDuration       = 0x4489
	idMuxingApp      = 0x4D80
	idWritingApp     = 0x5741

	idTracks       = 0x1654AE6B
	idTrackEntry   = 0xAE
	idTrackType    = 0x83
	idCodecID      = 0x86
	idCodecPrivate = 0x63A2
	idLanguage     = 0x22B59C
	idLanguageBCP  = 0x22B59D

	idVideo          = 0xE0
	idPixelWidth     = 0xB0
	idPixelHeight    = 0xBA
	idFlagInterlaced = 0x9A
	idColour         = 0x55B0
	idBitsPerChannel = 0x55B2
	idTransferChars  = 0x55BA
	idBlockAddMap    = 0x41E4
	idBlockAddIDType = 0x41E7

	idAudio    = 0xE1
	idChannels = 0x9F

	idCluster = 0x1F43B675
	idCues    = 0x1C53BB6B
	idTags    = 0x1254C367
	idAttachs = 0x1941A469
	idChapter = 0x1043A770
)

// BlockAddIDType values that mark a Dolby Vision enhancement layer.
const (
	blockAddDvcC = 0x64766343 // 'dvcC'
	blockAddDvvC = 0x64767643 // 'dvvC'
)

// Transfer characteristics values from the Matroska/ITU-T H.273 table.
const (
	transferPQ  = 16 // SMPTE ST 2084 — HDR10
	transferHLG = 18 // ARIB STD-B67 — HLG
)

// probeMatroska walks EBML far enough to fill an Info and no further.
//
// The structure is Segment{SeekHead?, Info, Tracks, Cluster+, …}. We iterate
// Segment's children in order, which is what every muxer in practice writes,
// and stop the moment we hit the first Cluster — media data is the one thing
// we refuse to read. If the sequential pass ended without both Info and Tracks
// (a muxer that put Tracks after the clusters), we fall back to the SeekHead
// index and jump straight to what is missing.
func probeMatroska(r io.ReaderAt, size int64) (Info, error) {
	c := newCursor(r, size, mkvBudget)

	// EBML header: verified by the caller's magic check, so just step over it.
	if _, _, body, err := readElement(c); err != nil {
		return Info{}, wrapMalformed(err)
	} else if err := c.skip(body); err != nil {
		return Info{}, wrapMalformed(err)
	}

	// Find the Segment. Anything else at top level (a second EBML header from
	// a concatenated file) is skipped.
	var segStart, segSize int64
	for c.remaining() > 0 {
		id, _, body, err := readElement(c)
		if err != nil {
			return Info{}, wrapMalformed(err)
		}
		if id == idSegment {
			segStart, segSize = c.pos, body
			break
		}
		if err := c.skip(body); err != nil {
			return Info{}, wrapMalformed(err)
		}
	}
	if segStart == 0 {
		return Info{}, ErrMalformed
	}
	segEnd := size
	if segSize > 0 && segStart+segSize < size {
		segEnd = segStart + segSize
	}

	var (
		info       Info
		seekInfo   int64 = -1
		seekTracks int64 = -1
		haveInfo   bool
		haveTracks bool
		firstErr   error
	)

	walk := func(from, to int64) {
		if err := c.seek(from); err != nil {
			return
		}
		for c.pos < to && c.remaining() > 0 {
			id, _, body, err := readElement(c)
			if err != nil {
				if firstErr == nil && !errors.Is(err, io.ErrUnexpectedEOF) {
					firstErr = err
				}
				return
			}
			start := c.pos
			switch id {
			case idInfo:
				if err := readSegmentInfo(c, body, &info); err != nil && firstErr == nil {
					firstErr = err
				}
				haveInfo = true
			case idTracks:
				if err := readTracks(c, body, &info); err != nil && firstErr == nil {
					firstErr = err
				}
				haveTracks = true
			case idSeekHead:
				readSeekHead(c, body, segStart, &seekInfo, &seekTracks)
			case idCluster:
				// Media data starts here; everything we came for is behind us
				// or reachable through the SeekHead.
				return
			}
			if body < 0 { // unknown-size element we did not consume: give up.
				return
			}
			if err := c.seek(start + body); err != nil {
				return
			}
			if haveInfo && haveTracks {
				return
			}
		}
	}

	walk(segStart, segEnd)

	// Sequential pass came up short — use the index.
	if !haveInfo && seekInfo >= 0 {
		walk(seekInfo, segEnd)
	}
	if !haveTracks && seekTracks >= 0 {
		walk(seekTracks, segEnd)
	}

	if !haveTracks && firstErr == nil {
		firstErr = ErrMalformed
	}
	return info, firstErr
}

func wrapMalformed(err error) error {
	if errors.Is(err, ErrBudget) {
		return err
	}
	return ErrMalformed
}

// readElement reads one element header and returns its ID, header length, and
// body size. A body size of -1 means "unknown" (a live-muxed stream); callers
// treat that as a stopping point rather than guessing.
func readElement(c *cursor) (id uint64, hdr int64, body int64, err error) {
	idv, idLen, err := readVint(c, true)
	if err != nil {
		return 0, 0, 0, err
	}
	sz, szLen, unknown, err := readSize(c)
	if err != nil {
		return 0, 0, 0, err
	}
	if unknown {
		return idv, int64(idLen + szLen), -1, nil
	}
	if sz > uint64(math.MaxInt64) {
		return 0, 0, 0, ErrMalformed
	}
	return idv, int64(idLen + szLen), int64(sz), nil
}

// readVint reads an EBML variable-length integer. keepMarker preserves the
// length-marker bit, which is how element IDs are written and compared.
func readVint(c *cursor, keepMarker bool) (uint64, int, error) {
	b, err := c.read(1)
	if err != nil {
		return 0, 0, err
	}
	first := b[0]
	if first == 0 {
		return 0, 0, ErrMalformed // 5+ byte lengths: not in any real file
	}
	length, mask := 1, byte(0x80)
	for first&mask == 0 {
		length++
		mask >>= 1
	}
	if length > 8 {
		return 0, 0, ErrMalformed
	}
	v := uint64(first)
	if !keepMarker {
		v = uint64(first &^ mask)
	}
	if length > 1 {
		rest, err := c.read(int64(length - 1))
		if err != nil {
			return 0, 0, err
		}
		for _, x := range rest {
			v = v<<8 | uint64(x)
		}
	}
	return v, length, nil
}

// readSize reads a body-size vint, reporting the all-ones "unknown size" form.
func readSize(c *cursor) (val uint64, length int, unknown bool, err error) {
	v, n, err := readVint(c, false)
	if err != nil {
		return 0, 0, false, err
	}
	if v == (uint64(1)<<(7*n))-1 {
		return 0, n, true, nil
	}
	return v, n, false, nil
}

// readSeekHead records where Info and Tracks live, in absolute file offsets.
// SeekPosition is relative to the start of Segment's data.
func readSeekHead(c *cursor, size, segStart int64, seekInfo, seekTracks *int64) {
	end := c.pos + size
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return
		}
		next := c.pos + body
		if id == idSeek {
			var targetID, pos uint64
			inner := c.pos + body
			for c.pos < inner {
				iid, _, ibody, err := readElement(c)
				if err != nil || ibody < 0 {
					return
				}
				istart := c.pos
				switch iid {
				case idSeekID:
					targetID, _ = readUintBody(c, ibody)
				case idSeekPosition:
					pos, _ = readUintBody(c, ibody)
				}
				if c.seek(istart+ibody) != nil {
					return
				}
			}
			if pos > 0 && pos < uint64(math.MaxInt64) {
				abs := segStart + int64(pos)
				switch targetID {
				case idInfo:
					*seekInfo = abs
				case idTracks:
					*seekTracks = abs
				}
			}
		}
		if c.seek(next) != nil {
			return
		}
	}
}

// readSegmentInfo pulls duration and the muxer signature out of Segment→Info.
// Duration is stored in TimestampScale units (nanoseconds by default), as a
// float, which is why it needs converting rather than reading.
func readSegmentInfo(c *cursor, size int64, info *Info) error {
	end := c.pos + size
	scale := uint64(1_000_000) // EBML default: 1 ms per unit
	var duration float64
	var muxingApp string
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return wrapMalformed(err)
		}
		start := c.pos
		switch id {
		case idTimestampScale:
			if v, ok := readUintBody(c, body); ok && v > 0 {
				scale = v
			}
		case idDuration:
			if v, ok := readFloatBody(c, body); ok {
				duration = v
			}
		case idWritingApp:
			info.WritingApp = readStringBody(c, body)
		case idMuxingApp:
			muxingApp = readStringBody(c, body)
		}
		if err := c.seek(start + body); err != nil {
			return wrapMalformed(err)
		}
	}
	if duration > 0 && scale > 0 {
		ms := duration * float64(scale) / 1e6
		if ms > 0 && ms < float64(math.MaxInt64) {
			info.DurationMS = int64(ms)
		}
	}
	// MuxingApp is the library ("libebml"), WritingApp the tool ("MakeMKV").
	// Source inference wants the tool; fall back to the library when a muxer
	// wrote only one of the two.
	if info.WritingApp == "" {
		info.WritingApp = muxingApp
	} else if muxingApp != "" && !strings.EqualFold(muxingApp, info.WritingApp) {
		info.WritingApp = info.WritingApp + " / " + muxingApp
	}
	return nil
}

// readTracks walks TrackEntry elements, keeping the first video track and
// every audio track.
func readTracks(c *cursor, size int64, info *Info) error {
	end := c.pos + size
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return wrapMalformed(err)
		}
		start := c.pos
		if id == idTrackEntry {
			readTrackEntry(c, body, info)
		}
		if err := c.seek(start + body); err != nil {
			return wrapMalformed(err)
		}
	}
	return nil
}

func readTrackEntry(c *cursor, size int64, info *Info) {
	end := c.pos + size
	var (
		codecID   string
		private   []byte
		language  string
		video     *VideoInfo
		audio     *AudioInfo
		trackType uint64
	)
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return
		}
		start := c.pos
		switch id {
		case idTrackType:
			trackType, _ = readUintBody(c, body)
		case idCodecID:
			codecID = readStringBody(c, body)
		case idCodecPrivate:
			if body <= 1<<20 { // a sane codec record is bytes, not megabytes
				private, _ = c.read(body)
			}
		case idLanguage, idLanguageBCP:
			if language == "" {
				language = readStringBody(c, body)
			}
		case idVideo:
			video = readVideoElement(c, body)
		case idAudio:
			audio = readAudioElement(c, body)
		}
		if err := c.seek(start + body); err != nil {
			return
		}
	}

	switch trackType {
	case 1: // video
		if info.Video != nil || video == nil {
			return // first video track wins; cover art comes second
		}
		video.Codec = matroskaVideoCodec(codecID)
		if d := bitDepthFromCodecPrivate(video.Codec, private); d > 0 && video.BitDepth == 0 {
			video.BitDepth = d
		}
		if dvFromCodecID(codecID) {
			video.HDR = append(video.HDR, DV)
		}
		info.Video = video
	case 2: // audio
		if audio == nil {
			audio = &AudioInfo{}
		}
		audio.Codec = matroskaAudioCodec(codecID)
		audio.Language = normalizeLanguage(language)
		if audio.Codec != "" {
			info.Audio = append(info.Audio, *audio)
		}
	}
}

func readVideoElement(c *cursor, size int64) *VideoInfo {
	end := c.pos + size
	v := &VideoInfo{}
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return v
		}
		start := c.pos
		switch id {
		case idPixelWidth:
			if n, ok := readUintBody(c, body); ok {
				v.Width = clampDimension(n)
			}
		case idPixelHeight:
			if n, ok := readUintBody(c, body); ok {
				v.Height = clampDimension(n)
			}
		case idFlagInterlaced:
			// 0 undetermined, 1 interlaced, 2 progressive.
			if n, ok := readUintBody(c, body); ok && n == 1 {
				v.Interlaced = true
			}
		case idColour:
			readColourElement(c, body, v)
		case idBlockAddMap:
			if blockAdditionIsDV(c, body) {
				v.HDR = append(v.HDR, DV)
			}
		}
		if err := c.seek(start + body); err != nil {
			return v
		}
	}
	return v
}

func readColourElement(c *cursor, size int64, v *VideoInfo) {
	end := c.pos + size
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return
		}
		start := c.pos
		switch id {
		case idBitsPerChannel:
			if n, ok := readUintBody(c, body); ok && n > 0 && n <= 16 {
				v.BitDepth = int(n)
			}
		case idTransferChars:
			if n, ok := readUintBody(c, body); ok {
				switch n {
				case transferPQ:
					v.HDR = append(v.HDR, HDR10)
				case transferHLG:
					v.HDR = append(v.HDR, HLG)
				}
			}
		}
		if err := c.seek(start + body); err != nil {
			return
		}
	}
}

// blockAdditionIsDV reports whether a BlockAdditionMapping describes a Dolby
// Vision enhancement layer — the MKV way of saying what MP4 says with a dvcC
// box inside the sample entry.
func blockAdditionIsDV(c *cursor, size int64) bool {
	end := c.pos + size
	found := false
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return found
		}
		start := c.pos
		if id == idBlockAddIDType {
			if n, ok := readUintBody(c, body); ok && (n == blockAddDvcC || n == blockAddDvvC) {
				found = true
			}
		}
		if err := c.seek(start + body); err != nil {
			return found
		}
	}
	return found
}

func readAudioElement(c *cursor, size int64) *AudioInfo {
	end := c.pos + size
	a := &AudioInfo{}
	for c.pos < end {
		id, _, body, err := readElement(c)
		if err != nil || body < 0 {
			return a
		}
		start := c.pos
		if id == idChannels {
			if n, ok := readUintBody(c, body); ok && n > 0 && n < 64 {
				a.Channels = int(n)
			}
		}
		if err := c.seek(start + body); err != nil {
			return a
		}
	}
	return a
}

// readUintBody reads a big-endian unsigned integer body (EBML stores them
// minimally: a 1 is one byte).
func readUintBody(c *cursor, size int64) (uint64, bool) {
	if size <= 0 || size > 8 {
		return 0, false
	}
	b, err := c.read(size)
	if err != nil {
		return 0, false
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v, true
}

// readFloatBody reads an EBML float (4 or 8 bytes; 0 bytes means 0.0).
func readFloatBody(c *cursor, size int64) (float64, bool) {
	switch size {
	case 0:
		return 0, true
	case 4:
		b, err := c.read(4)
		if err != nil {
			return 0, false
		}
		bits := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		f := float64(math.Float32frombits(bits))
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	case 8:
		b, err := c.read(8)
		if err != nil {
			return 0, false
		}
		var bits uint64
		for _, x := range b {
			bits = bits<<8 | uint64(x)
		}
		f := math.Float64frombits(bits)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func readStringBody(c *cursor, size int64) string {
	if size <= 0 || size > 4096 {
		return ""
	}
	b, err := c.read(size)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\x00")
}

func clampDimension(n uint64) int {
	if n > 65535 {
		return 0
	}
	return int(n)
}

func normalizeLanguage(l string) string {
	l = strings.TrimSpace(strings.ToLower(l))
	if l == "und" || l == "" {
		return ""
	}
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	return l
}

// matroskaVideoCodec maps CodecID strings onto monarr's codec vocabulary.
func matroskaVideoCodec(id string) string {
	switch {
	case strings.HasPrefix(id, "V_MPEG4/ISO/AVC"), strings.HasPrefix(id, "V_MPEG4/ISO/avc"):
		return "h264"
	case strings.HasPrefix(id, "V_MPEGH/ISO/HEVC"):
		return "hevc"
	case strings.HasPrefix(id, "V_AV1"):
		return "av1"
	case strings.HasPrefix(id, "V_VP9"):
		return "vp9"
	case strings.HasPrefix(id, "V_VP8"):
		return "vp8"
	case strings.HasPrefix(id, "V_MPEG2"):
		return "mpeg2"
	case strings.HasPrefix(id, "V_MPEG1"):
		return "mpeg1"
	case strings.HasPrefix(id, "V_MPEG4"):
		return "mpeg4"
	case strings.HasPrefix(id, "V_MS/VFW/FOURCC"):
		return "vc1" // the usual occupant; the FourCC itself lives in CodecPrivate
	case strings.HasPrefix(id, "V_THEORA"):
		return "theora"
	case id == "":
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(id, "V_"))
}

func matroskaAudioCodec(id string) string {
	switch {
	case strings.HasPrefix(id, "A_TRUEHD"), strings.HasPrefix(id, "A_MLP"):
		return "truehd"
	case strings.HasPrefix(id, "A_DTS"):
		return "dts"
	case strings.HasPrefix(id, "A_EAC3"), strings.HasPrefix(id, "A_AC3/BSID10"),
		strings.HasPrefix(id, "A_AC3/BSID11"):
		return "eac3"
	case strings.HasPrefix(id, "A_AC3"):
		return "ac3"
	case strings.HasPrefix(id, "A_AAC"):
		return "aac"
	case strings.HasPrefix(id, "A_FLAC"):
		return "flac"
	case strings.HasPrefix(id, "A_PCM"):
		return "pcm"
	case strings.HasPrefix(id, "A_ALAC"):
		return "alac"
	case strings.HasPrefix(id, "A_OPUS"):
		return "opus"
	case strings.HasPrefix(id, "A_VORBIS"):
		return "vorbis"
	case strings.HasPrefix(id, "A_MPEG/L3"):
		return "mp3"
	case strings.HasPrefix(id, "A_MPEG/L2"):
		return "mp2"
	case id == "":
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(id, "A_"))
}

// dvFromCodecID catches the Dolby Vision CodecIDs some muxers write instead of
// (or alongside) a BlockAdditionMapping.
func dvFromCodecID(id string) bool {
	u := strings.ToUpper(id)
	return strings.Contains(u, "DVHE") || strings.Contains(u, "DVH1") ||
		strings.Contains(u, "V_DOLBYVISION")
}
