package mediainfo

import (
	"errors"
	"io"
	"strings"
)

// mp4Budget caps an MP4 probe. `moov` carries a sample table whose size grows
// with the length of the film — a three-hour 4K feature can push a few MiB —
// and we read it whole because scanning boxes lazily over NFS is worse than one
// bounded sequential read. 32 MiB is well past any real `moov`.
const mp4Budget = 32 << 20

// maxMoov bounds a single moov read independently of the budget, so a corrupt
// size field cannot ask us to allocate the budget in one go.
const maxMoov = 32 << 20

// probeMP4 walks the ISO base media file format.
//
// The one structural surprise is that `moov` is often at the END of the file:
// a muxer writing a stream cannot know the sample table until it is done, and
// only an explicit faststart pass moves it to the front. That is the whole
// reason this package takes an io.ReaderAt — we walk the top-level box list,
// which is cheap (each step is one 8-byte read and a seek), until moov turns up
// wherever it lives.
func probeMP4(r io.ReaderAt, size int64) (Info, error) {
	c := newCursor(r, size, mp4Budget)

	var moov []byte
	for c.remaining() >= 8 {
		typ, body, err := readBoxHeader(c)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return Info{}, wrapMalformed(err)
		}
		if typ == "moov" {
			if body > maxMoov {
				return Info{}, ErrBudget
			}
			moov, err = c.read(body)
			if err != nil && len(moov) == 0 {
				return Info{}, wrapMalformed(err)
			}
			break
		}
		if body < 0 { // size 0 = "to end of file"
			break
		}
		if err := c.skip(body); err != nil {
			break
		}
	}
	if len(moov) == 0 {
		return Info{}, ErrMalformed
	}

	info := Info{}
	parseMoov(moov, &info)
	if info.Video == nil && len(info.Audio) == 0 {
		return info, ErrMalformed
	}
	return info, nil
}

// readBoxHeader reads one box header at the cursor and leaves the cursor on the
// box body. It returns the body length, or -1 for the "extends to EOF" form.
func readBoxHeader(c *cursor) (typ string, body int64, err error) {
	h, err := c.read(8)
	if err != nil {
		return "", 0, err
	}
	size := int64(h[0])<<24 | int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
	typ = string(h[4:8])
	switch {
	case size == 1:
		ext, err := c.read(8)
		if err != nil {
			return "", 0, err
		}
		var large uint64
		for _, x := range ext {
			large = large<<8 | uint64(x)
		}
		if large < 16 || large > 1<<62 {
			return "", 0, ErrMalformed
		}
		return typ, int64(large) - 16, nil
	case size == 0:
		return typ, -1, nil
	case size < 8:
		return "", 0, ErrMalformed
	}
	return typ, size - 8, nil
}

// boxes walks an in-memory box list, calling fn with each box's type and body.
// Malformed sizes end the walk rather than failing it: the boxes read so far
// are still true.
func boxes(b []byte, fn func(typ string, body []byte)) {
	r := &byteReader{b: b}
	for r.left() >= 8 {
		size, ok := r.u32()
		if !ok {
			return
		}
		t, ok := r.bytes(4)
		if !ok {
			return
		}
		typ := string(t)
		var bodyLen int
		switch {
		case size == 1:
			large, ok := r.u64()
			if !ok || large < 16 || large > uint64(r.left()+16) {
				return
			}
			bodyLen = int(large) - 16
		case size == 0:
			bodyLen = r.left()
		case size < 8:
			return
		default:
			bodyLen = int(size) - 8
		}
		body, ok := r.bytes(bodyLen)
		if !ok {
			// Truncated tail (a header-window fixture, or a partial download):
			// hand over what is there and stop.
			if rest, ok2 := r.bytes(r.left()); ok2 && len(rest) > 0 {
				fn(typ, rest)
			}
			return
		}
		fn(typ, body)
	}
}

func parseMoov(moov []byte, info *Info) {
	boxes(moov, func(typ string, body []byte) {
		switch typ {
		case "mvhd":
			parseMVHD(body, info)
		case "trak":
			parseTrak(body, info)
		}
	})
}

// parseMVHD reads the movie header for the overall duration. Version 1 widened
// the time fields to 64 bits for films longer than 2^32 timescale units.
func parseMVHD(b []byte, info *Info) {
	r := &byteReader{b: b}
	ver, ok := r.u8()
	if !ok || !r.skip(3) {
		return
	}
	var timescale uint32
	var duration uint64
	switch ver {
	case 1:
		if !r.skip(16) { // creation + modification time
			return
		}
		if timescale, ok = r.u32(); !ok {
			return
		}
		if duration, ok = r.u64(); !ok {
			return
		}
	default:
		if !r.skip(8) {
			return
		}
		if timescale, ok = r.u32(); !ok {
			return
		}
		d32, ok := r.u32()
		if !ok {
			return
		}
		duration = uint64(d32)
	}
	if timescale == 0 || duration == 0 || duration == 0xFFFFFFFF {
		return
	}
	ms := duration * 1000 / uint64(timescale)
	if ms > 0 && ms < 1<<62 {
		info.DurationMS = int64(ms)
	}
}

