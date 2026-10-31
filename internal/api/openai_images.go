package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/imageproc"
	"github.com/0fengzi0/chatgpt2api-go/internal/limiter"
	"github.com/0fengzi0/chatgpt2api-go/internal/provider"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

type imageGenerationRequest struct {
	Model             string `json:"model"`
	Prompt            string `json:"prompt"`
	N                 *int   `json:"n"`
	Size              string `json:"size"`
	Quality           string `json:"quality"`
	Background        string `json:"background"`
	Moderation        string `json:"moderation"`
	OutputFormat      string `json:"output_format"`
	OutputCompression *int   `json:"output_compression"`
	ResponseFormat    string `json:"response_format"`
	Stream            bool   `json:"stream"`
	PartialImages     *int   `json:"partial_images"`
	SafetyIdentifier  string `json:"safety_identifier"`
}

type imageOptions struct {
	Model             string
	Prompt            string
	N                 int
	Size              string
	Quality           string
	Background        string
	Moderation        string
	OutputFormat      string
	OutputCompression int
	CompressionSet    bool
	ResponseFormat    string
	Stream            bool
	PartialImages     int
	SafetyIdentifier  string
}

type imagePayload struct {
	Data          []byte
	MIME          string
	Width         int
	Height        int
	URL           string
	RevisedPrompt string
}

type imageExecution struct {
	Images     []imagePayload
	AccountIDs []string
	Succeeded  int
	QuotaUsed  int
	Usage      map[string]any
	CreatedAt  int64
}

type requestAudit struct {
	StartedAt       time.Time
	Mode            string
	Model           string
	Status          int
	Error           string
	RequestedImages int
	SucceededImages int
	QuotaUsed       int
	AccountIDs      []string
	SafetyID        string
}

func (s *Server) generateImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, openAIError{
			Status: http.StatusMethodNotAllowed, Type: "invalid_request_error",
			Code: "method_not_allowed", Message: "method not allowed",
		})
		return
	}
	audit := requestAudit{StartedAt: time.Now(), Mode: "generation"}
	defer s.recordRequestAudit(r, &audit)

	var body imageGenerationRequest
	if err := decodeJSON(r, &body); err != nil {
		s.finishImageError(w, r, &audit, parameterError(err))
		return
	}
	audit.Model = strings.TrimSpace(body.Model)
	audit.SafetyID = strings.TrimSpace(body.SafetyIdentifier)
	options, validationErr := s.normalizeImageOptions(imageOptionsInput{
		Model:             body.Model,
		Prompt:            body.Prompt,
		N:                 body.N,
		Size:              body.Size,
		Quality:           body.Quality,
		Background:        body.Background,
		Moderation:        body.Moderation,
		OutputFormat:      body.OutputFormat,
		OutputCompression: body.OutputCompression,
		ResponseFormat:    body.ResponseFormat,
		Stream:            body.Stream,
		PartialImages:     body.PartialImages,
		SafetyIdentifier:  body.SafetyIdentifier,
	})
	if validationErr != nil {
		s.finishImageError(w, r, &audit, *validationErr)
		return
	}
	audit.Model = options.Model
	audit.RequestedImages = options.N
	audit.SafetyID = options.SafetyIdentifier
	if err := s.checkPrompt(options); err != nil {
		s.finishImageError(w, r, &audit, *err)
		return
	}
	result, executionErr := s.executeImageRequest(r, options, nil)
	if executionErr != nil {
		audit.SucceededImages = result.Succeeded
		audit.QuotaUsed = result.QuotaUsed
		audit.AccountIDs = append(audit.AccountIDs, result.AccountIDs...)
		s.finishImageError(w, r, &audit, *executionErr)
		return
	}
	audit.SucceededImages = result.Succeeded
	audit.QuotaUsed = result.QuotaUsed
	audit.AccountIDs = append(audit.AccountIDs, result.AccountIDs...)
	if err := s.writeImageExecution(w, r, options, result); err != nil {
		audit.Status = err.Status
		audit.Error = err.Message
		return
	}
}

