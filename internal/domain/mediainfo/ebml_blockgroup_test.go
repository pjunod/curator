package mediainfo

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ebmlElement encodes one element with an 8-byte size, which every reader
// must accept whatever the writer would have chosen.
func ebmlElement(id byte, body []byte) []byte {
	out := []byte{id, 0x01}
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(body)))
	out = append(out, size[1:]...)
	return append(out, body...)
}

// Muxers that write a reference or duration with a frame put it in a
// BlockGroup instead of a SimpleBlock. The first video frame is read from
// either, and a BlockGroup of another track is stepped over.
func TestFirstVideoFrameInsideABlockGroup(t *testing.T) {
	nal, err := os.ReadFile(filepath.Join("dovi", "testdata", "p81_identity.nal"))
	if err != nil {
		t.Fatal(err)
	}
	au := make([]byte, 4, 4+len(nal))
	binary.BigEndian.PutUint32(au, uint32(len(nal)))
	au = append(au, nal...)
	block := func(track byte) []byte {
		return append([]byte{0x80 | track, 0, 0, 0}, au...) // track vint, timecode, flags
	}
	audioGroup := ebmlElement(idBlockGroup, ebmlElement(idBlock, block(2)))
	videoGroup := ebmlElement(idBlockGroup, append(ebmlElement(0xFB, []byte{0x00}), ebmlElement(idBlock, block(1))...)) // ReferenceBlock first
	cluster := append(audioGroup, videoGroup...)

	c := newCursor(bytes.NewReader(cluster), int64(len(cluster)), 1<<20)
	f := scanFirstVideoBlock(c, 0, int64(len(cluster)), mkvVideoTrack{number: 1, hevc: true, lengthSize: 4})
	if f.rpu == nil || f.rpu.Profile != 8 {
		t.Fatalf("frame facts = %+v, want the profile 8 RPU from the BlockGroup", f)
	}

	// A BlockGroup holding no Block, then the end of the cluster: nothing.
	empty := ebmlElement(idBlockGroup, ebmlElement(0xFB, []byte{0x00}))
	c = newCursor(bytes.NewReader(empty), int64(len(empty)), 1<<20)
	if f := scanFirstVideoBlock(c, 0, int64(len(empty)), mkvVideoTrack{number: 1, hevc: true, lengthSize: 4}); f.rpu != nil {
		t.Errorf("frame facts from an empty BlockGroup = %+v", f)
	}
}
