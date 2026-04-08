package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	apiKey           string
	logFile          string
	port             string
	logger           *log.Logger
	queryLogger      *log.Logger // Separate logger for prompts and outputs
	metrics          *MetricsCollector
	openRouterClient *http.Client
	timeout          time.Duration
	logQueries       bool
	models           *ModelResponse
}

type MetricsCollector struct {
	mu     sync.RWMutex
	timers map[string][]float64
}

type OpenRouterModelResponse struct {
	Data []struct {
		Id   string `json:"id"`
		Name string `json:"name"`
	}
}

type ModelResponse struct {
	mu                             sync.RWMutex
	IdNameMap                      map[string]string
	NameIdMap                      map[string]string
	LastUpdated                    time.Time
	ModelNameToFreeModelIdCacheMap map[string]string
}

func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		timers: make(map[string][]float64),
	}
}

func (m *MetricsCollector) Record(key string, duration float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timers[key] = append(m.timers[key], duration)
}

func (m *MetricsCollector) Snapshot() map[string]float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := make(map[string]float64)
	for key, times := range m.timers {
		if len(times) > 0 {
			var total float64
			for _, t := range times {
				total += t
			}
			snapshot[key] = total / float64(len(times))
		}
	}
	return snapshot
}

func (m *MetricsCollector) Reset(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timers[key] = m.timers[key][:0]
}

func LoadConfig() (*Server, error) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY environment variable is not set")
	}

	logFile := os.Getenv("LOG_FILE")
	if logFile == "" {
		exePath, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("failed to get executable path: %w", err)
		}
		baseDir := filepath.Dir(exePath)
		logDir := filepath.Join(baseDir, "logs")
		logFile = filepath.Join(logDir, "claude-proxy.log")
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create log directory: %w", err)
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Configure timeout (default: 30 minutes for AI inference)
	timeout := 30 * time.Minute
	if timeoutStr := os.Getenv("OPENROUTER_TIMEOUT"); timeoutStr != "" {
		if parsed, err := time.ParseDuration(timeoutStr); err != nil {
			return nil, fmt.Errorf("invalid OPENROUTER_TIMEOUT: %w", err)
		} else {
			timeout = parsed
		}
	}

	// Configure query logging (default: enabled)
	logQueries := true
	if logQueriesStr := os.Getenv("LOG_QUERIES"); logQueriesStr != "" {
		if parsed, err := strconv.ParseBool(logQueriesStr); err != nil {
			return nil, fmt.Errorf("invalid LOG_QUERIES: %w", err)
		} else {
			logQueries = parsed
		}
	}

	// Configure transport with optimized connection pool limits
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        200,               // Increased from 100
		MaxIdleConnsPerHost: 50,                // Increased from 10
		MaxConnsPerHost:     200,               // Increased from 100
		IdleConnTimeout:     120 * time.Second, // Increased from 90s
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
		// Additional optimizations
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Server{
		apiKey:           apiKey,
		logFile:          logFile,
		port:             port,
		timeout:          timeout,
		logQueries:       logQueries,
		metrics:          NewMetricsCollector(),
		openRouterClient: &http.Client{Transport: transport, Timeout: timeout},
	}, nil
}

func (s *Server) Init() error {
	f, err := os.OpenFile(s.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	log.SetOutput(f)
	log.Println("Logging started!")

	s.logger = log.New(io.MultiWriter(os.Stdout, f), "claude-proxy: ", log.LstdFlags|log.Lshortfile)
	s.logger.Println("Initialized successfully")

	// Initialize query logger (separate file for prompts and outputs)
	if s.logQueries {
		exePath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to get executable path: %w", err)
		}
		baseDir := filepath.Dir(exePath)
		logDir := filepath.Join(baseDir, "logs")
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return fmt.Errorf("failed to create log directory: %w", err)
		}
		queryLogFile := filepath.Join(logDir, "queries.log")
		queryFile, err := os.OpenFile(queryLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("failed to open query log file: %w", err)
		}
		s.queryLogger = log.New(queryFile, "", 0) // No prefix, no timestamps — clean JSON only
	}

	return nil
}

