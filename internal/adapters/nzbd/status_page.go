package nzbd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Status polls retain bounded metadata, not the full article/file records that
// Runner includes for recovery. Large discarded records still must finish
// before any status from the response becomes authoritative.
const statusFieldBudget = 1 << 20
const statusMetadataBudget = 8 << 20

type statusQueue struct {
	Jobs []job `json:"jobs"`
}
type statusHistory struct {
	Entries []historyEntry `json:"entries"`
}
type statusStream interface{ readStatusJSON(io.Reader) error }

func (p *statusQueue) readStatusJSON(body io.Reader) error {
	rows, err := readStatusPage[job](body, "jobs", 10000)
	p.Jobs = rows
	return err
}
func (p *statusHistory) readStatusJSON(body io.Reader) error {
	rows, err := readStatusPage[historyEntry](body, "entries", 100)
	p.Entries = rows
	return err
}

func readStatusPage[T any](body io.Reader, collection string, limit int) ([]T, error) {
	r := bufio.NewReader(body)
	rows := []T{}
	remaining := statusMetadataBudget
	found := false
	_, err := statusObject(r, func(name string) (json.RawMessage, error) {
		if name != collection {
			return discardStatusValue(r)
		}
		found = true
		return nil, statusArray(r, limit, func() error {
			fields, err := statusObject(r, func(key string) (json.RawMessage, error) {
				switch key {
				case "params":
					return statusParams(r)
				case "record":
					next, err := jsonNext(r)
					if err != nil {
						return nil, err
					}
					if next == 'n' {
						return statusValue(r)
					}
					counts, err := statusObject(r, func(field string) (json.RawMessage, error) {
						if field == "total_articles" || field == "success_articles" {
							return statusValue(r)
						}
						return discardStatusValue(r)
					})
					if err != nil {
						return nil, err
					}
					return json.Marshal(counts)
				case "kind", "torrent_role", "id", "job", "name", "status", "size_bytes", "downloaded_bytes", "pp_done", "ready", "stages", "control", "final_dir", "seq":
					return statusValue(r)
				default:
					return discardStatusValue(r)
				}
			})
			if err != nil {
				return err
			}
			raw, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			remaining -= len(raw)
			if remaining < 0 {
				return fmt.Errorf("status response metadata exceeds 8 MiB")
			}
			var row T
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("status response has no %s array", collection)
	}
	if _, err := jsonNext(r); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("trailing data after status response")
	}
	return rows, nil
}

func statusValue(r *bufio.Reader) (json.RawMessage, error) {
	raw, over, err := boundedJSONValue(r, statusFieldBudget)
	if err != nil {
		return nil, err
	}
	if over {
		return nil, fmt.Errorf("status metadata field exceeds 1 MiB")
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid status metadata JSON")
	}
	return raw, nil
}
func discardStatusValue(r *bufio.Reader) (json.RawMessage, error) {
	return nil, discardStatusJSON(r, 0)
}

func statusObject(r *bufio.Reader, field func(string) (json.RawMessage, error)) (map[string]json.RawMessage, error) {
	if err := jsonPunctuation(r, '{'); err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	seen := map[string]bool{}
	allowEnd := true
	for {
		next, err := jsonNext(r)
		if err != nil {
			return nil, err
		}
		if next == '}' {
			if !allowEnd {
				return nil, fmt.Errorf("trailing comma in status object")
			}
			_, err := r.ReadByte()
			return fields, err
		}
		if next != '"' {
			return nil, fmt.Errorf("status object field name must be a string")
		}
		key, over, err := boundedJSONValue(r, 4096)
		if err != nil || over {
			return nil, fmt.Errorf("invalid status field name: %v", err)
		}
		var name string
		if err := json.Unmarshal(key, &name); err != nil {
			return nil, err
		}
		if seen[name] || len(seen) >= 256 {
			return nil, fmt.Errorf("duplicate or excessive status fields")
		}
		seen[name] = true
		if err := jsonPunctuation(r, ':'); err != nil {
			return nil, err
		}
		value, err := field(name)
		if err != nil {
			return nil, err
		}
		if value != nil {
			fields[name] = value
		}
		next, err = jsonNext(r)
		if err != nil {
			return nil, err
		}
		if next == ',' {
			_, _ = r.ReadByte()
			allowEnd = false
			continue
		}
		if next != '}' {
			return nil, fmt.Errorf("invalid status object separator")
		}
		allowEnd = true
	}
}

func statusArray(r *bufio.Reader, limit int, element func() error) error {
	if next, err := jsonNext(r); err != nil {
		return err
	} else if next == 'n' {
		raw, err := statusValue(r)
		if err != nil {
			return err
		}
		if string(raw) != "null" {
			return fmt.Errorf("invalid null status array")
		}
		return nil
	}
	if err := jsonPunctuation(r, '['); err != nil {
		return err
	}
	allowEnd := true
	for count := 0; ; count++ {
		next, err := jsonNext(r)
		if err != nil {
			return err
		}
		if next == ']' {
			if !allowEnd {
				return fmt.Errorf("trailing comma in status array")
			}
			_, err := r.ReadByte()
			return err
		}
		if count >= limit {
			return fmt.Errorf("status array exceeds %d entries", limit)
		}
		if err := element(); err != nil {
			return err
		}
		next, err = jsonNext(r)
		if err != nil {
			return err
		}
		if next == ',' {
			_, _ = r.ReadByte()
			allowEnd = false
			continue
		}
		if next != ']' {
			return fmt.Errorf("invalid status array separator")
		}
		allowEnd = true
	}
}

func statusParams(r *bufio.Reader) (json.RawMessage, error) {
	pairs := [][2]string{}
	seen := map[string]bool{}
	err := statusArray(r, 1000000, func() error {
		if err := jsonPunctuation(r, '['); err != nil {
			return err
		}
		key, err := statusValue(r)
		if err != nil {
			return err
		}
		var name string
		if err := json.Unmarshal(key, &name); err != nil {
			return err
		}
		if err := jsonPunctuation(r, ','); err != nil {
			return err
		}
		keep := name == TransferParam || name == "*Control:v1" || name == "Failure:Files"
		if keep {
			if seen[name] {
				return fmt.Errorf("duplicate status parameter %s", name)
			}
			seen[name] = true
			value, err := statusValue(r)
			if err != nil {
				return err
			}
			var decoded string
			if err := json.Unmarshal(value, &decoded); err != nil {
				return err
			}
			pairs = append(pairs, [2]string{name, decoded})
		} else if _, err := discardStatusValue(r); err != nil {
			return err
		}
		return jsonPunctuation(r, ']')
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(pairs)
}
