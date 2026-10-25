package api

import (
	"net/http"
	"strings"
)

type imageModel struct {
	ID      string
	Title   string
	Quality []string
}

var imageModels = []imageModel{
	{ID: "gpt-image-2", Title: "GPT Image 2", Quality: []string{"auto", "low", "medium", "high"}},
	{ID: "gpt-image-2.5", Title: "GPT Image 2.5", Quality: []string{"auto", "low", "medium", "high", "xhigh", "max"}},
	{ID: "gpt-image-2.5-sunburst", Title: "GPT Image 2.5 Sunburst", Quality: []string{"auto", "low", "medium", "high", "xhigh", "max"}},
	{ID: "gpt-image-2.5-flare", Title: "GPT Image 2.5 Flare", Quality: []string{"auto", "low", "medium", "high", "xhigh", "max"}},
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, openAIError{
			Status: http.StatusMethodNotAllowed, Type: "invalid_request_error",
			Code: "method_not_allowed", Message: "method not allowed",
		})
		return
	}
	items := make([]map[string]any, 0, len(imageModels))
	for _, item := range imageModels {
		items = append(items, modelObject(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}

func (s *Server) modelByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, openAIError{
			Status: http.StatusMethodNotAllowed, Type: "invalid_request_error",
			Code: "method_not_allowed", Message: "method not allowed",
		})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	for _, item := range imageModels {
		if item.ID == id {
			writeJSON(w, http.StatusOK, modelObject(item))
			return
		}
	}
	writeOpenAIError(w, openAIError{
		Status: http.StatusNotFound, Type: "invalid_request_error",
		Param: "model", Code: "model_not_found", Message: "model not found: " + id,
	})
}

func modelObject(item imageModel) map[string]any {
	return map[string]any{
		"id":         item.ID,
		"object":     "model",
		"created":    1780000000,
		"owned_by":   "chatgpt2api",
		"root":       item.ID,
		"parent":     nil,
		"permission": []any{},
		"metadata": map[string]any{
			"title":           item.Title,
			"provider":        "chatgpt-web",
			"upstream_route":  "picture_v2",
			"quality_options": item.Quality,
		},
	}
}

func findImageModel(id string) (imageModel, bool) {
	id = strings.TrimSpace(id)
	for _, item := range imageModels {
		if item.ID == id {
			return item, true
		}
	}
	return imageModel{}, false
}
