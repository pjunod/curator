package nzbd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/pjunod/monarr/internal/ports"
)

// Delivery manifests are independent journal entries. Bound retained bytes per
// field, drain unsupported entries, and preserve their sequence and identity so
// one large or malformed manifest cannot hide later publications. The complete
// response also has a byte budget; transport/truncated JSON never advances it.
const deliveryFieldBudget = 8 << 20
const deliveryPageBudget = 16 << 20

func readDeliveryPage(body io.Reader, limit int) (page ports.RunnerDeliveryPage, resultErr error) {
	budget := &io.LimitedReader{R: body, N: deliveryPageBudget + 1}
	defer func() {
		if budget.N == 0 {
			resultErr = fmt.Errorf("nzbd: delivery response exceeds 16 MiB")
		}
	}()
	r := bufio.NewReader(budget)
	if err := jsonPunctuation(r, '{'); err != nil {
		return page, err
	}
	count := 0
	pageKeys := map[string]bool{}
	allowEnd := true
	for {
		next, err := jsonNext(r)
		if err != nil {
			return page, err
		}
		if next == '}' {
			if !allowEnd {
				return page, fmt.Errorf("nzbd: trailing comma in delivery page")
			}
			_, _ = r.ReadByte()
			break
		}
		key, _, err := boundedJSONValue(r, maxDeliveryIdentityBytes)
		if err != nil {
			return page, err
		}
		var name string
		if err := json.Unmarshal(key, &name); err != nil {
			return page, err
		}
		if pageKeys[name] || len(pageKeys) >= 16 {
			return page, fmt.Errorf("nzbd: duplicate or excessive delivery page fields")
		}
		pageKeys[name] = true
		allowEnd = true
		if err := jsonPunctuation(r, ':'); err != nil {
			return page, err
		}
		if name == "deliveries" {
			if err := jsonPunctuation(r, '['); err != nil {
				return page, err
			}
			rowEndAllowed := true
			for {
				next, err := jsonNext(r)
				if err != nil {
					return page, err
				}
				if next == ']' {
					if !rowEndAllowed {
						return page, fmt.Errorf("nzbd: trailing comma in delivery array")
					}
					_, _ = r.ReadByte()
					break
				}
				rowEndAllowed = true
				count++
				if count > limit {
					return page, fmt.Errorf("nzbd: delivery page returned %d rows for limit %d (row limit exceeded)", count, limit)
				}
				fields, invalid, err := readDeliveryFields(r)
				if err != nil {
					return page, err
				}
				raw, err := json.Marshal(fields)
				if err != nil {
					return page, err
				}
				var wire runnerDeliveryWire
				if err := json.Unmarshal(raw, &wire); err != nil {
					invalid = "malformed delivery entry"
				}
				if detail, ok := fields["discoveryError"]; ok {
					var reason string
					if json.Unmarshal(detail, &reason) == nil && reason != "" {
						invalid = reason
					}
				}
				delivery, err := convertRunnerDelivery(wire)
				if err != nil && invalid == "" {
					invalid = err.Error()
				}
				if invalid != "" {
					// Parse identity independently: a bad file/type must not erase the
					// sequence that permits bounded progress through the journal.
					var sequence uint64
					var id string
					_ = json.Unmarshal(fields["sequence"], &sequence)
					_ = json.Unmarshal(fields["deliveryId"], &id)
					if sequence == 0 {
						return page, fmt.Errorf("nzbd: invalid delivery has no journal sequence")
					}
					if len(id) > maxDeliveryIdentityBytes {
						id = ""
					}
					page.Rejected = append(page.Rejected, ports.RunnerDeliveryRejection{Sequence: sequence, DeliveryID: id, Reason: invalid})
				} else {
					page.Deliveries = append(page.Deliveries, delivery)
				}
				next, err = jsonNext(r)
				if err != nil {
					return page, err
				}
				if next == ',' {
					_, _ = r.ReadByte()
					rowEndAllowed = false
					continue
				}
				if next != ']' {
					return page, fmt.Errorf("nzbd: malformed delivery array")
				}
			}
		} else {
			value, oversized, err := boundedJSONValue(r, maxDeliveryIdentityBytes)
			if err != nil || oversized {
				return page, fmt.Errorf("nzbd: invalid delivery page metadata: %v", err)
			}
			switch name {
			case "protocolVersion":
				if err := json.Unmarshal(value, &page.ProtocolVersion); err != nil {
					return page, err
				}
			case "nextCursor":
				if err := json.Unmarshal(value, &page.NextCursor); err != nil {
					return page, err
				}
			}
		}
		next, err = jsonNext(r)
		if err != nil {
			return page, err
		}
		if next == ',' {
			_, _ = r.ReadByte()
			allowEnd = false
			continue
		}
		if next != '}' {
			return page, fmt.Errorf("nzbd: malformed delivery page")
		}
	}
	if page.ProtocolVersion != 1 {
		return page, fmt.Errorf("nzbd: unsupported delivery protocol version %d", page.ProtocolVersion)
	}
	// No trailing content, including a budget-truncated response, is accepted.
	if _, err := jsonNext(r); err != io.EOF {
		return page, fmt.Errorf("nzbd: trailing delivery response")
	}
	return page, nil
}

