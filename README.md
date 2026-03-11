# OpenRouter Proxy for Claude Code

A production-ready reverse proxy that forwards Claude Code API requests to OpenRouter, with message filtering, proper error handling, and enterprise features.

## Features

- **Full API Compatibility**: Implements Claude Code's `/v1/messages` endpoint
- **Message Filtering**: Automatically removes suggestion mode triggers
- **Production Ready**:
  - Thread-safe metrics tracking
  - Graceful shutdown with request draining
  - Health check endpoint (`/health`)
  - Request timeout handling (120s)
  - Stack traces on errors for debugging
- **Observability**: Structured logging with file output, metrics aggregation
- **Secure**: No sensitive data in logs, proper error responses
- **Zero Configuration**: Works out of the box with sensible defaults

## Quick Start

### Prerequisites

- Go 1.21+ (or download the binary)
- OpenRouter API key from [openrouter.ai](https://openrouter.ai)

### Build from Source

```bash
git clone https://github.com/AadhilFarhan/openrouter-proxy.git
cd openrouter-proxy
go mod download
go build -o openrouter-proxy .
```

### Run

```bash
# Set your OpenRouter API key
export OPENROUTER_API_KEY="sk-or-v1-..."

# Run (default port 8080)
./openrouter-proxy

# Or with custom port
export PORT=8080
./openrouter-proxy
```

### Test

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic/claude-sonnet-4-5-20250930",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## Configure Claude Code

In your Claude Code settings (`Cmd/Ctrl + Shift + P` → "Claude Code: Settings"):

```json
{
  "claude.code.apiUrl": "http://your-server-ip:8080/v1/messages"
}
```

Replace `your-server-ip` with your EC2 instance's public IP or domain.

## Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `OPENROUTER_API_KEY` | **Required.** Your OpenRouter API key | (none) |
| `PORT` | Port to listen on | `8080` |
| `LOG_FILE` | Path to log file | `./logs/claude-proxy.log` (auto-created) |
| `OPENROUTER_TIMEOUT` | Timeout for OpenRouter requests | `120s` |

### Example with Docker

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN go build -o openrouter-proxy .

FROM alpine:latest
COPY --from=builder /app/openrouter-proxy /usr/local/bin/
EXPOSE 8080
CMD ["openrouter-proxy"]
```

```bash
docker build -t openrouter-proxy .
docker run -d -p 8080:8080 -e OPENROUTER_API_KEY=your-key openrouter-proxy
```

## API Reference

### `POST /v1/messages`

Accepts the same request format as Anthropic's Claude API.

**Request:**
```json
{
  "model": "anthropic/claude-sonnet-4-5-20250930",
  "max_tokens": 1024,
  "messages": [
    {"role": "user", "content": "Hello!"}
  ]
}
```

**Response:**
Same as OpenRouter's response format. Content blocks are filtered to only include `text`, `thinking`, and `tool_use` types (Claude Code compatible).

### `GET /health`

Health check endpoint for monitoring.

**Response:**
```json
{
  "status": "healthy",
  "time": "2026-03-11T12:00:00Z"
}
```

## Deployment

### EC2 (Ubuntu/Debian)

```bash
# 1. Build for Linux (from your Mac/Linux)
GOOS=linux GOARCH=amd64 go build -o openrouter-proxy .

# 2. Copy to EC2
scp openrouter-proxy ec2-user@your-ec2-ip:/opt/openrouter-proxy/

# 3. SSH into EC2
ssh ec2-user@your-ec2-ip

# 4. Set up as service (optional)
sudo nano /etc/systemd/system/openrouter-proxy.service
```

**systemd service file:**
```ini
[Unit]
Description=OpenRouter Proxy
After=network.target

[Service]
Type=simple
Environment="OPENROUTER_API_KEY=your-key"
Environment="PORT=8080"
ExecStart=/opt/openrouter-proxy/openrouter-proxy
Restart=always
RestartSec=10
User=ec2-user
WorkingDirectory=/opt/openrouter-proxy

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable openrouter-proxy
sudo systemctl start openrouter-proxy
sudo systemctl status openrouter-proxy

# View logs
sudo journalctl -u openrouter-proxy -f
```

### Security Group Settings

Allow inbound traffic on port `8080` (or your chosen port) from your IP or 0.0.0.0/0 if public.

## Monitoring

- **Logs**: Written to both stdout and `LOG_FILE` (default: `./logs/claude-proxy.log`)
- **Metrics**: Periodic logs every 60s showing average API response times
- **Health**: `GET /health` returns 200 if service is alive

## Troubleshooting

### "OPENROUTER_API_KEY environment variable is not set"
Make sure you've exported the variable before starting:
```bash
export OPENROUTER_API_KEY="sk-or-v1-..."
```

### Binary not found / permission denied
```bash
chmod +x openrouter-proxy
```

### Log files not appearing
The `logs/` directory is auto-created. Check permissions:
```bash
ls -la logs/
```

### Cannot connect from Claude Code
1. Check security group allows port 8080
2. Verify server is running: `curl http://your-server:8080/health`
3. Check logs: `tail -f logs/claude-proxy.log`

### OpenRouter errors
- Ensure your API key is valid and has credits
- Check [OpenRouter status](https://openrouter.ai/status)
- Verify model name is correct (use format: `anthropic/claude-sonnet-4-5-20250930`)

## Development

### Run with hot reload (using air)
```bash
go install github.com/cosmtrek/air@latest
air
```

### Test locally
```bash
# Start server
./openrouter-proxy

# In another terminal
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -d '{"model":"anthropic/claude-3-5-sonnet-20241022","max_tokens":100,"messages":[{"role":"user","content":"test"}]}'
```

## How It Works

1. Claude Code sends request to `/v1/messages` on your proxy server
2. Proxy validates the request body
3. Filters out messages containing `[SUGGESTION MODE:...]` triggers
4. Forwards request to OpenRouter with your API key
5. Receives response, filters content blocks to only `text`, `thinking`, `tool_use`
6. Returns filtered response to Claude Code
7. Metrics are logged every 60 seconds

## License

MIT

## Acknowledgments

- Built for [Claude Code](https://claude.ai/code)
- Powered by [OpenRouter](https://openrouter.ai)
