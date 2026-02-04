# a2o

**一句话说明：** 让你的 Claude Code、Cursor 等工具，通过任意 OpenAI 兼容的 API（比如国内中转站、DeepSeek、本地模型等）来用，不用直接连 Anthropic。

## 它解决什么问题？

很多工具（Claude Code、Cursor 等）只支持 Anthropic 的 API 格式。但你可能：

- 没有 Anthropic 官方 API key
- 想用国内的中转 API 省钱
- 想用 DeepSeek 等其他模型
- 想用本地部署的模型（通过 Ollama、vLLM 等）

a2o 就是一个中间翻译层：**客户端以为在跟 Anthropic 对话，实际上 a2o 把请求翻译成 OpenAI 格式，发给你指定的后端，再把回复翻译回来。**

```
你的工具 (Claude Code 等)
    ↓ 发送 Anthropic 格式请求
   a2o（自动翻译）
    ↓ 转成 OpenAI 格式发出去
你的 API 后端（中转站/DeepSeek/本地模型等）
    ↓ 返回 OpenAI 格式回复
   a2o（自动翻译）
    ↓ 转成 Anthropic 格式返回
你的工具（完全感知不到区别）
```

## 快速开始

### 第一步：下载

去 [Releases](https://github.com/fjlmcm/a2o/releases) 下载对应系统的文件：

| 系统 | 文件 |
|------|------|
| Windows | `a2o-windows-amd64.exe` |
| Mac (Intel) | `a2o-mac-amd64` |
| Mac (Apple Silicon M1/M2/M3/M4) | `a2o-mac-arm64` |
| Linux | `a2o-linux-amd64` |

Mac/Linux 下载后需要加执行权限：

```bash
chmod +x a2o-mac-arm64
```

### 第二步：写配置文件

在 a2o 同目录下创建一个 `config.json` 文件。下面是最简配置：

```json
{
  "auth_token": "my-secret-key",
  "services": [
    {
      "comment": "我的API",
      "listen_address": "11001",
      "openai_base_url": "https://你的API地址/v1/chat/completions",
      "openai_api_key": "sk-你的API密钥"
    }
  ]
}
```

**每个字段什么意思：**

| 字段 | 意思 | 必填 |
|------|------|------|
| `auth_token` | 你自己设的密码，客户端连 a2o 时要用。留空表示不需要密码 | 否 |
| `comment` | 给自己看的备注，随便写 | 否 |
| `listen_address` | a2o 监听的端口号，客户端要连这个端口 | 是 |
| `openai_base_url` | 上游 API 的完整地址，必须到 `/v1/chat/completions` | 是 |
| `openai_api_key` | 上游 API 的密钥 | 是 |

### 第三步：启动

```bash
# Windows
a2o.exe

# Mac / Linux
./a2o-mac-arm64
```

看到类似这样的输出就是启动成功了：

```
A2O Proxy Config Loaded. DebugLevel: info
Starting Service #1 on 11001 (我的API)
```

### 第四步：配置你的工具

以 **Claude Code** 为例，设置环境变量：

```bash
# API 地址指向 a2o
export ANTHROPIC_BASE_URL=http://127.0.0.1:11001

# 密钥填你在 config.json 里设的 auth_token
export ANTHROPIC_API_KEY=my-secret-key
```

然后正常启动 Claude Code 就行了。

对于 **Cursor** 等其他工具，在设置里把 Anthropic API 的地址改成 `http://127.0.0.1:11001`，密钥填 `auth_token` 的值。

## 实际使用示例

### 示例 1：用国内中转站

```json
{
  "auth_token": "abc123",
  "services": [
    {
      "comment": "某中转站",
      "listen_address": "11001",
      "openai_base_url": "https://api.zhongzhuan.com/v1/chat/completions",
      "openai_api_key": "sk-xxxxxxxxx"
    }
  ]
}
```

### 示例 2：用 DeepSeek

DeepSeek 的 API 是 OpenAI 兼容格式，直接用。a2o 还会自动把 DeepSeek 的"思考过程"转换成 Anthropic 的 thinking 格式。

```json
{
  "auth_token": "abc123",
  "services": [
    {
      "comment": "DeepSeek",
      "listen_address": "11001",
      "openai_base_url": "https://api.deepseek.com/v1/chat/completions",
      "openai_api_key": "sk-你的deepseek密钥",
      "force_model": "deepseek-chat"
    }
  ]
}
```

> `force_model` 的作用：不管客户端请求哪个模型（比如 claude-sonnet-4-20250514），都强制用你指定的模型。

### 示例 3：多个 API 同时用 + 负载均衡

你有多个 API key，想轮流用来分散用量：

```json
{
  "auth_token": "abc123",
  "round_robin_address": "11000",
  "services": [
    {
      "comment": "API Key 1",
      "listen_address": "11001",
      "openai_base_url": "https://api.example.com/v1/chat/completions",
      "openai_api_key": "sk-key1"
    },
    {
      "comment": "API Key 2",
      "listen_address": "11002",
      "openai_base_url": "https://api.example.com/v1/chat/completions",
      "openai_api_key": "sk-key2"
    }
  ]
}
```

这样配置后：
- 连 `11001` → 固定走 Key 1
- 连 `11002` → 固定走 Key 2
- 连 `11000` → 自动轮流（第一次走 Key 1，第二次走 Key 2，第三次走 Key 1...）

### 示例 4：需要走代理访问

如果你的 API 需要通过代理才能访问：

```json
{
  "services": [
    {
      "comment": "需要代理的API",
      "listen_address": "11001",
      "openai_base_url": "https://api.example.com/v1/chat/completions",
      "openai_api_key": "sk-xxx",
      "upstream_proxy": "socks5://127.0.0.1:7890"
    }
  ]
}
```

支持 `socks5://` 和 `http://` 两种代理协议。

## 完整配置参考

```json
{
  "debug_level": "info",
  "timeout_seconds": 300,
  "auth_token": "",
  "round_robin_address": "",
  "services": [
    {
      "comment": "",
      "listen_address": "11001",
      "openai_base_url": "https://api.example.com/v1/chat/completions",
      "openai_api_key": "sk-xxx",
      "upstream_proxy": "",
      "force_model": ""
    }
  ]
}
```

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `debug_level` | 日志级别。`info` 只打印关键信息，`debug` 打印详细调试日志 | `info` |
| `timeout_seconds` | 请求超时时间（秒）。模型回复太慢会断开 | `300` |
| `auth_token` | 客户端连接 a2o 时需要提供的密钥。留空不验证 | 空 |
| `round_robin_address` | 负载均衡端口。留空不启用 | 空 |
| `force_model` | 强制替换模型名。留空则使用客户端请求的模型名 | 空 |
| `upstream_proxy` | 访问上游 API 时使用的代理。留空不走代理 | 空 |

## 用量统计

a2o 运行时会自动把每次请求的 token 用量记录到同目录下的 `usage_stats.csv` 文件，按日期、服务、模型分组统计。可以用 Excel 打开查看。

## 从源码编译

需要 Go 1.22+：

```bash
go build -o a2o.exe main.go      # Windows
go build -o a2o main.go           # Mac / Linux
```

## 支持的功能

- 普通对话和流式对话
- 工具调用 (function calling / tool use)
- 图片输入（base64 和 URL）
- DeepSeek 思考过程 → Anthropic thinking 块
- 自动重试（网络错误或空流时最多重试 3 次）
- 连接池复用，性能好
- 零外部依赖，单文件

## License

MIT License
