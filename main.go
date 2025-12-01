package main

import (
	"context"
	"encoding/hex"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
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

	historyRetention    = 10 * time.Minute
	historyMaxEntries   = 2000
	payloadSampleLimit  = 256
	historyCleanupEvery = 1 * time.Minute
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

	history *trafficHistory
}

func newSession(onAClose, onBClose func(net.Conn)) *session {
	ctx, cancel := context.WithCancel(context.Background())
	hist := &trafficHistory{}
	hist.startCleanup(ctx)
	s := &session{
		id:       atomic.AddUint64(&sessionSeq, 1),
		qAB:      make(chan []byte, queueCapacity),
		qBA:      make(chan []byte, queueCapacity),
		ctx:      ctx,
		cancel:   cancel,
		onAClose: onAClose,
		onBClose: onBClose,
		history:  hist,
	}

	go s.readerLoop("A", "A->B", s.getA, s.onAClose, s.qAB)
	go s.writerLoop("B", s.getB, s.qAB)

	go s.readerLoop("B", "B->A", s.getB, s.onBClose, s.qBA)
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
func (s *session) readerLoop(role, dir string, getConn func() net.Conn, onClose func(net.Conn), q chan<- []byte) {
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
			s.history.record(dir, cp)

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

type trafficEntry struct {
	ts     time.Time
	dir    string
	size   int
	sample string
}

type trafficHistory struct {
	mu      sync.Mutex
	entries []trafficEntry
}

func (h *trafficHistory) record(dir string, data []byte) {
	sample := formatSample(data)
	entry := trafficEntry{
		ts:     time.Now(),
		dir:    dir,
		size:   len(data),
		sample: sample,
	}

	h.mu.Lock()
	h.entries = append(h.entries, entry)
	if len(h.entries) > historyMaxEntries {
		h.entries = h.entries[len(h.entries)-historyMaxEntries:]
	}
	h.mu.Unlock()

	log.Printf("[traffic][%s] %d bytes: %s", dir, len(data), sample)
}

func (h *trafficHistory) startCleanup(ctx context.Context) {
	ticker := time.NewTicker(historyCleanupEvery)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.cleanup()
			}
		}
	}()
}

func (h *trafficHistory) cleanup() {
	cutoff := time.Now().Add(-historyRetention)
	h.mu.Lock()
	defer h.mu.Unlock()

	idx := 0
	for idx < len(h.entries) && h.entries[idx].ts.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		h.entries = append([]trafficEntry{}, h.entries[idx:]...)
	}
	if len(h.entries) > historyMaxEntries {
		h.entries = h.entries[len(h.entries)-historyMaxEntries:]
	}
}

func formatSample(data []byte) string {
	n := len(data)
	if n > payloadSampleLimit {
		n = payloadSampleLimit
	}
	sample := data[:n]

	printable := true
	for _, b := range sample {
		if b < 32 || b > 126 {
			printable = false
			break
		}
	}
	if printable {
		return string(sample)
	}
	return strings.ToUpper(hex.EncodeToString(sample))
}