func (s *Server) editImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, openAIError{
			Status: http.StatusMethodNotAllowed, Type: "invalid_request_error",
			Code: "method_not_allowed", Message: "method not allowed",
		})
		return
	}
	audit := requestAudit{StartedAt: time.Now(), Mode: "edit"}
	defer s.recordRequestAudit(r, &audit)

	options, images, validationErr := s.parseEditRequest(r)
	if validationErr != nil {
		audit.Model = r.FormValue("model")
		s.finishImageError(w, r, &audit, *validationErr)
		return
	}
	audit.Model = options.Model
	audit.RequestedImages = options.N
	audit.SafetyID = options.SafetyIdentifier
	if err := s.checkPrompt(options); err != nil {
		s.finishImageError(w, r, &audit, *err)
		return
	}
	result, executionErr := s.executeImageRequest(r, options, images)
	if executionErr != nil {
		audit.SucceededImages = result.Succeeded
		audit.QuotaUsed = result.QuotaUsed
		audit.AccountIDs = append(audit.AccountIDs, result.AccountIDs...)
		s.finishImageError(w, r, &audit, *executionErr)
		return
	}
	audit.SucceededImages = result.Succeeded
	audit.QuotaUsed = result.QuotaUsed
	audit.AccountIDs = append(audit.AccountIDs, result.AccountIDs...)
	if err := s.writeImageExecution(w, r, options, result); err != nil {
		audit.Status = err.Status
		audit.Error = err.Message
		return
	}
}

type imageOptionsInput struct {
	Model             string
	Prompt            string
	N                 *int
	Size              string
	Quality           string
	Background        string
	Moderation        string
	OutputFormat      string
	OutputCompression *int
	ResponseFormat    string
	Stream            bool
	PartialImages     *int
	SafetyIdentifier  string
}

func (s *Server) normalizeImageOptions(input imageOptionsInput) (imageOptions, *openAIError) {
	cfg := s.cfg.Snapshot()
	modelName := strings.TrimSpace(input.Model)
	if modelName == "" {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "model", Code: "missing_required_parameter",
			Message: "model is required",
		}
	}
	model, ok := findImageModel(modelName)
	if !ok {
		return imageOptions{}, &openAIError{
			Status: http.StatusNotFound, Param: "model", Code: "model_not_found",
			Message: "model not found: " + modelName,
		}
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "prompt", Code: "missing_required_parameter",
			Message: "prompt is required",
		}
	}
	n := 1
	if input.N != nil {
		n = *input.N
	}
	if n < 1 || n > 4 {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "n", Code: "invalid_value",
			Message: "n must be between 1 and 4",
		}
	}
	size := firstNonEmpty(input.Size, "auto")
	if size != "auto" {
		parts := strings.Split(strings.ToLower(size), "x")
		if len(parts) != 2 {
			return imageOptions{}, &openAIError{
				Status: http.StatusBadRequest, Param: "size", Code: "invalid_value",
				Message: "size must be auto or WIDTHxHEIGHT",
			}
		}
		width, errWidth := strconv.Atoi(parts[0])
		height, errHeight := strconv.Atoi(parts[1])
		if errWidth != nil || errHeight != nil || width < 1 || height < 1 ||
			width > cfg.Image.MaxDimension || height > cfg.Image.MaxDimension ||
			(cfg.Image.MaxImagePixels > 0 && int64(width)*int64(height) > cfg.Image.MaxImagePixels) {
			return imageOptions{}, &openAIError{
				Status: http.StatusBadRequest, Param: "size", Code: "invalid_value",
				Message: fmt.Sprintf(
					"size %s exceeds configured dimension limits (max-edge=%d, max-pixels=%d)",
					size, cfg.Image.MaxDimension, cfg.Image.MaxImagePixels,
				),
			}
		}
	}
	quality := firstNonEmpty(input.Quality, "auto")
	if !contains(model.Quality, quality) {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "quality", Code: "invalid_value",
			Message: "unsupported quality for " + modelName,
		}
	}
	background := firstNonEmpty(input.Background, "auto")
	if !contains([]string{"auto", "opaque", "transparent"}, background) {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "background", Code: "invalid_value",
			Message: "background must be auto, opaque, or transparent",
		}
	}
	moderation := firstNonEmpty(input.Moderation, "auto")
	if moderation != "auto" && moderation != "low" {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "moderation", Code: "invalid_value",
			Message: "moderation must be auto or low",
		}
	}
	outputFormat := strings.ToLower(firstNonEmpty(input.OutputFormat, "png"))
	if !contains([]string{"png", "jpeg", "webp"}, outputFormat) {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "output_format", Code: "invalid_value",
			Message: "output_format must be png, jpeg, or webp",
		}
	}
	compression := -1
	if input.OutputCompression != nil {
		if outputFormat == "png" {
			return imageOptions{}, &openAIError{
				Status: http.StatusBadRequest, Param: "output_compression", Code: "unsupported_parameter",
				Message: "output_compression is only supported for jpeg or webp",
			}
		}
		if *input.OutputCompression < 0 || *input.OutputCompression > 100 {
			return imageOptions{}, &openAIError{
				Status: http.StatusBadRequest, Param: "output_compression", Code: "invalid_value",
				Message: "output_compression must be between 0 and 100",
			}
		}
		compression = *input.OutputCompression
	}
	if background == "transparent" && outputFormat == "jpeg" {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "background", Code: "invalid_value",
			Message: "transparent background requires png or webp output",
		}
	}
	responseFormat := firstNonEmpty(input.ResponseFormat, "b64_json")
	if responseFormat != "b64_json" && responseFormat != "url" {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "response_format", Code: "invalid_value",
			Message: "response_format must be b64_json or url",
		}
	}
	partialImages := 0
	if input.PartialImages != nil {
		partialImages = *input.PartialImages
	}
	if partialImages < 0 || partialImages > 3 {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "partial_images", Code: "invalid_value",
			Message: "partial_images must be between 0 and 3",
		}
	}
	if partialImages > 0 && !input.Stream {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "partial_images", Code: "invalid_value",
			Message: "partial_images requires stream=true",
		}
	}
	safetyID := strings.TrimSpace(input.SafetyIdentifier)
	if len(safetyID) > 255 {
		return imageOptions{}, &openAIError{
			Status: http.StatusBadRequest, Param: "safety_identifier", Code: "invalid_value",
			Message: "safety_identifier must be 255 characters or fewer",
		}
	}
	return imageOptions{
		Model: modelName, Prompt: prompt, N: n, Size: size, Quality: quality,
		Background: background, Moderation: moderation, OutputFormat: outputFormat,
		OutputCompression: compression, CompressionSet: input.OutputCompression != nil,
		ResponseFormat: responseFormat, Stream: input.Stream, PartialImages: partialImages,
		SafetyIdentifier: safetyID,
	}, nil
}