func (s *Server) Start() error {
	http.HandleFunc("/v1/messages", s.messageHandler)
	http.HandleFunc("/v1/chat/completions", s.messageHandler)
	http.HandleFunc("/health", s.healthCheck)

	s.logger.Printf("Server starting on http://localhost:%s", s.port)

	server := &http.Server{
		Addr: ":" + s.port,
	}

	// Graceful shutdown handling
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop

		s.logger.Println("Shutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			s.logger.Fatalf("Server shutdown error: %v", err)
		}
		s.logger.Println("Server stopped")
	}()

	return server.ListenAndServe()
}

func (s *Server) healthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}

func (s *Server) logUserQuery(claudeReq map[string]interface{}) {
	if s.queryLogger == nil {
		return
	}
	if messages, ok := claudeReq["messages"].([]interface{}); ok {
		var userQueries []string
		for _, msg := range messages {
			if m, ok := msg.(map[string]interface{}); ok {
				if role, ok := m["role"].(string); ok && role == "user" {
					if content, ok := m["content"].(string); ok {
						userQueries = append(userQueries, content)
					} else if contentArr, ok := m["content"].([]interface{}); ok {
						for _, block := range contentArr {
							if b, ok := block.(map[string]interface{}); ok {
								if bType, ok := b["type"].(string); ok && bType == "text" {
									if text, ok := b["text"].(string); ok {
										userQueries = append(userQueries, text)
									}
								}
							}
						}
					}
				}
			}
		}
		if len(userQueries) > 0 {
			s.logger.Printf("User query logged") // operational log
			s.queryLogger.Printf("=== REQUEST ===\n%s", strings.Join(userQueries, " | "))
		}
	}
}

