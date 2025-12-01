# TCP 桥接器（单对转发）

## 概述
- 采集端 A 与被采集端 B 都只能作为 TCP 客户端，桥接器充当服务端同时接入 A/B，并在桥内完成透明转发。
- 允许丢包、允许“孤儿端”长期保持连接；任一端掉线不会主动关闭另一端，重连后自动恢复转发。

## 默认端口
- A 端口：`1024`
- B 端口：`1025`
如需调整，可修改 `main.go` 中 `portA/portB` 常量后重新构建。

## 行为与特性
- 固定一对一配对，常驻会话，双向字节流转发，不解析业务协议。
- 单点读写：仅 session 内的 readerLoop 读取，避免多 goroutine 抢读吞包；writer 在对端离线时不消费队列。
- 背压保护：每方向有界队列，满则丢弃新数据但不关连接（允许丢包防 OOM）。
- 半开处理：TCP KeepAlive；读空闲超时默认关闭（依赖 keepalive），可按需开启。
- 错误隔离：连接错误只清理对应槽位，另一侧保持；新连接上线后自动恢复。
- 日志：包含 session、方向、断因、丢弃等信息。

## 运行
- 直接运行可执行文件（推荐从 Release 下载）：  
  `.\bridge.exe`
- 或本机已有 Go 环境：  
  `go run .`

## 参数
位于 `main.go`：
- `portA`, `portB`：监听端口（默认 `:1024`、`:1025`）
- `readIdleTimeout`: 默认 `0`（关闭）；如需限制空闲读可设较大值
- `writeTimeout`: 默认 `10s`
- `queueCapacity`: 每方向缓冲队列容量（默认 `1024` 块）
- `maxChunkBytes`: 单次拷贝的最大字节数（默认 `4KB`）
- `tcpKeepAlivePeriod`: 默认 `30s`

## 断线与重连
- 任一端掉线：仅清理该端槽位，另一端连接保持（但可能无响应）。
- 队列中的数据在对端离线时不被消费；对端重连后继续转发新数据。

## 构建
- Windows 本地构建（无额外依赖）：  
  `go build -trimpath -ldflags "-s -w" -o bridge.exe .`
- 其他平台：同上命令，替换输出文件名即可。

## 发布（GitHub Actions）
- 推送 `v*` tag 自动触发 Release，产出 Windows 64 位 `bridge.exe`。工作流位于 `.github/workflows/release.yml`。

## 后续演进
- 多对并发 / 按 ID 配对
- metrics/healthz，配置热更新
- 可选应用层心跳增强