func (s *Server) parseEditRequest(r *http.Request) (imageOptions, [][]byte, *openAIError) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return imageOptions{}, nil, &openAIError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_multipart", Message: "invalid multipart form",
		}
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	allowedValues := map[string]bool{
		"model": true, "prompt": true, "n": true, "size": true, "quality": true,
		"background": true, "moderation": true, "output_format": true,
		"output_compression": true, "response_format": true, "stream": true,
		"partial_images": true, "safety_identifier": true,
	}
	for key := range r.MultipartForm.Value {
		if !allowedValues[key] {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: key, Code: "unsupported_parameter",
				Message: "unsupported parameter: " + key,
			}
		}
	}
	for key := range r.MultipartForm.File {
		if key != "image" && key != "image[]" && key != "mask" {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: key, Code: "unsupported_parameter",
				Message: "unsupported file parameter: " + key,
			}
		}
	}
	nValue := r.FormValue("n")
	nPointer := (*int)(nil)
	if strings.TrimSpace(nValue) != "" {
		parsed, err := strconv.Atoi(nValue)
		if err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "n", Code: "invalid_value",
				Message: "n must be an integer",
			}
		}
		nPointer = &parsed
	}
	compressionPointer := (*int)(nil)
	if value := strings.TrimSpace(r.FormValue("output_compression")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "output_compression", Code: "invalid_value",
				Message: "output_compression must be an integer",
			}
		}
		compressionPointer = &parsed
	}
	partialPointer := (*int)(nil)
	if value := strings.TrimSpace(r.FormValue("partial_images")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "partial_images", Code: "invalid_value",
				Message: "partial_images must be an integer",
			}
		}
		partialPointer = &parsed
	}
	stream := false
	if value := strings.TrimSpace(r.FormValue("stream")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "stream", Code: "invalid_value",
				Message: "stream must be a boolean",
			}
		}
		stream = parsed
	}
	options, validationErr := s.normalizeImageOptions(imageOptionsInput{
		Model: r.FormValue("model"), Prompt: r.FormValue("prompt"), N: nPointer,
		Size: r.FormValue("size"), Quality: r.FormValue("quality"),
		Background: r.FormValue("background"), Moderation: r.FormValue("moderation"),
		OutputFormat: r.FormValue("output_format"), OutputCompression: compressionPointer,
		ResponseFormat: r.FormValue("response_format"), Stream: stream,
		PartialImages: partialPointer, SafetyIdentifier: r.FormValue("safety_identifier"),
	})
	if validationErr != nil {
		return imageOptions{}, nil, validationErr
	}
	files := append([]*multipart.FileHeader{}, r.MultipartForm.File["image[]"]...)
	files = append(files, r.MultipartForm.File["image"]...)
	if len(files) == 0 {
		return imageOptions{}, nil, &openAIError{
			Status: http.StatusBadRequest, Param: "image", Code: "missing_required_parameter",
			Message: "image is required",
		}
	}
	if len(files) > 4 {
		return imageOptions{}, nil, &openAIError{
			Status: http.StatusBadRequest, Param: "image", Code: "invalid_value",
			Message: "image supports up to 4 files",
		}
	}
	cfg := s.cfg.Snapshot()
	images := make([][]byte, 0, len(files))
	for _, header := range files {
		data, err := readUploadedFile(header, cfg.Image.MaxUploadBytes)
		if err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "image", Code: "invalid_image",
				Message: err.Error(),
			}
		}
		if _, err := imageproc.ValidateUpload(data, cfg.Image.MaxUploadBytes, cfg.Image.MaxImagePixels); err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "image", Code: "invalid_image",
				Message: err.Error(),
			}
		}
		images = append(images, data)
	}
	if masks := r.MultipartForm.File["mask"]; len(masks) > 0 {
		if len(masks) > 1 {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "mask", Code: "invalid_value",
				Message: "mask supports one file",
			}
		}
		mask, err := readUploadedFile(masks[0], cfg.Image.MaxUploadBytes)
		if err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "mask", Code: "invalid_image",
				Message: err.Error(),
			}
		}
		if _, err := imageproc.ValidateUpload(mask, cfg.Image.MaxUploadBytes, cfg.Image.MaxImagePixels); err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "mask", Code: "invalid_image",
				Message: err.Error(),
			}
		}
		merged, err := imageproc.CompositeMask(images[0], mask, cfg.Image.MaxImagePixels)
		if err != nil {
			return imageOptions{}, nil, &openAIError{
				Status: http.StatusBadRequest, Param: "mask", Code: "invalid_image",
				Message: err.Error(),
			}
		}
		images[0] = merged
	}
	return options, images, nil
}