func readDeliveryFields(r *bufio.Reader) (map[string]json.RawMessage, string, error) {
	fields := map[string]json.RawMessage{}
	if err := jsonPunctuation(r, '{'); err != nil {
		return nil, "", err
	}
	invalid := ""
	remaining := deliveryFieldBudget
	count := 0
	allowEnd := true
	for {
		next, err := jsonNext(r)
		if err != nil {
			return nil, "", err
		}
		if next == '}' {
			if !allowEnd {
				return nil, "", fmt.Errorf("nzbd: trailing comma in delivery entry")
			}
			_, _ = r.ReadByte()
			return fields, invalid, nil
		}
		key, over, err := boundedJSONValue(r, maxDeliveryIdentityBytes)
		if err != nil || over {
			return nil, "", fmt.Errorf("nzbd: malformed delivery field name")
		}
		var name string
		if err := json.Unmarshal(key, &name); err != nil {
			return nil, "", err
		}
		if err := jsonPunctuation(r, ':'); err != nil {
			return nil, "", err
		}
		count++
		budget := remaining
		identity := name == "sequence" || name == "deliveryId" || name == "discoveryError"
		if count > 32 {
			invalid = "delivery entry exceeds 32 fields; publication retained for review"
			budget = 0
		}
		if identity {
			budget = maxDeliveryPathBytes
		}
		value, over, err := boundedJSONValue(r, budget)
		if err != nil {
			return nil, "", err
		}
		if _, duplicate := fields[name]; duplicate {
			invalid = "duplicate delivery field"
		}
		if over {
			invalid = "delivery entry exceeds 8 MiB; publication retained for review"
			value = json.RawMessage("null")
		}
		if !json.Valid(value) {
			invalid = "malformed delivery field"
			value = json.RawMessage("null")
		}
		if identity || count <= 32 {
			fields[name] = value
			remaining -= len(key) + len(value)
			if remaining < 0 {
				remaining = 0
				invalid = "delivery entry exceeds 8 MiB; publication retained for review"
			}
		}
		allowEnd = true
		next, err = jsonNext(r)
		if err != nil {
			return nil, "", err
		}
		if next == ',' {
			_, _ = r.ReadByte()
			allowEnd = false
			continue
		}
		if next != '}' {
			return nil, "", fmt.Errorf("nzbd: malformed delivery object")
		}
	}
}

func jsonNext(r *bufio.Reader) (byte, error) {
	for {
		b, err := r.Peek(1)
		if err != nil {
			return 0, err
		}
		if bytes.IndexByte([]byte(" \t\r\n"), b[0]) < 0 {
			return b[0], nil
		}
		_, _ = r.ReadByte()
	}
}
func jsonPunctuation(r *bufio.Reader, want byte) error {
	b, err := jsonNext(r)
	if err != nil {
		return err
	}
	if b != want {
		return fmt.Errorf("nzbd: expected JSON %q", want)
	}
	_, err = r.ReadByte()
	return err
}

