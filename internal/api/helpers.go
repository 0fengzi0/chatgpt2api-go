package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type openAIError struct {
	Status  int
	Type    string
	Param   string
	Code    string
	Message string
}

func (e openAIError) Error() string {
	return e.Message
}

func writeOpenAIError(w http.ResponseWriter, item openAIError) {
	if item.Status == 0 {
		item.Status = http.StatusBadRequest
	}
	if item.Type == "" {
		item.Type = "invalid_request_error"
	}
	body := map[string]any{"message": item.Message}
	if item.Param != "" {
		body["param"] = item.Param
	}
	if item.Code != "" {
		body["code"] = item.Code
	}
	body["type"] = item.Type
	writeJSON(w, item.Status, map[string]any{"error": body})
}

func writeManagementError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "management_error",
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 25<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

var unknownFieldPattern = regexp.MustCompile(`json: unknown field "([^"]+)"`)

func parameterError(err error) openAIError {
	if err == nil {
		return openAIError{Status: http.StatusBadRequest, Message: "invalid request"}
	}
	if matches := unknownFieldPattern.FindStringSubmatch(err.Error()); len(matches) == 2 {
		return openAIError{
			Status:  http.StatusBadRequest,
			Param:   matches[1],
			Code:    "unsupported_parameter",
			Message: "unsupported parameter: " + matches[1],
		}
	}
	if strings.Contains(err.Error(), "request body too large") {
		return openAIError{
			Status:  http.StatusRequestEntityTooLarge,
			Type:    "invalid_request_error",
			Code:    "request_too_large",
			Message: "request body is too large",
		}
	}
	return openAIError{
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Code:    "invalid_json",
		Message: "invalid JSON body: " + err.Error(),
	}
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		value := strings.TrimSpace(strings.Split(forwarded, ",")[0])
		if value == "http" || value == "https" {
			scheme = value
		}
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return scheme + "://" + host
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseIntField(value string, name string, defaultValue int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return number, nil
}
