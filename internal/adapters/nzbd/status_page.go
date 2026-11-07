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
	Entries        []historyEntry `json:"entries"`
	Incarnation    string         `json:"incarnation"`
	HeadSeq        int64          `json:"head_seq"`
	RetainedMinSeq int64          `json:"retained_min_seq"`
}
type statusStream interface{ readStatusJSON(io.Reader) error }

func (p *statusQueue) readStatusJSON(body io.Reader) error {
	rows, _, err := readStatusPage[job](body, "jobs", 10000)
	p.Jobs = rows
	return err
}
func (p *statusHistory) readStatusJSON(body io.Reader) error {
	rows, meta, err := readStatusPage[historyEntry](body, "entries", 100)
	p.Entries = rows
	if err == nil {
		// The history continuity fields (contract section 2.5); absent on a
		// Runner older than contract v2, which reads as incarnation "".
		for key, dst := range map[string]any{"incarnation": &p.Incarnation, "head_seq": &p.HeadSeq, "retained_min_seq": &p.RetainedMinSeq} {
			if raw, ok := meta[key]; ok && string(raw) != "null" {
				if e := json.Unmarshal(raw, dst); e != nil {
					return fmt.Errorf("status response %s: %w", key, e)
				}
			}
		}
	}
	return err
}

// statusPageMeta are the top-level scalar fields a status page may carry
// beside its collection.
var statusPageMeta = map[string]bool{"incarnation": true, "head_seq": true, "retained_min_seq": true}

func readStatusPage[T any](body io.Reader, collection string, limit int) ([]T, map[string]json.RawMessage, error) {
	r := bufio.NewReader(body)
	rows := []T{}
	remaining := statusMetadataBudget
	found := false
	meta, err := statusObject(r, func(name string) (json.RawMessage, error) {
		if statusPageMeta[name] {
			return statusValue(r)
		}
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
				case "kind", "torrent_role", "id", "job", "name", "status", "size_bytes", "downloaded_bytes", "pp_done", "ready", "stages", "control", "final_dir", "seq",
					// pipeline contract v2: the projected status and the typed terminal.
					"status_projected", "transfer", "outcome", "reason", "fetch_cause", "stage", "message", "blames_release",
					"salvage", "keep_on_failure", "fetched", "publication":
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
		return nil, nil, err
	}
	if !found {
		return nil, nil, fmt.Errorf("status response has no %s array", collection)
	}
	if _, err := jsonNext(r); err != io.EOF {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("trailing data after status response")
	}
	return rows, meta, nil
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

// keptStatusParams are the history/queue params Curator reads; the rest are
// drained unread.
var keptStatusParams = map[string]bool{
	TransferParam: true, "*Control:v1": true, "Failure:Files": true,
	"Delivery:Path": true, "Delivery:Generation": true, "Delivery:ManifestDigest": true,
	"Failure:Reason": true, "Failure:FetchCause": true, "Failure:BlamesRelease": true, "Failure:Salvage": true,
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
		keep := keptStatusParams[name]
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