func (s *Server) messageHandler(w http.ResponseWriter, req *http.Request) {
	startT := time.Now()
	ctx := req.Context()

	s.logger.Printf("Received %s request for %s", req.Method, req.URL.Path)

	// Enforce request size limit (default: 10MB)
	const maxRequestSize = 10 << 20 // 10MB
	req.Body = http.MaxBytesReader(w, req.Body, maxRequestSize)

	body, err := io.ReadAll(req.Body)
	if err != nil {
		s.logError("Error while reading body", err)
		s.writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	var claudeReq map[string]interface{}
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		s.logError("Error while unmarshaling request body", err)
		s.writeError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	// Log the user query (extracted from user messages) if enabled
	if s.logQueries {
		s.logUserQuery(claudeReq)
	}

	messages, ok := claudeReq["messages"].([]interface{})
	if !ok {
		s.logError("Missing or invalid 'messages' field", fmt.Errorf("messages is not an array"))
		s.writeError(w, http.StatusBadRequest, "Missing 'messages' field")
		return
	}

	newMessages := []interface{}{}
	for i, msg := range messages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			s.logger.Printf("Skipping invalid message at index %d: %+v", i, msg)
			continue
		}

		// Check if the content is a string containing the Suggestion Mode trigger
		contentStr, isString := m["content"].(string)
		if isString && strings.Contains(contentStr, "[SUGGESTION MODE:") {
			continue // Skip this message entirely
		}

		// Check if content is an array (Claude sometimes sends mixed types)
		if contentArr, isArr := m["content"].([]interface{}); isArr {
			skipMessage := false
			for j, block := range contentArr {
				b, ok := block.(map[string]interface{})
				if !ok {
					s.logger.Printf("Skipping invalid content block at message %d, block %d: %+v", i, j, block)
					continue
				}
				blockType, typeOk := b["type"].(string)
				if !typeOk {
					s.logger.Printf("Skipping block with missing 'type' at message %d, block %d: %+v", i, j, b)
					continue
				}
				// Check for suggestion mode trigger in any of the block types we filter
				if blockType == "text" || blockType == "thinking" || blockType == "tool_use" {
					if text, textOk := b["text"].(string); textOk && strings.Contains(text, "[SUGGESTION MODE:") {
						skipMessage = true
						break
					}
				}
			}
			if skipMessage {
				continue
			}
		}

		// Add the message to the filtered list (regardless of content type)
		newMessages = append(newMessages, m)
	}
	// claudeReq["stream"] = false
	claudeReq["messages"] = newMessages

	// Transform model name based on rules
	if model, ok := claudeReq["model"].(string); ok {
		if strings.HasPrefix(model, "claude") || strings.HasPrefix(model, "step") {
			claudeReq["model"] = "stepfun/step-3.5-flash:free"
			s.logger.Printf("Transformed model '%s' to 'stepfun/step-3.5-flash:free'", model)
		} else {
			modelName := s.models.GetModelName(model) // Just for logging
			if modelName == "unknown-model" {
				s.logger.Printf("Checking if model id exists for '%s' in OpenRouter", model)
				modelId := s.models.GetModelId(model)
				if modelId == "unknown-model" {
					s.logger.Printf("Model '%s' does not have a free version in OpenRouter, using original model name", model)
				} else {
					claudeReq["model"] = modelId
					s.logger.Printf("Resolved model '%s' to free model ID '%s'", model, modelId)
				}
			} else if strings.Contains(model, "free") {
				s.logger.Printf("Model '%s' exists in OpenRouter with name '%s'", model, modelName)
				claudeReq["model"] = model
			} else {
				s.logger.Printf("Model '%s' does not have a free version in OpenRouter. Skipping request.Proxy supports free models only.", model)
			}
		}
	}

	delete(claudeReq, "context_management") // Remove context management field if present, as OpenRouter may not support it

	// Check if streaming is requested
	streamRequested := false
	if streamVal, ok := claudeReq["stream"].(bool); ok {
		streamRequested = streamVal
	}

	// Retry loop: try primary model, fallback to stepfun on persistent errors
	const maxRetries = 4
	retryableModels := []string{"stepfun/step-3.5-flash:free", "qwen/qwen3.6-plus:free"}
	if model, ok := claudeReq["model"].(string); ok {
		retryableModels[0] = model
	}

	startTime := time.Now()
	var resp *http.Response
	var usedModel string

	for _, modelToTry := range retryableModels {
		usedModel = modelToTry
		claudeReq["model"] = modelToTry
		bodyForRequest, err := json.Marshal(claudeReq)
		if err != nil {
			s.logError("Failed to marshal request for model "+modelToTry, err)
			s.writeError(w, http.StatusInternalServerError, "Internal server error")
			return
		}

		if modelToTry != retryableModels[0] {
			s.logger.Printf("Primary model failed after %d attempts, falling back to %s", maxRetries, modelToTry)
		}

		for attempt := 0; attempt < maxRetries; attempt++ {
			rt, err := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/messages", bytes.NewReader(bodyForRequest))
			if err != nil {
				s.logError("Error while creating request for model "+modelToTry, err)
				s.writeError(w, http.StatusInternalServerError, "Failed to create request")
				return
			}
			rt.Header.Set("Content-Type", "application/json")
			rt.Header.Set("Authorization", "Bearer "+s.apiKey)
			rt.Header.Set("HTTP-Referer", "http://localhost:8080")
			rt.Header.Set("X-Title", "Claude-Code-Proxy")

			resp, err = s.openRouterClient.Do(rt)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					s.logger.Printf("OpenRouter request timeout (model: %s, attempt %d/%d)", modelToTry, attempt+1, maxRetries)
				} else {
					s.logger.Printf("OpenRouter request error (model: %s, attempt %d/%d): %v", modelToTry, attempt+1, maxRetries, err)
				}
				if attempt < maxRetries-1 {
					time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
				}
				resp = nil
				continue
			}

			// 429 or 5xx errors are retryable
			if resp.StatusCode == http.StatusOK {
				break
			}

			s.logger.Printf("OpenRouter returned %d (model: %s, attempt %d/%d)", resp.StatusCode, modelToTry, attempt+1, maxRetries)
			if resp.StatusCode >= 400 && attempt < maxRetries-1 {
				resp.Body.Close()
				resp = nil
				continue
			}

			// Non-retryable error or last attempt
			break
		}

		if resp != nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
		}
		resp = nil
	}

	if resp == nil {
		modelList := strings.Join(retryableModels, ", ")
		s.writeError(w, http.StatusBadGateway, fmt.Sprintf("Failed to reach OpenRouter after retries (tried: %s)", modelList))
		s.logger.Printf("All retry attempts exhausted for models: %v", retryableModels)
		return
	}
	s.logger.Printf("Request successful with model: %s", usedModel)

	if streamRequested {
		s.handleStreamingResponse(w, resp, ctx)
	} else {
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close() // Close immediately after reading
		if err != nil {
			s.logError("Error while reading response body", err)
			s.writeError(w, http.StatusBadGateway, "Failed to read response from OpenRouter")
			return
		}

		duration := time.Since(startTime)
		s.metrics.Record("OPENROUTER_API_CALL", duration.Seconds())
		s.logger.Printf("OpenRouter API call completed in %v", duration)

		if s.queryLogger != nil {
			s.queryLogger.Printf("=== RESPONSE ===\n%s", string(respBody))
		}

		var data map[string]interface{}
		if err := json.Unmarshal(respBody, &data); err != nil {
			s.logError("Error while unmarshaling response body", err)
			s.writeError(w, http.StatusInternalServerError, "Invalid response from OpenRouter")
			return
		}

		// Filter the 'content' array to only include types Claude Code understands
		if content, ok := data["content"].([]interface{}); ok {
			newContent := []interface{}{}
			for _, block := range content {
				b, ok := block.(map[string]interface{})
				if !ok {
					s.logger.Printf("Skipping invalid block in response: %+v", block)
					continue
				}
				blockType, ok := b["type"].(string)
				if !ok {
					s.logger.Printf("Skipping block with missing 'type': %+v", b)
					continue
				}
				if blockType == "text" || blockType == "thinking" || blockType == "tool_use" {
					// Filter out any block that contains the suggestion-mode trigger in its 'text' field
					if text, textOk := b["text"].(string); textOk && strings.Contains(text, "[SUGGESTION MODE:") {
						// Skip this block entirely
						continue
					}
					newContent = append(newContent, b)
				}
			}
			data["content"] = newContent
		}

		newBody, err := json.Marshal(data)
		if err != nil {
			s.logError("Failed to marshal final response", err)
			s.writeError(w, http.StatusInternalServerError, "Internal server error")
			return
		}

		// Copy headers from OpenRouter response to client
		for k, v := range resp.Header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(newBody)))
		w.WriteHeader(resp.StatusCode)
		w.Write(newBody)

		s.metrics.Record("FULL_REQUEST", time.Since(startT).Seconds())
	}
}

