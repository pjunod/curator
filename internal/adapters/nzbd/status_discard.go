package nzbd

import (
	"bufio"
	"fmt"
)

// Validate ignored recovery data without retaining file lists or proof strings.
func discardStatusJSON(r *bufio.Reader, depth int) error {
	if depth > 64 {
		return fmt.Errorf("status JSON nesting exceeds limit")
	}
	next, err := jsonNext(r)
	if err != nil {
		return err
	}
	if next == '"' {
		return discardStatusString(r)
	}
	if next != '[' && next != '{' {
		_, err := statusValue(r)
		return err
	}
	_, _ = r.ReadByte()
	end := byte(']')
	if next == '{' {
		end = '}'
	}
	b, err := jsonNext(r)
	if err != nil {
		return err
	}
	if b == end {
		_, err = r.ReadByte()
		return err
	}
	for {
		if next == '{' {
			if err := discardStatusString(r); err != nil {
				return err
			}
			if err := jsonPunctuation(r, ':'); err != nil {
				return err
			}
		}
		if err := discardStatusJSON(r, depth+1); err != nil {
			return err
		}
		b, err = jsonNext(r)
		if err != nil {
			return err
		}
		if b == end {
			_, err = r.ReadByte()
			return err
		}
		if err := jsonPunctuation(r, ','); err != nil {
			return err
		}
	}
}

func discardStatusString(r *bufio.Reader) error {
	if err := jsonPunctuation(r, '"'); err != nil {
		return err
	}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch {
		case b == '"':
			return nil
		case b < 0x20:
			return fmt.Errorf("invalid control byte in status JSON string")
		case b == '\\':
			b, err = r.ReadByte()
			if err != nil {
				return err
			}
			switch b {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for i := 0; i < 4; i++ {
					b, err = r.ReadByte()
					if err != nil {
						return err
					}
					if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
						return fmt.Errorf("invalid status JSON unicode escape")
					}
				}
			default:
				return fmt.Errorf("invalid status JSON escape")
			}
		}
	}
}
