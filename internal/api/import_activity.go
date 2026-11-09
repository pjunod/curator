package api

import (
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func (s *Server) ListImportActivity(w http.ResponseWriter, r *http.Request, p apigen.ListImportActivityParams) {
	filter, search, limit, offset := "all", "", 50, 0
	if p.Filter != nil {
		filter = string(*p.Filter)
	}
	if p.Q != nil {
		search = *p.Q
	}
	if p.Limit != nil {
		limit = *p.Limit
	}
	if p.Offset != nil {
		offset = *p.Offset
	}
	if (filter != "all" && filter != "active" && filter != "queued" && filter != "attention" && filter != "imported") || limit < 1 || limit > 200 || offset < 0 {
		writeError(w, 400, "invalid import filter or pagination")
		return
	}
	page, err := s.deps.Acquisition.ImportActivity(r.Context(), filter, search, limit, offset)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	rows := make([]sqlite.Download, 0, len(page.Rows))
	for _, entry := range page.Rows {
		if entry.ID < 0 {
			continue
		}
		d, e := s.deps.Store.GetDownload(r.Context(), entry.ID)
		if e != nil {
			s.acqErr(w, e)
			return
		}
		rows = append(rows, d)
	}
	items, err := s.queueItems(r.Context(), rows)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := apigen.ImportActivityPage{Items: []apigen.ImportActivityItem{}, Counts: page.Counts, Total: page.Total, HasMore: page.HasMore, RetentionDays: s.deps.Acquisition.RetentionDays(r.Context())}
	byID := make(map[int64]apigen.QueueItem, len(items))
	for _, item := range items {
		byID[item.Id] = item
	}
	for _, entry := range page.Rows {
		item := byID[entry.ID]
		if entry.ID < 0 {
			pub, e := s.deps.Store.GetPublication(r.Context(), -entry.ID)
			if e != nil {
				s.acqErr(w, e)
				return
			}
			files, e := s.deps.Store.ListPublicationFiles(r.Context(), pub.ID)
			if e != nil {
				s.acqErr(w, e)
				return
			}
			item = apigen.QueueItem{Id: entry.ID, Title: entry.Title, State: pub.State, Protocol: "historical", AddedAt: pub.DiscoveredAt, ImportPath: &pub.Path}
			s.fillPipelineV2(&item, sqlite.Download{}, []sqlite.Publication{pub}, map[int64][]sqlite.PublicationFile{pub.ID: files})
		}
		out.Items = append(out.Items, apigen.ImportActivityItem{Download: item, Category: apigen.ImportActivityItemCategory(entry.Category), Title: entry.Title, Kind: entry.Kind, Destination: entry.Destination, UpdatedAt: entry.UpdatedAt})
	}
	writeJSON(w, 200, out)
}