func (s *Server) handleStreamingResponse(w http.ResponseWriter, resp *http.Response, ctx context.Context) {
	// Set headers for streaming
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	// Copy important headers from OpenRouter response
	for k, v := range resp.Header {
		// Skip headers that would conflict with our streaming setup
		switch strings.ToLower(k) {
		case "content-length", "content-encoding", "transfer-encoding":
			continue
		}
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}

	// Flush headers immediately
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Ensure response body is closed when we exit
	defer resp.Body.Close()

	// Use a moderate buffer to reduce syscalls
	const bufSize = 16 * 1024
	buf := make([]byte, bufSize)

	// ============================================================
	// KEEPALIVE (commented out — see below)
	// ============================================================
	// Keepalive prevents idle connection drops from load balancers
	// and proxies by sending SSE comment lines every 15 seconds.
	//
	// Disabled because:
	// 1. Claude streams continuously (~100–500ms between tokens), so
	//    idle timeouts are unlikely in practice.
	// 2. Concurrent writes to w.Data-race between reader and keepalive
	//    goroutines (http.ResponseWriter is not thread-safe).
	// 3. If needed, use a single-goroutine approach with select{} to
	//    merge keepalive into the reader loop, or use a mutex.
	//
	// keepaliveDone := make(chan struct{})
	// go func() {
	// 	defer close(keepaliveDone)
	// 	ticker := time.NewTicker(15 * time.Second)
	// 	defer ticker.Stop()
	// 	for {
	// 		select {
	// 		case <-ctx.Done():
	// 			return
	// 		case <-ticker.C:
	// 			_, err := w.Write([]byte(": keepalive\n\n"))
	// 			if err != nil {
	// 				return
	// 			}
	// 			if f, ok := w.(http.Flusher); ok {
	// 				_ = f.Flush()
	// 			}
	// 		}
	// 	}
	// }()
	// defer func() { <-keepaliveDone }()

	// Stream using a goroutine for reading with cancellation support
	readerDone := make(chan error, 1)

	go func() {
		defer close(readerDone)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				// Send data to client
				_, writeErr := w.Write(buf[:n])
				if writeErr != nil {
					readerDone <- writeErr
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					readerDone <- nil
				} else {
					readerDone <- err
				}
				return
			}
		}
	}()

	// Wait for either read completion or client cancellation
	select {
	case <-ctx.Done():
		// Client disconnected — stop streaming
		s.logger.Println("Streaming request cancelled by client")
		return
	case err := <-readerDone:
		if err != nil && !errors.Is(err, io.EOF) {
			s.logError("Streaming error", err)
		} else if err == nil {
			s.logger.Println("OpenRouter stream ended normally")
		}
	}
}

