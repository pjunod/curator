package nzbd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

const maxDeliveryPage = 100
const (
	maxDeliveryFiles         = 10000
	maxDeliveryPathBytes     = 4096
	maxDeliveryIdentityBytes = 1024
)

type runnerDeliveryWire struct {
	ProtocolVersion       int                       `json:"protocolVersion"`
	Sequence              uint64                    `json:"sequence"`
	DeliveryID            string                    `json:"deliveryId"`
	PublicationGeneration string                    `json:"publicationGeneration"`
	SourceArtifactID      string                    `json:"sourceArtifactId,omitempty"`
	SourceGeneration      string                    `json:"sourceGeneration,omitempty"`
	ArtifactID            string                    `json:"artifactId"`
	JobID                 uint32                    `json:"jobId"`
	RequestToken          string                    `json:"requestToken"`
	RunnerInstanceID      string                    `json:"runnerInstanceId"`
	PublicationPath       string                    `json:"publicationPath"`
	ManifestDigest        string                    `json:"manifestDigest"`
	Files                 []ports.RunnerFile        `json:"files"`
	TerminalOutcome       string                    `json:"terminalOutcome,omitempty"`
	FailureStage          string                    `json:"failureStage,omitempty"`
	FailureCause          string                    `json:"failureCause,omitempty"`
	KeepSourceOnFailure   bool                      `json:"keepSourceOnFailure,omitempty"`
	FileOutcomes          []ports.RunnerFileOutcome `json:"fileOutcomes,omitempty"`
	ReadyAt               json.Number               `json:"readyAt"`
	SourceCleanup         json.RawMessage           `json:"sourceCleanup,omitempty"`
}

type runnerDeliveryPageWire struct {
	ProtocolVersion int                  `json:"protocolVersion"`
	Deliveries      []runnerDeliveryWire `json:"deliveries"`
	NextCursor      uint64               `json:"nextCursor"`
}

type runnerRequestAdmissionWire struct {
	ProtocolVersion     int    `json:"protocolVersion"`
	RequestToken        string `json:"requestToken"`
	JobID               uint32 `json:"jobId"`
	Generation          string `json:"generation"`
	RunnerInstanceID    string `json:"runnerInstanceId"`
	Outcome             string `json:"outcome"`
	TerminalOutcome     string `json:"terminalOutcome,omitempty"`
	PublicationPath     string `json:"publicationPath,omitempty"`
	SourceArtifactID    string `json:"sourceArtifactId,omitempty"`
	SourceGeneration    string `json:"sourceGeneration,omitempty"`
	FailureStage        string `json:"failureStage,omitempty"`
	FailureCause        string `json:"failureCause,omitempty"`
	CustodyState        string `json:"custodyState,omitempty"`
	KeepSourceOnFailure bool   `json:"keepSourceOnFailure,omitempty"`
	SourceCleanupStatus string `json:"sourceCleanupStatus,omitempty"`
}

func (w runnerRequestAdmissionWire) portValue() ports.RequestAdmission {
	return ports.RequestAdmission{
		ProtocolVersion: w.ProtocolVersion, RequestToken: w.RequestToken,
		JobID: strconv.FormatUint(uint64(w.JobID), 10), Generation: w.Generation,
		RunnerInstanceID: w.RunnerInstanceID, Outcome: w.Outcome,
		TerminalOutcome: w.TerminalOutcome, PublicationPath: w.PublicationPath,
		SourceArtifactID: w.SourceArtifactID, SourceGeneration: w.SourceGeneration,
		FailureStage: w.FailureStage, FailureCause: w.FailureCause,
		CustodyState: w.CustodyState, KeepSourceOnFailure: w.KeepSourceOnFailure,
		SourceCleanupStatus: w.SourceCleanupStatus,
	}
}