func parseTrak(b []byte, info *Info) {
	var handler string
	var lang string
	var sampleDesc []byte
	boxes(b, func(typ string, body []byte) {
		if typ != "mdia" {
			return
		}
		boxes(body, func(t2 string, b2 []byte) {
			switch t2 {
			case "hdlr":
				handler = parseHDLR(b2)
			case "mdhd":
				lang = parseMDHDLanguage(b2)
			case "minf":
				boxes(b2, func(t3 string, b3 []byte) {
					if t3 != "stbl" {
						return
					}
					boxes(b3, func(t4 string, b4 []byte) {
						if t4 == "stsd" {
							sampleDesc = b4
						}
					})
				})
			}
		})
	})
	if sampleDesc == nil {
		return
	}
	switch handler {
	case "vide":
		if info.Video == nil {
			if v := parseVisualSampleEntry(sampleDesc); v != nil {
				info.Video = v
			}
		}
	case "soun":
		if a := parseAudioSampleEntry(sampleDesc); a != nil {
			a.Language = lang
			info.Audio = append(info.Audio, *a)
		}
	}
}

// parseHDLR returns the four-character handler type ("vide", "soun").
func parseHDLR(b []byte) string {
	if len(b) < 12 {
		return ""
	}
	return string(b[8:12])
}

// parseMDHDLanguage reads the packed ISO-639-2 language from the media header
// (three 5-bit values, each offset by 0x60).
func parseMDHDLanguage(b []byte) string {
	r := &byteReader{b: b}
	ver, ok := r.u8()
	if !ok || !r.skip(3) {
		return ""
	}
	skip := 16 // v0: creation(4) modification(4) timescale(4) duration(4)
	if ver == 1 {
		skip = 28 // 8 + 8 + 4 + 8
	}
	if !r.skip(skip) {
		return ""
	}
	packed, ok := r.u16()
	if !ok {
		return ""
	}
	out := []byte{
		byte((packed>>10)&0x1F) + 0x60,
		byte((packed>>5)&0x1F) + 0x60,
		byte(packed&0x1F) + 0x60,
	}
	for _, c := range out {
		if c < 'a' || c > 'z' {
			return ""
		}
	}
	return normalizeLanguage(string(out))
}

// Byte offsets inside a sample entry, from ISO/IEC 14496-12, measured from the
// start of the box BODY (i.e. past size+type). Both entry kinds open with the
// same 8-byte SampleEntry preamble: reserved(6) data_reference_index(2).
//
//	VisualSampleEntry  8 pre_defined(2) reserved(2) pre_defined(12)
//	                  24 width(2) 26 height(2)
//	                  28 horizresolution(4) vertresolution(4) reserved(4)
//	                  40 frame_count(2) 42 compressorname(32) 74 depth(2)
//	                  76 pre_defined(2)  →  78 sub-boxes (avcC/hvcC/colr/dvcC)
//	AudioSampleEntry   8 version(2) revision(2) vendor(4)
//	                  16 channelcount(2) 18 samplesize(2) 20 pre_defined(2)
//	                  22 reserved(2) 24 samplerate(4)  →  28 sub-boxes
const (
	visualWidthOffset  = 24
	visualSubBoxOffset = 78
	audioChannelOffset = 16
	audioSubBoxOffset  = 28
)

// parseVisualSampleEntry reads the first video entry out of an stsd box.
func parseVisualSampleEntry(stsd []byte) *VideoInfo {
	entries := stsdEntries(stsd)
	for _, e := range entries {
		codec, dv := mp4VideoCodec(e.format)
		if codec == "" {
			continue
		}
		v := &VideoInfo{Codec: codec}
		if dv {
			v.HDR = append(v.HDR, DV)
		}
		r := &byteReader{b: e.body, pos: visualWidthOffset}
		if w, ok := r.u16(); ok {
			v.Width = int(w)
		}
		if h, ok := r.u16(); ok {
			v.Height = int(h)
		}
		if len(e.body) > visualSubBoxOffset {
			parseVisualSubBoxes(e.body[visualSubBoxOffset:], v)
		}
		return v
	}
	return nil
}

func parseVisualSubBoxes(b []byte, v *VideoInfo) {
	boxes(b, func(typ string, body []byte) {
		switch typ {
		case "hvcC":
			if d := bitDepthHVCC(body); d > 0 {
				v.BitDepth = d
			}
		case "avcC":
			if d := bitDepthAVCC(body); d > 0 {
				v.BitDepth = d
			}
		case "av1C":
			if d := bitDepthAV1C(body); d > 0 {
				v.BitDepth = d
			}
		case "dvcC", "dvvC":
			v.HDR = append(v.HDR, DV)
		case "colr":
			parseColr(body, v)
		}
	})
}

