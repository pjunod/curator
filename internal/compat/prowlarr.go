package compat

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/monarr-media/monarr/internal/ports"
)

// mountIndexers adds the indexer-management surface Prowlarr drives when it
// syncs "apps": schema discovery, then POST/PUT/DELETE of Torznab configs.
func (p *Personality) mountIndexers(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/indexer", p.listIndexersV3)
	mux.HandleFunc("GET /api/v3/indexer/schema", p.indexerSchema)
	mux.HandleFunc("POST /api/v3/indexer", p.addIndexerV3)
	mux.HandleFunc("PUT /api/v3/indexer/{id}", p.updateIndexerV3)
	mux.HandleFunc("DELETE /api/v3/indexer/{id}", p.deleteIndexerV3)
}

// v3Indexer renders our config in the Sonarr/Radarr indexer shape.
func v3Indexer(cfg ports.IndexerConfig) map[string]any {
	cats := make([]int, len(cfg.Categories))
	copy(cats, cfg.Categories)
	return map[string]any{
		"id": cfg.ID, "name": cfg.Name, "implementation": "Torznab",
		"implementationName": "Torznab", "configContract": "TorznabSettings",
		"protocol": cfg.Protocol, "enableRss": cfg.Enabled,
		"enableAutomaticSearch": cfg.Enabled, "enableInteractiveSearch": cfg.Enabled,
		"priority": 25, "tags": []any{},
		"fields": []map[string]any{
			{"name": "baseUrl", "value": cfg.URL},
			{"name": "apiPath", "value": "/api"},
			{"name": "apiKey", "value": cfg.APIKey},
			{"name": "categories", "value": cats},
		},
	}
}

func (p *Personality) listIndexersV3(w http.ResponseWriter, r *http.Request) {
	configs, err := p.deps.Store.ListIndexers(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, v3Indexer(cfg))
	}
	writeJSON(w, http.StatusOK, out)
}

// indexerSchema advertises the Torznab implementation Prowlarr looks for.
func (p *Personality) indexerSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]any{
		{
			"name": "", "implementation": "Torznab", "implementationName": "Torznab",
			"configContract": "TorznabSettings", "protocol": "torrent",
			"infoLink": "https://github.com/monarr-media/monarr",
			"fields": []map[string]any{
				{"name": "baseUrl", "value": ""},
				{"name": "apiPath", "value": "/api"},
				{"name": "apiKey", "value": ""},
				{"name": "categories", "value": []int{}},
			},
		},
		{
			"name": "", "implementation": "Newznab", "implementationName": "Newznab",
			"configContract": "NewznabSettings", "protocol": "usenet",
			"infoLink": "https://github.com/monarr-media/monarr",
			"fields": []map[string]any{
				{"name": "baseUrl", "value": ""},
				{"name": "apiPath", "value": "/api"},
				{"name": "apiKey", "value": ""},
				{"name": "categories", "value": []int{}},
			},
		},
	})
}

// v3IndexerPayload is the add/update body Prowlarr sends.
type v3IndexerPayload struct {
	Name           string `json:"name"`
	Implementation string `json:"implementation"`
	Protocol       string `json:"protocol"`
	EnableRss      *bool  `json:"enableRss"`
	Fields         []struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
	} `json:"fields"`
}

func (b v3IndexerPayload) toConfig() ports.IndexerConfig {
	cfg := ports.IndexerConfig{Name: b.Name, Protocol: b.Protocol, Enabled: true}
	if strings.EqualFold(b.Implementation, "newznab") {
		cfg.Protocol = "usenet"
	}
	// Clamp to the schema's vocabulary: anything that isn't usenet is a
	// torrent feed as far as grab routing goes.
	if cfg.Protocol != "usenet" {
		cfg.Protocol = "torrent"
	}
	if b.EnableRss != nil {
		cfg.Enabled = *b.EnableRss
	}
	base, apiPath := "", ""
	for _, f := range b.Fields {
		switch f.Name {
		case "baseUrl":
			_ = json.Unmarshal(f.Value, &base)
		case "apiPath":
			_ = json.Unmarshal(f.Value, &apiPath)
		case "apiKey":
			_ = json.Unmarshal(f.Value, &cfg.APIKey)
		case "categories":
			_ = json.Unmarshal(f.Value, &cfg.Categories)
		}
	}
	cfg.URL = strings.TrimRight(base, "/")
	// Our torznab adapter appends /api itself; a non-default apiPath is
	// preserved by folding it into the URL.
	if apiPath != "" && apiPath != "/api" {
		cfg.URL += strings.TrimSuffix(apiPath, "/api")
	}
	return cfg
}

func (p *Personality) addIndexerV3(w http.ResponseWriter, r *http.Request) {
	var body v3IndexerPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid body"})
		return
	}
	cfg := body.toConfig()
	// Idempotent by URL: both personalities point Prowlarr at the same
	// store, so a second sync must not duplicate.
	existing, _ := p.deps.Store.ListIndexers(r.Context())
	for _, e := range existing {
		if e.URL == cfg.URL && e.Name == cfg.Name {
			writeJSON(w, http.StatusCreated, v3Indexer(e))
			return
		}
	}
	id, err := p.deps.Store.AddIndexer(r.Context(), cfg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	cfg.ID = id
	writeJSON(w, http.StatusCreated, v3Indexer(cfg))
}

// updateIndexerV3 is delete-and-recreate (translation-only; no in-place
// update in the native store yet).
func (p *Personality) updateIndexerV3(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := p.deps.Store.GetIndexer(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "indexer not found"})
		return
	}
	var body v3IndexerPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid body"})
		return
	}
	if err := p.deps.Store.DeleteIndexer(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cfg := body.toConfig()
	newID, err := p.deps.Store.AddIndexer(r.Context(), cfg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	cfg.ID = newID
	writeJSON(w, http.StatusAccepted, v3Indexer(cfg))
}

func (p *Personality) deleteIndexerV3(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := p.deps.Store.DeleteIndexer(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "indexer not found"})
		return
	}
	w.WriteHeader(http.StatusOK)
}
