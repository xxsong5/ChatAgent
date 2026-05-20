# Chat Agent

A versatile AI agent framework built with Go, supporting multiple server modes, MCP (Model Context Protocol) integration, and sandboxed code execution.

## Features

- **Multi-Server Support**: OpenAI-compatible API, MCP, CLI, Web UI, WeChat integration
- **MCP Integration**: Connect to external MCP servers via stdio or HTTP
- **Skill System**: Discoverable skill registry for extending capabilities
- **Sandbox Execution**: Docker-based isolated code execution environment
- **Session Management**: Persistent chat sessions with configurable TTL
- **Tool Registry**: Extensible tool system (shell commands, file stash, skills)

## Project Structure

```
chat-agent/
├── agent/          # Core agent logic (loop, streaming, events)
├── config/         # Configuration files and skills
├── mcp/            # MCP client and manager
├── sandbox/        # Docker sandbox execution
├── server/         # Server implementations (web, openai, mcp, cli, weixin)
├── skill/          # Skill discovery and registry
├── tool/           # Tool implementations
├── types/          # Shared types and configuration
├── main.go         # Application entry point
└── go.mod          # Go module definition
```

## Configuration

Edit `config/config.json`:

```json
{
    "base_url": "https://api.deepseek.com",
    "api_key": "DEEPSEEK_API_KEY",
    "model": "deepseek-chat",
    "server": {
        "type": "web",
        "host": "0.0.0.0",
        "port": 8080
    },
    "sandbox": {
        "enabled": true,
        "image": "chat-agent:latest"
    }
}
```

## Quick Start

1. Set your API key:
   ```bash
   export DEEPSEEK_API_KEY=your_api_key
   ```

2. Build and run:
   ```bash
   go build -o chat-agent
   ./chat-agent
   ```

3. Access the Web UI at `http://localhost:8080`

## Server Types

| Type | Description |
|------|-------------|
| `web` | Web UI server (default) |
| `openai` | OpenAI-compatible API |
| `mcp` | MCP HTTP server |
| `cli` | Command-line interface |
| `weixin` | WeChat integration |

## Skills

Place skill definitions in `config/skills/` directory. The agent automatically discovers and loads skills at startup.
