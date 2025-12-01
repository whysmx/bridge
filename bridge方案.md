下面是按你确认的前提（**丢包可接受、孤儿端可长期保持**）整理的**技术规划 + Go 第一版代码**，已经排版成 Markdown，你可以直接保存成 `bridge方案.md`。

---

# TCP 桥接器技术方案（Go 第一版）

## 0. 背景与目标

**场景**  
- 采集程序 A：只能做 TCP 客户端  
- 被采集模块 B：也只能做 TCP 客户端  
- 因此需要一个 **TCP 桥接器**做服务端，接入 A 与 B，并在桥内完成双向转发。

**目标语义**  
1. 桥监听两个端口：`portA`、`portB`（默认 `:1024`、`:1025`）  
2. 任何一端掉线时：
   - **桥不主动关闭另一端连接**
   - 孤儿端保持 TCP 连接，只表现为“无应答/无数据返回”
3. 当另一端重新连上时自动恢复转发  
4. 第一版功能保持简单，重点是稳定、可长期运行。

---

## 1. 第一版范围（刻意收敛）

### 1.1 做这些
- **固定一对一配对**
  - A 连上占用 A 槽位
  - B 连上占用 B 槽位
  - 两端都在线时自动配对并转发
- **透明全双工转发**
  - 桥不解析业务协议，只做字节流转发
- **稳定性基础**
  - TCP KeepAlive；可选读空闲超时（默认关闭，支持长期孤儿）；写超时
  - 有界缓冲队列（背压保护）
  - 队列满时**丢弃新数据**（允许丢包但不关连接）
  - goroutine panic recover
  - 结构化日志（session、方向、断因、drop 次数）

### 1.2 暂不做
- 多对并发 / ID 配对
- 协议级心跳增强
- metrics/管理接口/热更新

---

## 2. 架构与状态机

### 2.1 架构组件
- **ListenerA/ListenerB**
  - 分别监听 `portA` 与 `portB`
- **Bridge（配对器 + 单会话）**
  - 每端一个“活连接槽位”：`a`、`b`
  - 槽位可长期保持
- **Session（常驻转发引擎）**
  - 两个方向缓冲：`qAB`、`qBA`
  - 4 个 goroutine：
    - reader A → qAB
    - writer qAB → B
    - reader B → qBA
    - writer qBA → A

### 2.2 状态机（第一版）
- `ONLY_A`：只有 A 在线  
- `ONLY_B`：只有 B 在线  
- `PAIRED`：A/B 都在线并转发  

**转移规则**
1. A 连上：进入 `ONLY_A` 或 `PAIRED`  
2. B 连上：进入 `ONLY_B` 或 `PAIRED`
3. 任一端断开：
   - 只清理该端槽位 → 回到 `ONLY_另一端`
4. 新端上线：
   - 自动恢复到 `PAIRED`

---

## 3. 稳定性设计要点

### 3.1 超时与半开连接
- `TCP KeepAlive`
  - 系统级清理半开连接
- `SetReadDeadline(readIdleTimeout)`
  - 默认关闭（0），依赖 keepalive；如需限制空闲读可设置较大超时
- `SetWriteDeadline(writeTimeout)`
  - 避免写阻塞卡死

### 3.2 背压与缓冲（允许丢包）
- 每方向一个有界缓冲 `chan []byte`
- 下游不在/不读时：
  - writer 不消费队列
  - 队列满则 reader **丢弃新数据**
- 目的：防止 OOM，同时不主动断开上游连接

### 3.3 错误隔离与自愈
- Session 常驻；连接断开只清槽位
- 新连接上线自动恢复转发
- goroutine 内 `recover()` 防止 panic 崩进程
- 读/写错误由 readerLoop 捕获并清槽位，不再额外起探活 goroutine，避免抢读

### 3.4 单点读写，避免抢读
- 同一连接只有 session 内的 readerLoop 读取，避免与其他 goroutine 抢读导致业务数据被吞
- writer 在对端离线时不消费队列，待对端恢复后继续 flush

### 3.5 日志建议
必须包含：
- session id
- 连接角色（A/B）
- 数据方向（A->B / B->A）
- 断开原因（EOF/timeout/reset/overflow）
- drop 发生次数

