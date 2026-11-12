package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Re-match (ADR 0027): POST /library/{id}/rematch/preview, POST
// /library/{id}/rematch, GET /rematch/{operationId}, POST .../cancel.

// rematchRequest turns the body into the service request. A target given by
// provider id that is not in the library is hydrated (preview) — never added
// here.
func (s *Server) rematchRequest(r *http.Request, id int64, in apigen.RematchRequest) (acquisition.RematchRequest, *library.AddRequest, error) {
	req := acquisition.RematchRequest{SourceItemID: id, Blocklist: in.Blocklist}
	if in.CopyId != nil {
		req.TargetCopyID = *in.CopyId
	}
	if in.FileIds != nil {
		req.FileIDs = *in.FileIds
	}
	if in.Choices != nil {
		for _, c := range *in.Choices {
			choice := acquisition.RematchChoice{FileID: c.FileId, Season: -1}
			if c.Season != nil {
				choice.Season = *c.Season
			}
			if c.Episodes != nil {
				choice.Episodes = *c.Episodes
			}
			if c.Leave != nil {
				choice.Leave = *c.Leave
			}
			req.Choices = append(req.Choices, choice)
		}
	}
	if in.RequestId != nil {
		req.RequestID = *in.RequestId
	}
	if in.SourceRevision != nil {
		req.SourceRevision = *in.SourceRevision
	}
	if in.TargetRevision != nil {
		req.TargetRevision = *in.TargetRevision
	}
	if in.Fingerprint != nil {
		req.PlanFingerprint = *in.Fingerprint
	}
	t := in.Target
	if t == nil {
		return req, nil, nil
	}
	if t.ItemId != nil && *t.ItemId != 0 {
		req.TargetItemID = *t.ItemId
		return req, nil, nil
	}
	source, err := s.deps.Store.GetMediaItemFull(r.Context(), id)
	if err != nil {
		return req, nil, err
	}
	add := library.AddRequest{Kind: source.Kind, Monitored: true}
	if t.Kind != nil {
		add.Kind = domain.MediaKind(*t.Kind)
	}
	if t.TmdbId != nil {
		add.TMDBID = *t.TmdbId
	}
	if t.TvdbId != nil {
		add.TVDBID = *t.TvdbId
	}
	if t.ImdbId != nil {
		add.IMDBID = *t.ImdbId
	}
	if t.Olid != nil {
		add.OLID = *t.Olid
	}
	if t.HydrationSource != nil {
		add.HydrationSource = *t.HydrationSource
	}
	probe := domain.MediaItem{Kind: add.Kind, IDs: domain.ExternalIDs{TMDB: add.TMDBID, TVDB: add.TVDBID, IMDB: add.IMDBID, OLID: add.OLID}}
	if existing := s.deps.Acquisition.LibraryItemFor(r.Context(), probe); existing != 0 {
		req.TargetItemID = existing
		return req, nil, nil
	}
	if root, err := s.deps.Acquisition.RematchRootFolder(r.Context(), source, add.Kind); err == nil {
		add.RootFolderID = root.ID
	}
	return req, &add, nil
}

func (s *Server) rematchErr(w http.ResponseWriter, err error) {
	var blocked *acquisition.RematchBlockedError
	switch {
	case errors.As(err, &blocked):
		code := "rematch_blocked"
		writeJSON(w, http.StatusConflict, apigen.RematchConflict{Code: &code, Message: blocked.Error(), Blocked: &apigen.RematchBlocked{
			Reason: apigen.RematchBlockedReason(blocked.Blocked.Reason), Ref: blocked.Blocked.Ref, ItemId: blocked.Blocked.ItemID,
			Message: blocked.Blocked.Message, Retry: blocked.Blocked.Retry}})
	case errors.Is(err, acquisition.ErrRematchStalePreview), errors.Is(err, sqlite.ErrRematchStale):
		writeCodedError(w, http.StatusConflict, "stale", err.Error())
	case errors.Is(err, sqlite.ErrRematchRequestConflict):
		writeCodedError(w, http.StatusConflict, "request_conflict", err.Error())
	case errors.Is(err, ports.ErrMaintenance):
		writeCodedError(w, http.StatusServiceUnavailable, "maintenance", err.Error())
	case errors.Is(err, acquisition.ErrImportTargetBusy):
		writeCodedError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, acquisition.ErrInvalidRematch):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, library.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		s.acqErr(w, err)
	}
}

