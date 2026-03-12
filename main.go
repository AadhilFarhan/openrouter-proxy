package main

import (
	"bufio"
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
	metrics          *MetricsCollector
	openRouterClient *http.Client
	timeout          time.Duration
}

type MetricsCollector struct {
	mu     sync.RWMutex
	timers map[string][]float64
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

	// Configure transport with connection pool limits
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     100,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}

	return &Server{
		apiKey:           apiKey,
		logFile:          logFile,
		port:             port,
		timeout:          timeout,
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
	return nil
}

func (s *Server) Start() error {
	http.HandleFunc("/v1/messages", s.messageHandler)
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
	if messages, ok := claudeReq["messages"].([]interface{}); ok {
		var userQueries []string
		for _, msg := range messages {
			if m, ok := msg.(map[string]interface{}); ok {
				if role, ok := m["role"].(string); ok && role == "user" {
					if content, ok := m["content"].(string); ok {
						userQueries = append(userQueries, content)
					} else if contentArr, ok := m["content"].([]interface{}); ok {
						// Handle array of content blocks
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
			s.logger.Printf("User query: %s", strings.Join(userQueries, " | "))
		}
	}
}

func (s *Server) messageHandler(w http.ResponseWriter, req *http.Request) {
	startT := time.Now()
	ctx := req.Context()

	s.logger.Printf("Received %s request for %s", req.Method, req.URL.Path)

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

	// Log the user query (extracted from user messages)
	s.logUserQuery(claudeReq)

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
		if strings.HasPrefix(model, "claude") {
			claudeReq["model"] = "stepfun/step-3.5-flash:free"
			s.logger.Printf("Transformed model '%s' to 'stepfun/step-3.5-flash:free'", model)
		} else if !strings.HasPrefix(model, "stepfun") {
			claudeReq["model"] = "nvidia/nemotron-3-nano-30b-a3b:free"
			s.logger.Printf("Transformed model '%s' to 'nvidia/nemotron-3-nano-30b-a3b:free'", model)
		}
	}

	cleanedBody, err := json.Marshal(claudeReq)
	if err != nil {
		s.logError("Failed to marshal cleaned request", err)
		s.writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	// Check if streaming is requested
	streamRequested := false
	if streamVal, ok := claudeReq["stream"].(bool); ok {
		streamRequested = streamVal
	}

	reqToOpenRouter, err := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/messages", bytes.NewBuffer(cleanedBody))
	if err != nil {
		s.logError("Error while creating request", err)
		s.writeError(w, http.StatusInternalServerError, "Failed to create request")
		return
	}

	reqToOpenRouter.Header.Set("Content-Type", "application/json")
	reqToOpenRouter.Header.Set("Authorization", "Bearer "+s.apiKey)
	reqToOpenRouter.Header.Set("HTTP-Referer", "http://localhost:8080")
	reqToOpenRouter.Header.Set("X-Title", "Claude-Code-Proxy")

	startTime := time.Now()

	if streamRequested {
		// Handle streaming response
		s.handleStreamingResponse(w, reqToOpenRouter)
	} else {
		// Handle non-streaming response (existing logic)
		resp, err := s.openRouterClient.Do(reqToOpenRouter)
		if err != nil {
			// Check for timeout
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				s.logError("OpenRouter request timeout", err)
				s.writeError(w, http.StatusGatewayTimeout, "OpenRouter request timed out after "+s.timeout.String())
				return
			}
			s.logError("Error while sending request to OpenRouter", err)
			s.writeError(w, http.StatusBadGateway, "Failed to reach OpenRouter")
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			s.logError("Error while reading response body", err)
			s.writeError(w, http.StatusBadGateway, "Failed to read response from OpenRouter")
			return
		}

		duration := time.Since(startTime)
		s.metrics.Record("OPENROUTER_API_CALL", duration.Seconds())
		s.logger.Printf("OpenRouter API call completed in %v", duration)

		if resp.StatusCode != http.StatusOK {
			s.logger.Printf("Received non-OK response from OpenRouter: %d %s", resp.StatusCode, string(respBody))
			s.writeError(w, http.StatusBadGateway, fmt.Sprintf("OpenRouter error: %d", resp.StatusCode))
			return
		}

		s.logger.Printf("OpenRouter raw response: %s", string(respBody))

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

func (s *Server) handleStreamingResponse(w http.ResponseWriter, reqToOpenRouter *http.Request) {
	ctx := reqToOpenRouter.Context()

	// Set headers for streaming
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	// Flush the headers immediately
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	resp, err := s.openRouterClient.Do(reqToOpenRouter)
	if err != nil {
		// Check for timeout
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			s.logError("OpenRouter streaming request timeout", err)
			s.writeError(w, http.StatusGatewayTimeout, "OpenRouter request timed out after "+s.timeout.String())
		} else {
			s.logError("Error while sending streaming request to OpenRouter", err)
			s.writeError(w, http.StatusBadGateway, "Failed to reach OpenRouter")
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read error body
		errBody, _ := io.ReadAll(resp.Body)
		s.logger.Printf("OpenRouter returned non-OK status: %d, body: %s", resp.StatusCode, string(errBody))
		s.writeError(w, http.StatusBadGateway, fmt.Sprintf("OpenRouter error: %d", resp.StatusCode))
		return
	}

	// Stream the response from OpenRouter to the client
	// Use a buffered reader for efficient reading.
	const bufSize = 32 * 1024 // 32 KB buffer
	buf := make([]byte, bufSize)
	reader := bufio.NewReader(resp.Body)
	for {
		select {
		case <-ctx.Done():
			s.logger.Println("Streaming request cancelled by client")
			return
		default:
			n, err := reader.Read(buf)
			if n > 0 {
				// Forward the chunk to the client
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					s.logError("Error writing to client", writeErr)
					return
				}
				// Flush to ensure immediate delivery
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					s.logger.Println("OpenRouter stream ended")
				} else {
					s.logError("Error reading from OpenRouter stream", err)
				}
				return
			}
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

func main() {
	server, err := LoadConfig()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

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
