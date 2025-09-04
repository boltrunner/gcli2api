# gemini-cli-2api

一个用 Go 实现的轻量级 HTTP 服务，它将 `gemini-cli` 所使用的 API 封装为 “Gemini v1beta 风格” 的 HTTP 接口。
核心功能包括非流式与 SSE 流式生成、多账户轮询和自动重试。

## 安装

从 [Releases 页面](https://github.com/boltrunner/gcli2api/releases)下载最新的二进制文件。

配置文件请参考 [`config.json.example`](./config.json.example)。

## Docker 使用
- **准备配置**: 将凭据与数据路径映射到容器内。例如，在宿主机准备 `config.json` 并设置如下路径:
  - `geminiOauthCredsFiles`: `"/secrets/account1/oauth_creds.json"`
  - `sqlitePath`: `"/app/data/state.db"`
- **运行服务**:
  ```bash
  docker run --rm \
    -p 8085:8085 \
    -v $(pwd)/config.json:/app/config.json:ro \
    -v ~/.gemini_accounts:/secrets:rw \
    -v $(pwd)/data:/app/data:rw \
    --name gcli2api \
    boltrunner000/gcli2api:latest
  ```
  - **说明**: 容器内默认执行 `server -c /app/config.json` 命令，并暴露 `8085` 端口。
  - **健康检查**: `curl http://127.0.0.1:8085/health`
- **校验配置 (容器内)**:
  ```bash
  docker run --rm \
    -v $(pwd)/config.json:/app/config.json:ro \
    boltrunner000/gcli2api:latest check -c /app/config.json
  ```
- **写入权限提示**: 容器默认以非 root 用户 (UID 10001) 运行。如需将刷新后的 token 写回挂载目录，请确保该目录对容器内用户可写。如果遇到权限问题，可以尝试使用宿主机的 UID 运行容器：
  ```bash
  docker run --rm -p 8085:8085 \
    -u $(id -u):$(id -g) \
    -v $(pwd)/config.json:/app/config.json:ro \
    -v ~/.gemini_accounts:/secrets:rw \
    -v $(pwd)/data:/app/data:rw \
    boltrunner000/gcli2api:latest
  ```


## 获取多账户凭据

要为多个账户生成凭据，请重复以下步骤：
1. 将当前的 `~/.gemini` 目录重命名或移走。
2. 运行 `gemini-cli`，在浏览器中登录一个新账户。
3. 保存新生成的 `~/.gemini/oauth_creds.json` 文件。

将获取到的多个凭据文件路径填入 `config.json` 即可实现轮询。

## 从源码构建

```
git clone https://github.com/boltrunner/gcli2api.git
cd gcli2api
go build -o gcli2api .
./gcli2api server --config config.json
```

## 命令
- `server`：启动 HTTP 服务（启动前会校验配置）
  - 示例：`go run . server -c ./config.json`
- `check`：校验配置文件（包含未知键检测与 authKey 占位符检测）
  - 示例：`go run . check -c ./config.json`

未传子命令时默认等价于 `server`。

## 主要功能
- **Gemini 风格接口**:
  - `GET /health`: 健康检查
  - `GET /v1beta/models`: 模型列表 (内置 `gemini-2.5-flash`, `gemini-2.5-pro`)
  - `POST /v1beta/models/<model>:generateContent`: 非流式生成
  - `POST /v1beta/models/<model>:streamGenerateContent`: SSE 流式生成
- **请求轮询与重试**: 支持多凭据/项目单元轮询；`requestMaxRetries` 用于在不同单元间旋转重试（总尝试次数 = 1 + 重试次数）。针对 `401/403/429/5xx` 和常见网络错误发生时进行旋转重试；旋转为“立即切换”，不做指数退避。流式请求仅在首个事件发送前允许旋转，首个事件后不再切换。
- **状态缓存**: 自动将 GCP Project ID 缓存至 SQLite 数据库 (默认为 `./data/state.db`)。
- **API Key 认证**: 可设置 `authKey`，要求客户端在请求时提供 `Authorization: Bearer <key>` 或 `x-goog-api-key: <key>`。

## 快速开始
1) 准备 OAuth 凭据 JSON（含可刷新令牌）
2) 放置配置文件 `config.json`（见下）
3) 启动服务：`go run . server -c ./config.json`
4) 健康检查：`curl http://127.0.0.1:8085/health`