// boundedJSONValue frames a value without allocating its discarded suffix.
// JSON syntax is validated by Unmarshal for retained values. Even rejected
// values must have balanced containers and complete strings before proceeding.
func boundedJSONValue(r *bufio.Reader, budget int) ([]byte, bool, error) {
	first, err := jsonNext(r)
	if err != nil {
		return nil, false, err
	}
	out := make([]byte, 0, 256)
	stack := make([]byte, 0, 16)
	quoted, escaped, oversized := false, false, false
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, false, err
		}
		if len(out) < budget {
			out = append(out, b)
		} else {
			oversized = true
		}
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
			}
		} else {
			switch b {
			case '"':
				quoted = true
			case '{', '[':
				stack = append(stack, b)
				if len(stack) > 64 {
					return nil, false, fmt.Errorf("nzbd: delivery JSON nesting exceeds limit")
				}
			case '}', ']':
				if len(stack) == 0 || (b == '}' && stack[len(stack)-1] != '{') || (b == ']' && stack[len(stack)-1] != '[') {
					return nil, false, fmt.Errorf("nzbd: mismatched delivery JSON container")
				}
				stack = stack[:len(stack)-1]
			}
		}
		if !quoted && len(stack) == 0 {
			if first == '{' || first == '[' || first == '"' {
				return out, oversized, nil
			}
			next, err := r.Peek(1)
			if err == io.EOF {
				return out, oversized, nil
			}
			if err != nil {
				return nil, false, err
			}
			if bytes.IndexByte([]byte(",]} \t\r\n"), next[0]) >= 0 {
				return out, oversized, nil
			}
		}
	}
}

// Keep page parsing isolated from admission, which only consults status.
func (c *Client) listDeliveryPage(req *http.Request, limit int) (ports.RunnerDeliveryPage, error) {
	c.authorize(req)
	req.Header.Set("X-Nzbd-Client", clientHeader())
	resp, err := c.http.Do(req)
	if err != nil {
		return ports.RunnerDeliveryPage{}, fmt.Errorf("nzbd: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
		closeErr := resp.Body.Close()
		message := "unexpected delivery response"
		if resp.StatusCode == http.StatusUnauthorized {
			message = "authentication rejected"
		} else {
			var body struct {
				Error string `json:"error"`
			}
			if len(raw) <= 64*1024 && json.Unmarshal(raw, &body) == nil && body.Error != "" {
				message = body.Error
			}
		}
		return ports.RunnerDeliveryPage{}, errors.Join(apiError(resp, message), readErr, closeErr)
	}
	body := &deliveryTransportBody{r: resp.Body}
	page, parseErr := readDeliveryPage(body, limit)
	closeErr := resp.Body.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("nzbd: close delivery response: %w", closeErr)
	}
	// Only a page-content problem (oversize, malformed, protocol violation)
	// falls back to one row at a time. A transport failure reading or closing
	// the body is not a page too large to read: a smaller page would only
	// repeat it.
	if parseErr != nil && body.err == nil && closeErr == nil && limit > 1 && req.Context().Err() == nil {
		retry := req.Clone(req.Context())
		query := retry.URL.Query()
		query.Set("limit", "1")
		retry.URL.RawQuery = query.Encode()
		fallback, fallbackErr := c.listDeliveryPage(retry, 1)
		if fallbackErr != nil {
			return fallback, errors.Join(fmt.Errorf("nzbd: bad delivery response (read delivery response): %w", parseErr), fallbackErr)
		}
		return fallback, nil
	}
	if parseErr != nil {
		parseErr = fmt.Errorf("nzbd: bad delivery response (read delivery response): %w", parseErr)
	}
	return page, errors.Join(parseErr, closeErr)
}

// deliveryTransportBody records a failure of the response body itself, so a
// transport error is told apart from a page whose content could not be used.
// A clean end of body is not a transport failure: a page cut short there is
// malformed content.
type deliveryTransportBody struct {
	r   io.Reader
	err error
}

func (b *deliveryTransportBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.err == nil {
		b.err = err
	}
	return n, err
}