func readUploadedFile(header *multipart.FileHeader, maxBytes int64) ([]byte, error) {
	if maxBytes > 0 && header.Size > maxBytes {
		return nil, fmt.Errorf("image file exceeds %d bytes", maxBytes)
	}
	file, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := io.Reader(file)
	if maxBytes > 0 {
		reader = io.LimitReader(file, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("image file exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func (s *Server) checkPrompt(options imageOptions) *openAIError {
	if options.Moderation == "low" {
		return nil
	}
	lowerPrompt := strings.ToLower(options.Prompt)
	for _, word := range s.cfg.Snapshot().SensitiveWords {
		if strings.Contains(lowerPrompt, strings.ToLower(word)) {
			return &openAIError{
				Status: http.StatusBadRequest, Param: "prompt", Code: "moderation_blocked",
				Message: "prompt was blocked by local moderation",
			}
		}
	}
	return nil
}

func (s *Server) executeImageRequest(r *http.Request, options imageOptions, images [][]byte) (imageExecution, *openAIError) {
	cfg := s.cfg.Snapshot()
	timeout := time.Duration(cfg.Image.RequestTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	type indexedResult struct {
		index  int
		result provider.Result
		err    error
	}
	results := make([]provider.Result, options.N)
	channel := make(chan indexedResult, options.N)
	var wait sync.WaitGroup
	for index := 0; index < options.N; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := s.generateOneImage(ctx, options, images)
			channel <- indexedResult{index: index, result: result, err: err}
		}(index)
	}
	go func() {
		wait.Wait()
		close(channel)
	}()

	var firstErr error
	for item := range channel {
		if item.err != nil {
			if firstErr == nil {
				firstErr = item.err
				cancel()
			}
			continue
		}
		results[item.index] = item.result
	}
	execution := imageExecution{CreatedAt: time.Now().Unix()}
	for _, result := range results {
		if len(result.Images) == 0 {
			continue
		}
		data, mime, width, height, err := imageproc.Convert(
			result.Images[0], options.OutputFormat, options.OutputCompression, cfg.Image.MaxImagePixels, options.Background,
		)
		if err != nil {
			return execution, &openAIError{
				Status: http.StatusBadGateway, Type: "server_error", Code: "image_processing_failed",
				Message: "failed to process generated image",
			}
		}
		execution.Images = append(execution.Images, imagePayload{
			Data: data, MIME: mime, Width: width, Height: height,
			RevisedPrompt: firstNonEmpty(result.RevisedPrompt, options.Prompt),
		})
		execution.AccountIDs = appendUnique(execution.AccountIDs, result.AccountID)
		execution.Succeeded++
		execution.QuotaUsed++
	}
	if firstErr != nil {
		return execution, classifyImageError(firstErr)
	}
	if ctx.Err() != nil {
		return execution, classifyImageError(ctx.Err())
	}
	if execution.Succeeded != options.N {
		return execution, classifyImageError(errors.New("provider returned no images"))
	}
	width, height := 0, 0
	if len(execution.Images) > 0 {
		width = execution.Images[0].Width
		height = execution.Images[0].Height
	}
	execution.Usage = imageproc.EstimateUsage(options.Prompt, execution.Succeeded, width, height)
	return execution, nil
}

func (s *Server) generateOneImage(ctx context.Context, options imageOptions, images [][]byte) (provider.Result, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		release, err := s.limiter.Acquire(ctx)
		if err != nil {
			return provider.Result{}, err
		}
		result, err := s.provider.Generate(ctx, provider.Request{
			Prompt: options.Prompt, Model: options.Model, Size: options.Size, Quality: options.Quality,
			Background: options.Background, Moderation: options.Moderation,
			SafetyIdentifier: options.SafetyIdentifier, Images: images,
		})
		release()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt == 1 || !isTransientImageError(err) || ctx.Err() != nil {
			return provider.Result{}, err
		}
		select {
		case <-ctx.Done():
			return provider.Result{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return provider.Result{}, lastErr
}

func (s *Server) finishImageError(w http.ResponseWriter, r *http.Request, audit *requestAudit, item openAIError) {
	s.errors.Add(1)
	if item.Message == "" {
		item.Message = "image request failed"
	}
	audit.Status = item.Status
	audit.Error = item.Message
	writeOpenAIError(w, item)
	_ = r
	_ = audit
}

func (s *Server) writeImageExecution(w http.ResponseWriter, r *http.Request, options imageOptions, result imageExecution) *openAIError {
	data := make([]map[string]any, 0, len(result.Images))
	for index := range result.Images {
		item := &result.Images[index]
		outputItem := map[string]any{"revised_prompt": item.RevisedPrompt}
		if options.ResponseFormat == "url" {
			stored, err := s.outputs.Save(r.Context(), item.Data, item.MIME, item.Width, item.Height)
			if err != nil {
				s.errors.Add(1)
				item := openAIError{
					Status: http.StatusInternalServerError, Type: "server_error",
					Code: "temporary_storage_failed", Message: "failed to store temporary image",
				}
				writeOpenAIError(w, item)
				return &item
			}
			item.URL = s.outputs.SignedURL(requestOrigin(r), stored)
			outputItem["url"] = item.URL
		} else {
			outputItem["b64_json"] = base64.StdEncoding.EncodeToString(item.Data)
		}
		data = append(data, outputItem)
	}
	response := map[string]any{
		"created":       result.CreatedAt,
		"background":    options.Background,
		"data":          data,
		"output_format": options.OutputFormat,
		"quality":       options.Quality,
		"usage":         result.Usage,
	}
	if len(result.Images) > 0 {
		response["size"] = fmt.Sprintf("%dx%d", result.Images[0].Width, result.Images[0].Height)
	} else {
		response["size"] = options.Size
	}
	if options.Stream {
		s.writeImageStream(w, options, result, response)
		return nil
	}
	writeJSON(w, http.StatusOK, response)
	return nil
}

func (s *Server) writeImageStream(w http.ResponseWriter, options imageOptions, result imageExecution, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	data, _ := response["data"].([]map[string]any)
	for _, item := range data {
		event := map[string]any{
			"type":          "image_generation.completed",
			"created_at":    result.CreatedAt,
			"size":          response["size"],
			"quality":       options.Quality,
			"background":    options.Background,
			"output_format": options.OutputFormat,
			"usage":         result.Usage,
		}
		for key, value := range item {
			event[key] = value
		}
		payload, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: image_generation.completed\ndata: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (s *Server) recordRequestAudit(r *http.Request, audit *requestAudit) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	status := http.StatusOK
	if audit.Status >= 400 {
		status = audit.Status
	}
	errorText := ""
	if strings.TrimSpace(audit.Error) != "" {
		errorText = sanitizeUpstreamError(audit.Error)
	}
	item := store.RequestLog{
		RequestID: requestID(ctx), Owner: owner(ctx), Method: r.Method, Path: r.URL.Path,
		Mode: audit.Mode, Model: audit.Model, Status: status, Error: errorText,
		DurationMS:      time.Since(audit.StartedAt).Milliseconds(),
		RequestedImages: audit.RequestedImages, SucceededImages: audit.SucceededImages,
		QuotaUsed: audit.QuotaUsed, AccountIDs: strings.Join(audit.AccountIDs, ","),
		SafetyIdentifier: audit.SafetyID,
	}
	_, _ = s.db.AddRequestLog(ctx, item)
	failed := status >= 400
	if item.Owner != "" {
		s.db.RecordAPIKeyUse(ctx, item.Owner, failed)
	}
}

func classifyImageError(err error) *openAIError {
	if errors.Is(err, limiter.ErrQueueFull) {
		return &openAIError{
			Status: http.StatusTooManyRequests, Type: "requests",
			Code: "concurrency_limit_exceeded", Message: "image request queue is full",
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &openAIError{
			Status: http.StatusGatewayTimeout, Type: "server_error",
			Code: "upstream_timeout", Message: "image generation timed out",
		}
	}
	if errors.Is(err, context.Canceled) {
		return &openAIError{
			Status: http.StatusRequestTimeout, Type: "server_error", Code: "request_canceled",
			Message: "request was canceled",
		}
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "no available image quota") ||
		strings.Contains(lower, "no available account") {
		return &openAIError{
			Status: http.StatusTooManyRequests, Type: "insufficient_quota",
			Code: "insufficient_quota", Message: "no available image quota",
		}
	}
	if strings.Contains(lower, "http 429") || strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit") {
		return &openAIError{
			Status: http.StatusTooManyRequests, Type: "requests",
			Code: "rate_limit_exceeded", Message: "image generation rate limit exceeded",
		}
	}
	if strings.Contains(lower, "moderation") || strings.Contains(lower, "content policy") ||
		strings.Contains(lower, "rejected by upstream policy") {
		return &openAIError{
			Status: http.StatusBadRequest, Param: "prompt", Code: "moderation_blocked",
			Message: "image request was blocked by moderation",
		}
	}
	if strings.Contains(lower, "sentinel solver") || strings.Contains(lower, "chromium-family browser not found") ||
		strings.Contains(lower, "browser not found") {
		return &openAIError{
			Status: http.StatusServiceUnavailable, Type: "server_error",
			Code: "solver_unavailable", Message: "image solver is not ready",
		}
	}
	return &openAIError{
		Status: http.StatusBadGateway, Type: "server_error",
		Code: "upstream_error", Message: "The image generation request failed. Please try again later.",
	}
}

func sanitizeUpstreamError(message string) string {
	message = strings.TrimSpace(message)
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"authorization:", "bearer ", "cookie:", "token=", "backend-api/", "chatgpt.com",
		"http ", "http=", "status=", "body=", "upstream", "sse", "stream",
	} {
		if strings.Contains(lower, marker) {
			return "The image generation request failed. Please try again later."
		}
	}
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		return "The image generation request failed. Please try again later."
	}
	return message
}

func isTransientImageError(err error) bool {
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{
		"timeout", "timed out", "connection reset", "connection aborted", "tls",
		"temporary failure", "unexpected eof", "http 429", "http 500", "http 502",
		"http 503", "http 504",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func readAllLimited(reader io.Reader, limit int64) ([]byte, error) {
	var buffer bytes.Buffer
	_, err := io.CopyN(&buffer, reader, limit+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(buffer.Len()) > limit {
		return nil, errors.New("body is too large")
	}
	return buffer.Bytes(), nil
}
