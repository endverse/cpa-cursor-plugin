# Cursor CPA Plugin

[中文](README.zh-CN.md)

This repository is for testing and research only. It is not an official Cursor integration, it does not offer production guarantees, and it does not include support. Use only a Cursor account you are authorized to use.

This plugin connects [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) to Cursor's cloud Agent API. Claude Code uses Anthropic Messages, and the Codex CLI uses OpenAI Responses. The plugin sends and receives both directly. It does not pass them through a Chat Completions translation, which would drop thinking signatures.

The plugin id is `cursor-agent`. The store already has another plugin with the id `cursor`, so this plugin does not use that name.

Each new user message starts a new Run and carries the full history. A stable `conversation_id` sends that request back to the machine that holds the prompt cache. After the tool calls in one reply finish, the next user message, including tool results, imports the whole history again.

## Build

```bash
make build
```

The build writes `dist/cursor-agent.so`. CPA treats the filename as the plugin id, so the configured name is `cursor-agent`. A local macOS build is still a `.so`. The Darwin archive published to the plugin store names that library `cursor-agent.dylib`.

```yaml
plugins:
  enabled: true
  dir: /absolute/path/to/dist
```

## Sign in

After installation, open Cursor sign-in from the CPA management page, or choose 登录 in the plugin menu. The page opens the `cursor.com` sign-in flow directly. When it finishes, the account is written into CPA's auth directory. You do not paste JSON or an API key by hand.

Browser sign-in expires after about two months. Cursor does not provide a refresh API for third parties. Sign in again after it expires.

If you already have a token, or credentials from a local `cursor-agent login`, you can still put a JSON file in the auth directory. Set `cursor_auth.type` to `oauth` (field `access`), `api` (field `key`), or `cli`. An optional `proxy_url` applies only to that account. When it is omitted, the process uses `HTTPS_PROXY`.

## Clients

Point Claude Code's Anthropic base URL at this CPA. Requests go to `/v1/messages`. Point Codex at `/v1/responses` on the same CPA, or at CPA's Codex-compatible path. Use a model id from the account catalog, or `auto`.

Thinking is returned to the client in an opaque field that starts with `cps1.`. It is valid only for requests that pass through this plugin. On the next turn it places the same reasoning back into Cursor's conversation record.

## Limits

- The client's own tools, such as bash and edit, are returned as tool calls for the client to run.
- Cursor's own shell, file reads, and web search are not executed. A server-side `web_search` in the request is removed.
- `responses/compact` and the Responses WebSocket fail.
- When Cursor does not report usage for a tool step in that Run, the plugin estimates it from the text length.
- Outbound traffic prefers the HTTP/2 bidirectional Run. If that connection cannot be established, the plugin switches to HTTP/1.1 RunSSE + BidiAppend and keeps that path for ten minutes.

## License

[MIT](LICENSE).