// PreviewRematch implements POST /library/{id}/rematch/preview.
func (s *Server) PreviewRematch(w http.ResponseWriter, r *http.Request, id int64) {
	var in apigen.RematchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req, add, err := s.rematchRequest(r, id, in)
	if err != nil {
		s.rematchErr(w, err)
		return
	}
	if add != nil {
		item, err := s.deps.Library.Hydrate(r.Context(), *add)
		if err != nil {
			s.libraryErr(w, err)
			return
		}
		item.Kind, item.RootFolderID = add.Kind, add.RootFolderID
		req.NewTarget = &item
	}
	preview, err := s.deps.Acquisition.PreviewRematch(r.Context(), req)
	if err != nil {
		s.rematchErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// StartRematch implements POST /library/{id}/rematch.
func (s *Server) StartRematch(w http.ResponseWriter, r *http.Request, id int64) {
	var in apigen.RematchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req, add, err := s.rematchRequest(r, id, in)
	if err != nil {
		s.rematchErr(w, err)
		return
	}
	if req.RequestID == "" || req.PlanFingerprint == "" {
		writeError(w, http.StatusBadRequest, "requestId and the preview's fingerprint are required")
		return
	}
	added := int64(0)
	if add != nil {
		// The title is added through the normal add path, then the
		// operation runs against it; the fingerprint check refuses the move
		// if what was added is not what was previewed.
		item, err := s.deps.Library.Add(r.Context(), *add)
		if err != nil {
			s.libraryErr(w, err)
			return
		}
		req.TargetItemID, req.TargetRevision, added = item.ID, 0, item.ID
	}
	op, err := s.deps.Acquisition.StartRematch(r.Context(), req)
	if err != nil {
		if added != 0 {
			// Refused (blocked, stale, no room): the title was added only for
			// this move, so it does not stay behind as an item nobody asked for.
			if derr := s.deps.Store.DeleteMediaItem(context.WithoutCancel(r.Context()), added); derr != nil {
				s.deps.Log.Warn("re-match: could not remove the title added for a refused move", "item", added, "err", derr)
			}
		}
		s.rematchErr(w, err)
		return
	}
	status, err := s.deps.Acquisition.GetRematch(r.Context(), op.ID)
	if err != nil {
		s.rematchErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

// GetLibraryRematch implements GET /library/{id}/rematch: the open
// operation moving files out of or into the item (204 when there is none),
// for the item page to follow after its dialog closed.
func (s *Server) GetLibraryRematch(w http.ResponseWriter, r *http.Request, id int64) {
	opID, err := s.deps.Store.OpenRematchForItem(r.Context(), id)
	if err != nil {
		s.rematchErr(w, err)
		return
	}
	if opID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.GetRematch(w, r, opID)
}

// GetRematch implements GET /rematch/{operationId}.
func (s *Server) GetRematch(w http.ResponseWriter, r *http.Request, operationID string) {
	status, err := s.deps.Acquisition.GetRematch(r.Context(), operationID)
	if err != nil {
		s.rematchErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// CancelRematch implements POST /rematch/{operationId}/cancel.
func (s *Server) CancelRematch(w http.ResponseWriter, r *http.Request, operationID string) {
	if err := s.deps.Acquisition.CancelRematch(r.Context(), operationID); err != nil {
		if errors.Is(err, acquisition.ErrInvalidRematch) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.rematchErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
