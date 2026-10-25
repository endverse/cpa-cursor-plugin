# Cursor CPA 插件

本仓库仅供测试和研发使用。它不是 Cursor 的官方接入，不提供生产环境保障，也不附带服务支持。请只使用你本人有权使用的 Cursor 账号。

这个插件把 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 接到 Cursor 的云端 Agent API。Claude Code 走 Anthropic Messages，Codex CLI 走 OpenAI Responses，两条都由插件直接收发，不经过会丢掉思考签名的 Chat Completions 转换。

插件 id 是 `cursor-agent`。市场里已经有另一个 id 为 `cursor` 的插件，所以这里不用那个名字。

每次新的用户消息都会带上完整历史重新发起一次 Run，并用稳定的 `conversation_id` 让 Cursor 把这次请求送回持有 prompt cache 的机器。同一次回复里的工具调用结束后，下一轮用户消息（含工具结果）再导入整段历史。

## 构建

```bash
make build
```

产物是 `dist/cursor-agent.so`。CPA 用文件名当作插件 id，所以配置里的名字是 `cursor-agent`。macOS 上本地编译出来的仍是 `.so`；放进插件市场的 Darwin 压缩包会把它命名为 `cursor-agent.dylib`。

```yaml
plugins:
  enabled: true
  dir: /absolute/path/to/dist
```

## 登录

装好插件后，在 CPA 管理页点 Cursor 的登录，或点插件菜单里的「登录」。页面会直接打开 `cursor.com` 的登录，完成后账号写进 CPA 的 auth 目录，不用手填 JSON 或 API key。

浏览器登录大约两个月过期。Cursor 没有给第三方用的刷新接口，过期后再点一次登录。

已经有 token 或本机 `cursor-agent login` 凭证时，仍可以放一份 JSON 到 auth 目录：`cursor_auth.type` 取 `oauth`（字段 `access`）、`api`（字段 `key`）或 `cli`。可选 `proxy_url` 只作用于这一个账号；没写时使用进程的 `HTTPS_PROXY`。

## 客户端

Claude Code 把 Anthropic Base URL 指到这台 CPA，请求打到 `/v1/messages`。Codex 把接口指到同一台 CPA 的 `/v1/responses`（或 CPA 的 Codex 兼容路径）。模型用账号模型列表里的 id，或者 `auto`。

思考内容会以 `cps1.` 开头的不透明字段回给客户端。它只在经过本插件时有效，用来下一轮把同一段推理放回 Cursor 的对话记录。

## 边界

- 客户端自己的工具（bash、编辑等）会作为 tool call 交回去，由客户端执行。
- Cursor 自己的 shell、读文件、网页搜索不会执行。请求里的服务端 `web_search` 会被去掉。
- `responses/compact` 和 Responses WebSocket 会失败。
- 工具步骤如果 Cursor 没有在这次 Run 里给出用量，插件按文本长度估算。
- 出站优先走 HTTP/2 双向 Run；连不上时改走 HTTP/1.1 的 RunSSE + BidiAppend，并在十分钟内继续用这条路径。

## 协议

[MIT](LICENSE)。
