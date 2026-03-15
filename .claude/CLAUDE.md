# Claude Code Configuration for OpenRouter Proxy

This file defines custom slash commands and project-specific instructions for the OpenRouter Proxy project.

## Project Overview

OpenRouter Proxy is a Go-based reverse proxy that forwards Claude Code API requests to OpenRouter with automatic model transformation, message filtering, and Stream support.

**Tech Stack**: Go 1.26+
**Main Entry**: `main.go`
**Build Tool**: Go modules
**Architecture**: HTTP server with streaming support

---

## Slash Commands with Auto-Triggers

### /build
- Trigger: Files matching `*.go` or `go.mod` changed
- Description: Build the Go binary for the current platform
- Command: `go build -o openrouter-proxy .`

### /run
- Trigger: No current trigger (manual-only)
- Description: Build and run the proxy locally with default configuration
- Command:
  ```
  go build -o openrouter-proxy .
  LOG_FILE=./logs/claude-proxy.log ./openrouter-proxy
  ```

### /test
- Trigger: Files matching `*_test.go` changed
- Description: Run Go tests with verbose output
- Command: `go test -v ./...`

### /lint
- Trigger: Files matching `*.go` changed
- Description: Run go vet and fmt to check code quality
- Command:
  ```
  go vet ./...
  go fmt ./...
  ```

### /clean
- Trigger: No current trigger (manual-only)
- Description: Remove build artifacts and logs
- Command: `rm -f openrouter-proxy && rm -rf logs/`

### /deploy-build
- Trigger: Branch is `main` and files matching `*.go` or `go.mod` changed
- Description: Build Linux binary for deployment to EC2/cloud
- Command: `GOOS=linux GOARCH=amd64 go build -o openrouter-proxy-linux-amd64 .`

### /health-check
- Trigger: No current trigger (manual-only)
- Description: Check if the proxy server is running and healthy
- Command: `curl -s http://localhost:8080/health | head -1`

---

## Project Conventions

### Code Style
- Follow standard Go formatting (`go fmt`)
- Use meaningful variable names with Hungarian notation prefix optional
- Add comments for exported functions and complex logic
- Error messages should be descriptive and include context

### Commit Messages
- Use conventional commits: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`
- Keep first line under 72 characters
- Include relevant issue numbers if applicable

### Environment Configuration
- Required: `OPENROUTER_API_KEY`
- Optional: `PORT`, `LOG_FILE`, `OPENROUTER_TIMEOUT`, `LOG_QUERIES`
- Default timeout: 30 minutes (suitable for AI inference)
- Default port: 8080

### Testing
- Add table-driven tests for complex functions
- Test edge cases: network errors, malformed JSON, timeouts
- Use `httptest` for HTTP handler tests
- Mock external dependencies when testing

### Logging
- All errors should include stack traces (`debug.Stack()`)
- Log user queries for debugging (respect `LOG_QUERIES` flag)
- Use structured logging with the project's `*log.Logger`
- Log levels: Info for normal operations, errors with full context

---

## Important Files

| File | Purpose |
|------|---------|
| `main.go` | Main server implementation with all handlers |
| `go.mod` | Go module definition and dependencies |
| `README.md` | Project documentation and API reference |
| `logs/` | Log directory (auto-created) |

---

## Development Workflow

1. **Make changes** to `main.go` or related files
2. **Build**: Use `/build` or run manually
3. **Test locally**: Set `OPENROUTER_API_KEY` and run with `/run`
4. **Check health**: Use `/health-check` after starting
5. **Test API**: Use curl or Claude Code configured to point to `http://localhost:8080`
6. **Lint**: Run `/lint` before committing
7. **Deploy**: Use `/deploy-build` to create Linux binary

---

## Key Implementation Details

- **Streaming**: Uses buffered I/O with 64KB output buffer and 32KB input buffer
- **Model Transformation**: Claude → StepFun, Others → NVIDIA (see `messageHandler` in main.go)
- **Content Filtering**: Removes `[SUGGESTION MODE:...]` triggers from requests and responses
- **Metrics**: Tracks `FULL_REQUEST` and `OPENROUTER_API_CALL` durations
- **Graceful Shutdown**: 10-second drain period on SIGINT/SIGTERM
- **Connection Pool**: Max 200 idle connections, 50 per host

---

## Notes

- The proxy only implements the `/v1/messages` endpoint (Claude Code compatible)
- Streaming is automatically supported when `"stream": true` in request
- Health check endpoint: `/health` returns JSON with status and timestamp
- All OpenRouter errors are proxied with appropriate HTTP status codes
- The server runs indefinitely until interrupted; use systemd for production
