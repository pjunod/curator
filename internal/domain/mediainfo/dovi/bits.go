package dovi

// bitReader reads an MSB-first bitstream. Errors are sticky: once a read runs
// past the end, every later read returns zero and ok() is false, so a parser
// can read a whole structure and check once — the shape the RPU syntax tables
// are written in.
type bitReader struct {
	b   []byte
	pos int // bit position
	bad bool
}

func newBitReader(b []byte) *bitReader { return &bitReader{b: b} }

func (r *bitReader) ok() bool { return !r.bad }

// left is how many bits remain.
func (r *bitReader) left() int {
	if r.bad {
		return 0
	}
	return len(r.b)*8 - r.pos
}

func (r *bitReader) aligned() bool { return r.pos%8 == 0 }

// bits reads n (0..64) bits as an unsigned integer.
func (r *bitReader) bits(n int) uint64 {
	if r.bad || n < 0 || n > 64 || r.left() < n {
		r.bad = true
		return 0
	}
	var v uint64
	for i := 0; i < n; i++ {
		byteIdx := r.pos >> 3
		bit := (r.b[byteIdx] >> (7 - uint(r.pos&7))) & 1
		v = v<<1 | uint64(bit)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.bits(1) == 1 }

// skip advances n bits without decoding them.
func (r *bitReader) skip(n int) {
	if r.bad || n < 0 || r.left() < n {
		r.bad = true
		return
	}
	r.pos += n
}

// ue reads an Exp-Golomb unsigned value. More than 32 leading zeros cannot
// come from a valid stream and is treated as corruption, which also keeps
// the result inside uint64.
func (r *bitReader) ue() uint64 {
	zeros := 0
	for !r.bad && r.bits(1) == 0 {
		zeros++
		if zeros > 32 {
			r.bad = true
			return 0
		}
	}
	if r.bad {
		return 0
	}
	return (uint64(1)<<uint(zeros) - 1) + r.bits(zeros)
}

// se reads an Exp-Golomb signed value.
func (r *bitReader) se() int64 {
	k := r.ue()
	if k&1 == 1 {
		return int64((k + 1) / 2)
	}
	return -int64(k / 2)
}

// unescape removes HEVC emulation-prevention bytes (00 00 03 → 00 00).
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, x := range b {
		if zeros >= 2 && x == 3 {
			zeros = 0
			continue
		}
		if x == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, x)
	}
	return out
}

// crc32MPEG2 is the CRC the RPU trailer carries: polynomial 0x04C11DB7,
// MSB-first, initial value all ones, no final inversion. Go's hash/crc32 only
// does the reflected form, and this is twelve lines.
func crc32MPEG2(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