func (s *Server) logError(msg string, err error) {
	s.logger.Printf("%s: %+v\nStack trace: %s", msg, err, debug.Stack())
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

func (mr *ModelResponse) BuildModelMaps() {
	idNameMap := make(map[string]string)
	nameIdMap := make(map[string]string)

	req, err := http.NewRequest("GET", "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		log.Printf("Error creating model fetch request: %v", err)
		return
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", os.Getenv("OPENROUTER_API_KEY")))

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error fetching models from OpenRouter: %v", err)
		return
	}
	defer resp.Body.Close()

	var modelResp OpenRouterModelResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelResp); err != nil {
		log.Printf("Error decoding model response: %v", err)
		return
	}

	for _, model := range modelResp.Data {
		idNameMap[model.Id] = model.Name
		nameIdMap[model.Name] = model.Id
	}
	log.Printf("Building model maps from OpenRouter \n")
	mr.IdNameMap = idNameMap
	mr.NameIdMap = nameIdMap
	mr.LastUpdated = time.Now()
}

func (mr *ModelResponse) GetModelName(id string) string {
	if mr.IdNameMap == nil || time.Since(mr.LastUpdated) > 24*time.Hour {
		mr.BuildModelMaps()
	}
	name, exists := mr.IdNameMap[id]
	if exists {
		return name
	}
	log.Printf("Model ID '%s' does not exist in OpenRouter", id)
	return "unknown-model"
}

func (mr *ModelResponse) GetModelId(name string) string {
	if mr.NameIdMap == nil || time.Since(mr.LastUpdated) > 24*time.Hour {
		mr.BuildModelMaps()
	}
	nameIdMap := make(map[string]string)
	for k, v := range mr.NameIdMap {
		nameIdMap[k] = v
	}

	for n, id := range nameIdMap {
		if id == "stepfun/step-3.5-flash:free" {
			fmt.Println("Found free model in OpenRouter with name:", n, "and id:", id)
		}
		if strings.Contains(strings.TrimSpace(strings.ToLower(n)), strings.TrimSpace(strings.ToLower(name))) {
			if strings.Contains(strings.ToLower(id), "free") {
				log.Printf("Model '%s' exists with ID '%s' \n", name, id)
				mr.ModelNameToFreeModelIdCacheMap[name] = id
				return id
			} else {
				log.Printf("Model '%s' exists with id '%s' but does not have a free version in OpenRouter\n", name, id)
			}
		}
	}
	return "unknown-model"
}

func main() {
	server, err := LoadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}
	server.models = &ModelResponse{}
	server.models.BuildModelMaps()

	if err := server.Init(); err != nil {
		log.Fatalf("Initialization error: %v", err)
	}

	// Start metrics ticker
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			snapshot := server.metrics.Snapshot()
			for key, avg := range snapshot {
				server.logger.Printf("Average time for %s: %.2f seconds", key, avg)
				server.metrics.Reset(key)
			}
		}
	}()

	if err := server.Start(); err != nil && err != http.ErrServerClosed {
		server.logger.Fatalf("Server error: %v", err)
	}
}
