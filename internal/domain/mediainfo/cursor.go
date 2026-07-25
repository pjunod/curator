package mediainfo

import (
	"errors"
	"io"
)

// cursor is a budgeted forward reader over an io.ReaderAt.
//
// Every byte a walker consumes goes through here, which is how the "never read
// the whole file" rule is enforced mechanically instead of by discipline: once
// spent exceeds budget, every read fails with ErrBudget and the walker unwinds
// with whatever it had learned so far. Seeking backwards is allowed (MP4's moov
// can sit behind us) and costs nothing; only bytes actually read count.
type cursor struct {
	r      io.ReaderAt
	size   int64
	pos    int64
	spent  int64
	budget int64
}

func newCursor(r io.ReaderAt, size, budget int64) *cursor {
	return &cursor{r: r, size: size, budget: budget}
}

// read returns exactly n bytes at the current position and advances past them.
// n is bounds-checked against both the file size and the remaining budget
// before a single byte is allocated: a corrupt length field claiming 2^60 bytes
// must not turn into a 2^60-byte make().
func (c *cursor) read(n int64) ([]byte, error) {
	if n < 0 {
		return nil, ErrMalformed
	}
	if n == 0 {
		return nil, nil
	}
	if c.pos < 0 || c.pos+n > c.size {
		return nil, io.ErrUnexpectedEOF
	}
	if c.spent+n > c.budget {
		return nil, ErrBudget
	}
	buf := make([]byte, n)
	got, err := c.r.ReadAt(buf, c.pos)
	c.spent += int64(got)
	c.pos += int64(got)
	if got < int(n) {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return buf[:got], err
	}
	return buf, nil
}

// skip advances the position without spending budget.
func (c *cursor) skip(n int64) error {
	if n < 0 || c.pos+n > c.size {
		return io.ErrUnexpectedEOF
	}
	c.pos += n
	return nil
}

func (c *cursor) seek(off int64) error {
	if off < 0 || off > c.size {
		return io.ErrUnexpectedEOF
	}
	c.pos = off
	return nil
}

func (c *cursor) remaining() int64 { return c.size - c.pos }

// byteReader walks an in-memory buffer with the same bounds discipline. Used
// for structures already read into memory (codec-private blobs, MP4 boxes).
type byteReader struct {
	b   []byte
	pos int
}

func (b *byteReader) u8() (uint8, bool) {
	if b.pos+1 > len(b.b) {
		return 0, false
	}
	v := b.b[b.pos]
	b.pos++
	return v, true
}

func (b *byteReader) u16() (uint16, bool) {
	if b.pos+2 > len(b.b) {
		return 0, false
	}
	v := uint16(b.b[b.pos])<<8 | uint16(b.b[b.pos+1])
	b.pos += 2
	return v, true
}

func (b *byteReader) u32() (uint32, bool) {
	if b.pos+4 > len(b.b) {
		return 0, false
	}
	v := uint32(b.b[b.pos])<<24 | uint32(b.b[b.pos+1])<<16 |
		uint32(b.b[b.pos+2])<<8 | uint32(b.b[b.pos+3])
	b.pos += 4
	return v, true
}

func (b *byteReader) u64() (uint64, bool) {
	hi, ok := b.u32()
	if !ok {
		return 0, false
	}
	lo, ok := b.u32()
	if !ok {
		return 0, false
	}
	return uint64(hi)<<32 | uint64(lo), true
}

func (b *byteReader) bytes(n int) ([]byte, bool) {
	if n < 0 || b.pos+n > len(b.b) {
		return nil, false
	}
	v := b.b[b.pos : b.pos+n]
	b.pos += n
	return v, true
}

func (b *byteReader) skip(n int) bool {
	if n < 0 || b.pos+n > len(b.b) {
		b.pos = len(b.b)
		return false
	}
	b.pos += n
	return true
}

func (b *byteReader) left() int { return len(b.b) - b.pos }