---

## 4. 参数建议（第一版默认值）
- `readIdleTimeout`: 0（关闭空闲读超时，依赖 keepalive；如需上限请设置一个较大值）  
- `writeTimeout`: 10s  
- `queueCapacity`: 1024 chunks  
- `maxChunkBytes`: 4KB  
- `tcpKeepAlivePeriod`: 30s  

可按吞吐调整：
- `queueCapacity * maxChunkBytes` ≈ 每方向最大缓存字节数

---

## 5. Go 第一版实现代码（单文件可运行）

```go
package main

import (
    "context"
    "io"
    "log"
    "net"
    "os"
    "os/signal"
    "sync"
    "sync/atomic"
    "syscall"
    "time"
)

const (
    portA = ":1024"
    portB = ":1025"

    // 0 代表不设读空闲超时，仅依赖 TCP keepalive 检测半开
    readIdleTimeout = 0
    writeTimeout    = 10 * time.Second

    queueCapacity      = 1024
    maxChunkBytes      = 4 * 1024
    tcpKeepAlivePeriod = 30 * time.Second
)

var sessionSeq uint64

func setKeepAlive(c net.Conn) {
    if tc, ok := c.(*net.TCPConn); ok {
        _ = tc.SetKeepAlive(true)
        _ = tc.SetKeepAlivePeriod(tcpKeepAlivePeriod)
    }
}

// single-pair bridge, keep orphan alive
type bridge struct {
    mu sync.Mutex

    a net.Conn
    b net.Conn

    sess *session
}

func newBridge() *bridge {
    br := &bridge{}
    br.sess = newSession(br.clearA, br.clearB)
    return br
}

func (br *bridge) putA(c net.Conn) {
    br.mu.Lock()
    defer br.mu.Unlock()

    if br.a != nil {
        _ = br.a.Close()
    }
    br.a = c
    br.sess.setA(c)
    log.Printf("[bridge] A online: %s", c.RemoteAddr())
}

func (br *bridge) putB(c net.Conn) {
    br.mu.Lock()
    defer br.mu.Unlock()

    if br.b != nil {
        _ = br.b.Close()
    }
    br.b = c
    br.sess.setB(c)
    log.Printf("[bridge] B online: %s", c.RemoteAddr())
}

func (br *bridge) clearA(c net.Conn) {
    br.mu.Lock()
    defer br.mu.Unlock()
    if br.a == c {
        br.a = nil
        br.sess.setA(nil)
        log.Printf("[bridge] A offline")
    }
}

func (br *bridge) clearB(c net.Conn) {
    br.mu.Lock()
    defer br.mu.Unlock()
    if br.b == c {
        br.b = nil
        br.sess.setB(nil)
        log.Printf("[bridge] B offline")
    }
}

type session struct {
    id uint64

    mu sync.RWMutex
    a  net.Conn
    b  net.Conn

    qAB chan []byte
    qBA chan []byte

    ctx    context.Context
    cancel context.CancelFunc

    onAClose func(net.Conn)
    onBClose func(net.Conn)
}

func newSession(onAClose, onBClose func(net.Conn)) *session {
    ctx, cancel := context.WithCancel(context.Background())
    s := &session{
        id:       atomic.AddUint64(&sessionSeq, 1),
        qAB:      make(chan []byte, queueCapacity),
        qBA:      make(chan []byte, queueCapacity),
        ctx:      ctx,
        cancel:   cancel,
        onAClose: onAClose,
        onBClose: onBClose,
    }

    go s.readerLoop("A", s.getA, s.onAClose, s.qAB)
    go s.writerLoop("B", s.getB, s.qAB)

    go s.readerLoop("B", s.getB, s.onBClose, s.qBA)
    go s.writerLoop("A", s.getA, s.qBA)

    return s
}

func (s *session) setA(c net.Conn) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.a = c
}

func (s *session) setB(c net.Conn) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.b = c
}

func (s *session) getA() net.Conn {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.a
}

func (s *session) getB() net.Conn {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.b
}

// readerLoop 只由 session 持有，避免与其他 goroutine 抢读。
// queue 满则丢弃新数据，不断开连接。
func (s *session) readerLoop(role string, getConn func() net.Conn, onClose func(net.Conn), q chan<- []byte) {
    buf := make([]byte, maxChunkBytes)

    for {
        select {
        case <-s.ctx.Done():
            return
        default:
        }

        c := getConn()
        if c == nil {
            time.Sleep(100 * time.Millisecond)
            continue
        }

        if readIdleTimeout > 0 {
            _ = c.SetReadDeadline(time.Now().Add(readIdleTimeout))
        }

        n, err := c.Read(buf)
        if n > 0 {
            cp := make([]byte, n)
            copy(cp, buf[:n])

            select {
            case q <- cp:
            default:
                log.Printf("[sess %d][%s->*] queue full, drop %d bytes", s.id, role, n)
            }
        }
        if err != nil {
            if ne, ok := err.(net.Error); ok && ne.Timeout() {
                log.Printf("[sess %d][%s] read timeout", s.id, role)
            } else if err == io.EOF {
                log.Printf("[sess %d][%s] peer closed EOF", s.id, role)
            } else {
                log.Printf("[sess %d][%s] read err: %v", s.id, role, err)
            }
            _ = c.Close()
            onClose(c)
            time.Sleep(100 * time.Millisecond)
        }
    }
}

// writerLoop 在 dst 不在线时不取队列，避免消费并丢失未发送的数据。
func (s *session) writerLoop(role string, getConn func() net.Conn, q <-chan []byte) {
    for {
        select {
        case <-s.ctx.Done():
            return
        default:
        }

        dst := getConn()
        if dst == nil {
            time.Sleep(100 * time.Millisecond)
            continue
        }

        select {
        case data := <-q:
            _ = dst.SetWriteDeadline(time.Now().Add(writeTimeout))
            _, err := dst.Write(data)
            if err != nil {
                if ne, ok := err.(net.Error); ok && ne.Timeout() {
                    log.Printf("[sess %d][*->%s] write timeout", s.id, role)
                } else {
                    log.Printf("[sess %d][*->%s] write err: %v", s.id, role, err)
                }
                _ = dst.Close()
                time.Sleep(100 * time.Millisecond)
            }
        case <-time.After(200 * time.Millisecond):
        }
    }
}

func acceptLoop(l net.Listener, name string, onConn func(net.Conn)) {
    for {
        c, err := l.Accept()
        if err != nil {
            if ne, ok := err.(net.Error); ok && ne.Temporary() {
                log.Printf("[%s] accept temp err: %v", name, err)
                time.Sleep(200 * time.Millisecond)
                continue
            }
            log.Printf("[%s] accept err: %v", name, err)
            return
        }
        setKeepAlive(c)
        log.Printf("[%s] connected: %s", name, c.RemoteAddr())
        onConn(c)
    }
}

func main() {
    log.SetFlags(log.LstdFlags | log.Lmicroseconds)

    la, err := net.Listen("tcp", portA)
    if err != nil {
        log.Fatal(err)
    }
    lb, err := net.Listen("tcp", portB)
    if err != nil {
        log.Fatal(err)
    }
    defer la.Close()
    defer lb.Close()

    br := newBridge()

    go acceptLoop(la, "listenerA", br.putA)
    go acceptLoop(lb, "listenerB", br.putB)

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
    <-sig
    log.Println("shutting down...")
    br.sess.cancel()
}
```

---

## 6. 验收测试清单（上线前）
1. **基本互通**
   - A 与 B 同时连上，双向通讯正常
2. **单端掉线**
   - A 掉线，B 连接仍保持
   - B 掉线，A 连接仍保持
3. **断线恢复**
   - 任一端重连后通讯自动恢复
4. **背压丢包**
   - B 不读或不在线时 A 高速发送
   - 观察 drop 日志出现但内存稳定
5. **长稳测试**
   - 24h soak：goroutine、内存、fd 无增长

---

## 7. 后续演进（不破坏当前结构）
1. **多对并发 / 按 ID 配对**
2. **metrics + healthz HTTP**
3. **热更新配置**
4. **可选应用层心跳增强**