// parseColr reads the colour information box. Only the 'nclx' flavour carries
// the transfer function we care about; 'prof'/'rICC' are ICC payloads.
func parseColr(b []byte, v *VideoInfo) {
	if len(b) < 10 {
		return
	}
	switch string(b[0:4]) {
	case "nclx", "nclc":
	default:
		return
	}
	transfer := int(b[6])<<8 | int(b[7])
	switch transfer {
	case transferPQ:
		v.HDR = append(v.HDR, HDR10)
	case transferHLG:
		v.HDR = append(v.HDR, HLG)
	}
}

func parseAudioSampleEntry(stsd []byte) *AudioInfo {
	for _, e := range stsdEntries(stsd) {
		codec := mp4AudioCodec(e.format)
		if codec == "" {
			continue
		}
		a := &AudioInfo{Codec: codec}
		r := &byteReader{b: e.body, pos: audioChannelOffset}
		if ch, ok := r.u16(); ok && ch < 64 {
			a.Channels = int(ch)
		}
		// The AudioSampleEntry channel count lies for (E-)AC-3: muxers write 2
		// there for a 5.1 track. The real layout is in the codec box, so let
		// dac3/dec3 correct what the entry claimed.
		if len(e.body) > audioSubBoxOffset {
			boxes(e.body[audioSubBoxOffset:], func(typ string, body []byte) {
				switch typ {
				case "dec3":
					// data_rate(13) num_ind_sub(3) | fscod(2) bsid(5)
					// reserved(1) | asvc(1) bsmod(3) acmod(3) lfeon(1)
					if len(body) >= 4 {
						setChannels(a, int(body[3]>>1)&0x07, int(body[3])&0x01)
					}
				case "dac3":
					// fscod(2) bsid(5) bsmod(3) acmod(3) lfeon(1) …
					if len(body) >= 2 {
						setChannels(a, int(body[1]>>3)&0x07, int(body[1]>>2)&0x01)
					}
				}
			})
		}
		return a
	}
	return nil
}

func setChannels(a *AudioInfo, acmod, lfe int) {
	if n := acmodChannels(acmod) + lfe; n > 0 {
		a.Channels = n
	}
}

// acmodChannels maps the AC-3 audio coding mode onto a channel count.
func acmodChannels(acmod int) int {
	switch acmod {
	case 0:
		return 2 // dual mono
	case 1:
		return 1
	case 2:
		return 2
	case 3, 4:
		return 3
	case 5, 6:
		return 4
	case 7:
		return 5
	}
	return 0
}

type sampleEntry struct {
	format string
	body   []byte // everything after the 8-byte box header
}

// stsdEntries splits a sample description box into its entries.
func stsdEntries(stsd []byte) []sampleEntry {
	r := &byteReader{b: stsd}
	if !r.skip(4) { // version + flags
		return nil
	}
	count, ok := r.u32()
	if !ok {
		return nil
	}
	if count > 64 {
		count = 64 // a sane stsd has one or two entries
	}
	var out []sampleEntry
	rest, ok := r.bytes(r.left())
	if !ok {
		return nil
	}
	boxes(rest, func(typ string, body []byte) {
		if len(out) < int(count) {
			out = append(out, sampleEntry{format: typ, body: body})
		}
	})
	return out
}

// mp4VideoCodec maps a sample entry format onto monarr's codec vocabulary and
// reports whether the format itself declares Dolby Vision.
func mp4VideoCodec(format string) (codec string, dv bool) {
	switch strings.ToLower(format) {
	case "avc1", "avc2", "avc3", "avc4":
		return "h264", false
	case "dva1", "dvav":
		return "h264", true
	case "hvc1", "hev1", "hvc2", "hev2":
		return "hevc", false
	case "dvh1", "dvhe":
		return "hevc", true
	case "av01":
		return "av1", false
	case "dav1":
		return "av1", true
	case "vp09":
		return "vp9", false
	case "vp08":
		return "vp8", false
	case "mp4v":
		return "mpeg4", false
	case "mp2v", "mpeg":
		return "mpeg2", false
	case "vc-1", "ovc1":
		return "vc1", false
	}
	return "", false
}

func mp4AudioCodec(format string) string {
	switch strings.ToLower(format) {
	case "mp4a":
		return "aac"
	case "ec-3", "ec+3":
		return "eac3"
	case "ac-3":
		return "ac3"
	case "ac-4":
		return "ac4"
	case "dtsc", "dtse", "dtsh", "dtsl", "dtsx":
		return "dts"
	case "mlpa":
		return "truehd"
	case "flac":
		return "flac"
	case "opus":
		return "opus"
	case "alac":
		return "alac"
	case "sowt", "twos", "lpcm", "in24", "in32", "ipcm", "raw ":
		return "pcm"
	case ".mp3", "mp3 ":
		return "mp3"
	}
	return ""
}
