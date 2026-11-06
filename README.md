<div align="center">
  <h1>WordRush · Backend</h1>
  <p>单词闯关应用后端 · Go + Gin，进度同步、经济结算与 AI 口语对话</p>
</div>

<div align="center">

![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8)
![Gin](https://img.shields.io/badge/Gin-v1-008ECF)
![Cloudflare](https://img.shields.io/badge/AI-Workers%20AI-F38020)
![License](https://img.shields.io/badge/License-MIT-green)

</div>

## 项目定位

WordRush 是一个单词闯关 / 复习类学习应用，本仓库是它的**后端 API**。前端是
[WordRush-web](https://github.com/CheeseTJ/WordRush-web)（React + Vite，可打包成
PWA 与 Android 壳）。

除了常规的学习进度与经济系统，它还有两条有意思的链路：

- **AI 口语对话** —— 场景化对话，LLM 由 Cloudflare Workers AI 提供
- **发音评测** —— 接自托管的 OpenPronounce 微服务，做 TTS / STT / 发音打分

```text
浏览器 / Capacitor 壳 ──> WordRush-web
                              │  fetch /api/*
                              ▼
                        本仓库 (Go + Gin)
                              ├──> MySQL           用户进度 / 经济 / 会话
                              ├──> Workers AI      口语对话（OpenAI 兼容端点）
                              └──> OpenPronounce   TTS / STT / 发音评测
```

## 功能

### 学习与进度
- 课程 / 单元 / 单词三级结构，闯关记录与错题本
- 复习队列（间隔重复），复习结果回写掌握度
- 学习进度跨设备同步（同一账号）

### 经济系统
- 学习行为产出货币，解锁场景与装扮
- 结算在后端做，客户端只提交事件，不接受客户端计算结果

### AI 口语
- 场景化对话，按角色与情境生成回复
- 接 Workers AI 的 OpenAI 兼容端点，可通过 `enable_thinking: false` 关闭思考链，
  **把输出 token 降两个数量级**，显著省额度、降延迟
- 对话历史与会话状态持久化

### 发音评测
- `/api/v1/speak/tts` —— 文本转语音（16 kHz mono WAV）
- `/api/v1/chat/stt` —— 语音转文本
- `/api/v1/speak/assess` —— 发音打分
- 后端是对 OpenPronounce 的封装，评测服务地址由 `speak_eval.base_url` 配置

### 其他
- **统一鉴权** —— 统一身份中心的 JWT，JWKS 校验 `aud`
- **版本检查** —— `/api/app/version`，供 Android 壳判断是否需更新
- **健康检查** —— `/api/health` 带数据库探活

## 快速开始

```bash
cp config.example.yaml config.yaml
# 按需修改；密钥走环境变量，见下

go build -o bin/wordrush ./cmd/wordrush
./bin/wordrush
```

或 Docker：

```bash
docker compose up -d --build
curl http://localhost:8092/api/health
```

### 环境变量

密钥不写入配置文件，全部用 `WORDRUSH_*` 前缀覆盖（见 `internal/config/config.go`）：

```bash
export WORDRUSH_DB_PASSWORD=...
export WORDRUSH_AI_API_TOKEN=...     # Cloudflare Workers AI
```

### 外部依赖

| 依赖 | 是否必需 | 说明 |
|---|---|---|
| MySQL | 必需 | OCI HeatWave（MySQL 兼容）。自签证书用 `tls_mode: preferred` |
| 统一身份中心 | 必需 | 提供 JWKS，校验 JWT |
| Cloudflare Workers AI | 口语对话需要 | OpenAI 兼容端点，填 `account_id` + token |
| OpenPronounce | 发音评测需要 | 自托管 Python 微服务，默认 `http://127.0.0.1:8100` |

发音评测服务可选：不配置时 TTS / STT / 评测接口不可用，其余功能正常。

## 配置说明

```yaml
server:
  addr: ":8092"
  allowed_origins: [...]     # 浏览器直连白名单

db:
  host / port / name / user  # 密码走环境变量
  tls_mode: "preferred"      # HeatWave 自签证书必须用它

speak_eval:
  base_url: "http://127.0.0.1:8100"
  timeout: "60s"

ai:
  account_id: "..."           # Workers AI 账号
  model: "@cf/..."
  enable_thinking: false      # 关闭思考链，省额度
```

## 目录结构

```text
cmd/wordrush/          入口
internal/api/          HTTP 路由与 handler
internal/auth/         JWT / JWKS 校验
internal/ai/           Workers AI 客户端（口语对话）
internal/speak/        OpenPronounce 封装（TTS / STT / 评测）
internal/store/        MySQL 访问层
internal/release/      版本检查
internal/config/       配置加载 + 环境变量覆盖
lib/                   sherpa-onnx / onnxruntime 运行库
```

## 部署

仓库自带 `.github/workflows/deploy.yml`：push 到 `master` 后由 self-hosted runner
构建镜像并 `docker compose up -d --build`。

> 该 job 含 `if: github.repository_owner == 'CheeseTJ'` 门禁——self-hosted runner 与
> 生产服务同机，这道门禁防止外部 fork/PR 触发在服务器上执行代码。

## License

[MIT](./LICENSE) © 2026 JuneT
