# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

a2o 是一个 Go 语言编写的 API 代理服务，将 Anthropic Claude Messages API 请求转换为 OpenAI Chat Completions API 格式。这使得期望 Anthropic API 的客户端能够与任意 OpenAI 兼容的后端通信。

## Build Commands

```bash
# Windows 构建
go build -o a2o.exe main.go

# Mac 交叉编译（在 Windows PowerShell 中）
.\build_mac.ps1

# 运行
./a2o.exe -config config.json
```

## Architecture

整个项目是单文件架构 (`main.go`)，约 1400 行代码，无外部依赖。

### 核心组件

1. **配置系统** (L24-41, L373-395)
   - `Config`: 全局配置，包含认证、超时、服务列表
   - `ServiceConfig`: 单个服务配置，支持上游代理、强制模型替换

2. **API 转换层** (L141-273, L633-902)
   - `AnthropicRequest/Response`: Anthropic 消息格式结构体
   - `OpenAIRequest/Response`: OpenAI 聊天格式结构体
   - `convertToOpenAI()`: 核心转换函数，处理消息、工具调用、系统提示、图片等

3. **HTTP 处理** (L274-351, L403-623)
   - 每个服务启动独立监听端口
   - 可选的 Round-Robin 负载均衡端口
   - 支持 CORS、认证验证、请求重试

4. **流式响应处理** (L976-1225)
   - `handleStream()`: 将 OpenAI SSE 格式转换为 Anthropic SSE 格式
   - 支持 text、thinking（DeepSeek 推理）、tool_use 三种内容块类型
   - 处理多工具并行调用的流式传输

5. **连接池** (L63-139)
   - HTTP 客户端复用，支持代理配置
   - 针对流式传输优化（禁用压缩和 HTTP/2）

6. **统计聚合** (L1257-1398)
   - 按日期/服务/模型聚合 token 使用量
   - 定期写入 `usage_stats.csv`

### 数据流

```
客户端 (Anthropic API)
    ↓ POST /v1/messages
代理服务 (a2o)
    ↓ convertToOpenAI()
上游服务 (OpenAI 兼容 API)
    ↓ 响应
代理服务
    ↓ handleNormal() / handleStream()
客户端 (Anthropic 格式响应)
```

## Configuration

`config.json` 示例结构：
```json
{
  "debug_level": "info|debug",
  "timeout_seconds": 300,
  "auth_token": "客户端认证密钥",
  "round_robin_address": "11000",
  "services": [{
    "comment": "服务描述",
    "listen_address": "11001",
    "openai_base_url": "https://api.example.com/v1/chat/completions",
    "openai_api_key": "上游API密钥",
    "upstream_proxy": "socks5://host:port",
    "force_model": "强制使用的模型名"
  }]
}
```

## Key Implementation Details

- **流式重试**: 首次请求会 peek 数据确认流有效，无效则自动重试 (L515-568)
- **工具调用映射**: OpenAI 的 tool index 到 Anthropic block index 的映射 (L1007, L1069-1078)
- **Thinking 支持**: 转换 DeepSeek 的 `reasoning_content` 为 Anthropic 的 thinking 块 (L1109-1133)
- **原子写入**: 统计文件使用临时文件+重命名确保一致性 (L1346-1398)