// ListDeliveries implements ports.DeliveryProtocol.
func (c *Client) ListDeliveries(ctx context.Context, after uint64, limit int) (ports.RunnerDeliveryPage, error) {
	if limit < 1 || limit > maxDeliveryPage {
		return ports.RunnerDeliveryPage{}, fmt.Errorf("nzbd: delivery page limit must be between 1 and %d", maxDeliveryPage)
	}
	path := "/api/v1/deliveries?after=" + strconv.FormatUint(after, 10) + "&limit=" + strconv.Itoa(limit)
	var wire runnerDeliveryPageWire
	if err := c.deliveryRequest(ctx, http.MethodGet, path, nil, &wire); err != nil {
		return ports.RunnerDeliveryPage{}, err
	}
	if wire.ProtocolVersion != 1 {
		return ports.RunnerDeliveryPage{}, fmt.Errorf("nzbd: unsupported delivery protocol version %d", wire.ProtocolVersion)
	}
	if len(wire.Deliveries) > limit {
		return ports.RunnerDeliveryPage{}, fmt.Errorf("nzbd: delivery page returned %d rows for limit %d", len(wire.Deliveries), limit)
	}
	page := ports.RunnerDeliveryPage{ProtocolVersion: wire.ProtocolVersion, NextCursor: wire.NextCursor, Deliveries: make([]ports.RunnerDelivery, 0, len(wire.Deliveries))}
	for _, d := range wire.Deliveries {
		converted, err := convertRunnerDelivery(d)
		if err != nil {
			return ports.RunnerDeliveryPage{}, err
		}
		page.Deliveries = append(page.Deliveries, converted)
	}
	return page, nil
}

// GetDelivery refreshes durable mutable progress for a delivery that has
// already been accepted and therefore no longer appears in the pending list.
func (c *Client) GetDelivery(ctx context.Context, deliveryID string) (ports.RunnerDelivery, error) {
	if deliveryID == "" || len(deliveryID) > maxDeliveryIdentityBytes {
		return ports.RunnerDelivery{}, fmt.Errorf("nzbd: invalid delivery ID")
	}
	var d runnerDeliveryWire
	if err := c.deliveryRequest(ctx, http.MethodGet, "/api/v1/deliveries/"+url.PathEscape(deliveryID), nil, &d); err != nil {
		return ports.RunnerDelivery{}, err
	}
	return convertRunnerDelivery(d)
}

// RetryDeliverySourceCleanup requests a retry of Runner-owned processing
// source cleanup. It never changes Curator's accepted publication files.
func (c *Client) RetryDeliverySourceCleanup(ctx context.Context, deliveryID string) (ports.RunnerDelivery, error) {
	if deliveryID == "" || len(deliveryID) > maxDeliveryIdentityBytes {
		return ports.RunnerDelivery{}, fmt.Errorf("nzbd: invalid delivery ID")
	}
	path := "/api/v1/deliveries/" + url.PathEscape(deliveryID) + "/source-cleanup/retry"
	var d runnerDeliveryWire
	if err := c.deliveryRequest(ctx, http.MethodPost, path, []byte("{}"), &d); err != nil {
		return ports.RunnerDelivery{}, err
	}
	return convertRunnerDelivery(d)
}