## 配置说明（config.json）
字段（小写）：
- `host`（默认 `127.0.0.1`）
- `port`（默认 `8085`）
- `authKey`（可选，若为占位符 `UNSAFE-KEY-REPLACE` 则校验失败）
- `geminiOauthCredsFiles`：凭据文件路径数组（必填）
- `projectIds`：可选。以“凭据文件路径”为键、以“Project ID 数组”为值的映射。键会进行 `~` 展开（不解析符号链接），并且必须与 `geminiOauthCredsFiles` 中的某一项完全匹配；否则 `check` 会失败。若某个键对应的数组为空，则视为未配置、回退到自动发现。若数组中包含特殊标记 `"_auto"`，表示除显式列出的项目外，还应加入一个“自动发现”的项目单元。
- `requestMaxRetries`（默认 `3`）：跨单元重试预算（总尝试次数 = 1 + 重试次数）。
- `requestBaseDelay`（毫秒，默认 `1000`）
- `sqlitePath`（默认 `./data/state.db`）

校验规则：
- 配置包含未知键将报错并指出键名。
- `authKey` 若为示例占位符 `UNSAFE-KEY-REPLACE` 将报错。

示例：
```json
{
  "authKey": "YOUR-SECRET-KEY",
  "port": 8085,
  "host": "127.0.0.1",
  "geminiOauthCredsFiles": [
    "~/.gemini_account1/oauth_creds.json",
    "~/.gemini_account2/oauth_creds.json"
  ],
  "projectIds": {
    "~/.gemini_account1/oauth_creds.json": ["project-id1", "project-id2"]
  },
  "requestMaxRetries": 2,
  "requestBaseDelay": 1000,
  "sqlitePath": "./data/state.db"
}
```

## 按凭据指定 Project ID（projectIds）

- 用法：在 `config.json` 中新增 `projectIds` 字段，为映射类型：
  - 键：凭据文件路径（支持 `~` 展开，不解析符号链接）。
  - 值：该凭据下要使用的一个或多个 Project ID（有序）。
- 负载均衡：每个 “(凭据, Project ID)” 组合视为独立的轮询单元；相同凭据的多个项目共享同一 HTTP/OAuth 客户端。
- 回退策略：
  - 未出现在 `projectIds` 的凭据，继续使用自动发现 Project ID，并将结果缓存到 SQLite。
  - 若某个键的数组为空，则记录警告并回退到自动发现（等价于未配置）。
  - 若某个凭据的 `projectIds` 数组包含特殊标记 `"_auto"`，则额外加入一个“自动发现的 Project ID” 轮询单元；若未包含该标记，则仅使用显式列出的项目。
- 校验：`gcli2api check -c ./config.json` 会在以下情况下失败：
  - `projectIds` 中存在经 `~` 展开后无法与 `geminiOauthCredsFiles` 精确匹配的键。
- 重试/轮换策略：遇到 `401/403/429/5xx`、项目发现失败或常见网络错误时在单元间旋转；单凭据部署下将对同一单元重试。流式场景仅在首个事件之前允许旋转，之后不再切换。

示例:
```json
{
  "geminiOauthCredsFiles": [
    "~/.gemini_account1/oauth_creds.json",
    "~/.gemini_account2/oauth_creds.json"
  ],
  "projectIds": {
    "~/.gemini_account1/oauth_creds.json": ["_auto", "project-id1", "project-id2"]
  }
}
```

上面这个配置:
- 对于 account1 会使用自动发现的 project id 以及显式指定的两个 project id
- 对于 account2（未在 projectIds 中出现）会使用自动发现的 project id

## API 约定与请求格式
- 模型名：`gemini-2.5-flash`、`gemini-2.5-pro`
- 请求体字段遵循 Gemini 风格（`contents`、`generationConfig` 等）

示例（非流式）：
```bash
curl -X POST \
  -H "Authorization: Bearer <你的API_KEY>" \
  -H "Content-Type: application/json" \
  http://127.0.0.1:8085/v1beta/models/gemini-2.5-flash:generateContent \
  -d '{"contents":[{"role":"user","parts":[{"text":"你好，介绍一下你自己"}]}]}'
```

## 测试与覆盖率
- 运行测试：`go test ./...`
- 覆盖率报告：`go test ./... -coverprofile=coverage.out && go tool cover -html=coverage.out`

## 认证与安全
- **OAuth**: 通过 `geminiOauthCredsFiles` 提供凭据。服务会自动刷新 token 并以 `0600` 权限持久化。
- **API Key**: 配置 `authKey` 后，受保护的接口需要提供正确的 Key。

## 致谢

本项目的灵感来自 [justlovemaki/AIClient-2-API](https://github.com/justlovemaki/AIClient-2-API)，并借鉴了其实现和配置方式。

## 已知限制
- 目前内置可用模型为 `gemini-2.5-flash` 与 `gemini-2.5-pro`。