func convertRunnerDelivery(d runnerDeliveryWire) (ports.RunnerDelivery, error) {
	if d.ProtocolVersion != 1 {
		return ports.RunnerDelivery{}, fmt.Errorf("nzbd: unsupported delivery protocol version %d", d.ProtocolVersion)
	}
	if len(d.DeliveryID) == 0 || len(d.DeliveryID) > maxDeliveryIdentityBytes || len(d.PublicationGeneration) == 0 || len(d.PublicationGeneration) > maxDeliveryIdentityBytes || len(d.ManifestDigest) > 128 || len(d.ArtifactID) > maxDeliveryIdentityBytes || len(d.RequestToken) > maxDeliveryIdentityBytes || len(d.RunnerInstanceID) > maxDeliveryIdentityBytes || len(d.PublicationPath) > maxDeliveryPathBytes || len(d.Files) == 0 || len(d.Files) > maxDeliveryFiles || len(d.TerminalOutcome) > maxDeliveryIdentityBytes || len(d.FailureStage) > maxDeliveryIdentityBytes || len(d.FailureCause) > maxDeliveryIdentityBytes || len(d.FileOutcomes) > maxDeliveryFiles {
		return ports.RunnerDelivery{}, fmt.Errorf("nzbd: delivery %q exceeds protocol size limits", d.DeliveryID)
	}
	for _, file := range d.Files {
		if len(file.Path) == 0 || len(file.Path) > maxDeliveryPathBytes || len(file.SHA256) != 64 || file.SizeBytes < 0 {
			return ports.RunnerDelivery{}, fmt.Errorf("nzbd: delivery %q contains an oversized or malformed file entry", d.DeliveryID)
		}
	}
	for _, outcome := range d.FileOutcomes {
		if len(outcome.SourcePath) == 0 || len(outcome.SourcePath) > maxDeliveryPathBytes || len(outcome.Classification) > maxDeliveryIdentityBytes || len(outcome.Outcome) > maxDeliveryIdentityBytes || len(outcome.Reason) > maxDeliveryPathBytes || len(outcome.SourceCleanupStatus) > maxDeliveryIdentityBytes || (outcome.PublicationPath != nil && len(*outcome.PublicationPath) > maxDeliveryPathBytes) {
			return ports.RunnerDelivery{}, fmt.Errorf("nzbd: delivery %q contains an oversized file outcome", d.DeliveryID)
		}
	}
	seconds, err := strconv.ParseFloat(d.ReadyAt.String(), 64)
	if err != nil {
		return ports.RunnerDelivery{}, fmt.Errorf("nzbd: malformed readyAt for delivery %q: %w", d.DeliveryID, err)
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * float64(time.Second))
	return ports.RunnerDelivery{ProtocolVersion: 1, Sequence: d.Sequence, DeliveryID: d.DeliveryID, PublicationGeneration: d.PublicationGeneration, SourceArtifactID: d.SourceArtifactID, SourceGeneration: d.SourceGeneration, ArtifactID: d.ArtifactID, JobID: strconv.FormatUint(uint64(d.JobID), 10), RequestToken: d.RequestToken, RunnerInstanceID: d.RunnerInstanceID, PublicationPath: d.PublicationPath, ManifestDigest: d.ManifestDigest, Files: d.Files, TerminalOutcome: d.TerminalOutcome, FailureStage: d.FailureStage, FailureCause: d.FailureCause, KeepSourceOnFailure: d.KeepSourceOnFailure, FileOutcomes: d.FileOutcomes, ReadyAt: time.Unix(whole, nanos).UTC(), SourceCleanup: d.SourceCleanup}, nil
}

// AcceptDelivery implements ports.DeliveryProtocol. Repeating the same
// immutable tuple and receipt ID is the server-side idempotency contract.
func (c *Client) AcceptDelivery(ctx context.Context, deliveryID string, acceptance ports.RunnerDeliveryAcceptance) error {
	if acceptance.ProtocolVersion != 1 || acceptance.PublicationGeneration == "" ||
		acceptance.ManifestDigest == "" || acceptance.ConsumerReceiptID == "" {
		return fmt.Errorf("nzbd: delivery acceptance is missing its immutable identity")
	}
	if deliveryID == "" {
		return fmt.Errorf("nzbd: delivery ID is empty")
	}
	body, err := json.Marshal(acceptance)
	if err != nil {
		return fmt.Errorf("nzbd: encode delivery acceptance: %w", err)
	}
	path := "/api/v1/deliveries/" + url.PathEscape(deliveryID) + "/accept"
	err = c.deliveryRequest(ctx, http.MethodPost, path, body, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %s", ports.ErrDeliveryConflict, apiErr.Message)
	}
	return err
}

func (c *Client) deliveryRequest(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.URL, "/")+path, reader)
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("X-Nzbd-Client", clientHeader())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("nzbd: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	closeErr := resp.Body.Close()
	if err != nil {
		return fmt.Errorf("nzbd: read delivery response: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("nzbd: close delivery response: %w", closeErr)
	}
	if len(raw) > 8<<20 {
		return fmt.Errorf("nzbd: delivery response exceeds 8 MiB")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return apiError(resp, "authentication rejected")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var message struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &message) == nil && message.Error != "" {
			return apiError(resp, message.Error)
		}
		return apiError(resp, "unexpected delivery response")
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("nzbd: bad delivery response: %w", err)
		}
	}
	return nil
}
