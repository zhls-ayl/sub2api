package handler

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const leonardoFakeStreamKey = "leonardo_images_fake_stream"

// leonardoFakeStream 为 stream=true 的 Leonardo 出图请求提供「假流式」：Leonardo
// 上游不流式，等待期间按间隔写 SSE 注释保活，出图完成后一次性写出 OpenAI
// Images 流式协议的 completed 事件。
//
// 响应头延迟到第一次保活（或最终写出）时才提交：之前出现的错误（参数校验、
// 无可用账号等）仍能以原状态码返回 JSON；提交之后的错误只能写 SSE error 事件
// （状态码已是 200，与 OpenAI 出图链路的取舍一致）。所有写入都在 mu 下进行，
// 保活 goroutine 与请求 goroutine 不会交错写 c.Writer。
type leonardoFakeStream struct {
	c *gin.Context

	mu      sync.Mutex
	started bool // 响应头已提交（SSE）
	paused  bool // 转交中转号期间暂停保活
	stopped bool // 保活已结束（写出终态或请求结束）
	dead    bool // 写客户端失败（已断开）

	done     chan struct{}
	doneOnce sync.Once
}

// startLeonardoFakeStream 创建假流式会话并挂到 gin context；interval ≤ 0 时不保活
// （仍可在最终写出时提交 SSE 响应）。
func startLeonardoFakeStream(c *gin.Context, interval time.Duration) *leonardoFakeStream {
	s := &leonardoFakeStream{c: c, done: make(chan struct{})}
	c.Set(leonardoFakeStreamKey, s)
	if interval > 0 {
		go s.keepalive(interval)
	}
	return s
}

func leonardoFakeStreamFromContext(c *gin.Context) *leonardoFakeStream {
	if c == nil {
		return nil
	}
	value, ok := c.Get(leonardoFakeStreamKey)
	if !ok {
		return nil
	}
	s, _ := value.(*leonardoFakeStream)
	return s
}

func (s *leonardoFakeStream) keepalive(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var reqDone <-chan struct{}
	if s.c.Request != nil {
		reqDone = s.c.Request.Context().Done()
	}
	for {
		select {
		case <-s.done:
			return
		case <-reqDone:
			return
		case <-ticker.C:
			s.beat()
		}
	}
}

func (s *leonardoFakeStream) beat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.paused || s.dead {
		return
	}
	s.commitLocked()
	if _, err := io.WriteString(s.c.Writer, ":\n\n"); err != nil {
		s.dead = true
		return
	}
	s.c.Writer.Flush()
}

// commitLocked 提交 SSE 响应头（只做一次）。
func (s *leonardoFakeStream) commitLocked() {
	if s.started {
		return
	}
	header := s.c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	s.c.Writer.WriteHeader(http.StatusOK)
	s.started = true
	service.MarkResponseCommitted(s.c)
}

// finishLocked 结束保活（幂等）。
func (s *leonardoFakeStream) finishLocked() {
	s.stopped = true
	s.doneOnce.Do(func() { close(s.done) })
}

// committed 报告 SSE 响应头是否已提交。
func (s *leonardoFakeStream) committed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// pause / resume 在转交中转号转发（真流式，自写响应头）前后暂停与恢复保活。
func (s *leonardoFakeStream) pause() {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
}

func (s *leonardoFakeStream) resume() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
}

// stop 结束保活（请求结束时 defer 调用）。
func (s *leonardoFakeStream) stop() {
	s.mu.Lock()
	s.finishLocked()
	s.mu.Unlock()
}

// writeEvents 结束保活并写出事件（必要时先提交 SSE 响应头）。
func (s *leonardoFakeStream) writeEvents(events []service.ImagesStreamEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked()
	if s.dead {
		return
	}
	s.commitLocked()
	for _, event := range events {
		if err := writeLeonardoSSEEvent(s.c.Writer, event.Name, event.Payload); err != nil {
			s.dead = true
			return
		}
	}
	s.c.Writer.Flush()
}

// failIfCommitted 结束保活；SSE 已提交时把错误写成 error 事件并返回 true，
// 否则返回 false（调用方按普通 JSON 错误响应）。检查与结束在同一把锁内，
// 不会与保活 goroutine 的提交交错。
func (s *leonardoFakeStream) failIfCommitted(errType, code, message string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked()
	if !s.started {
		return false
	}
	if !s.dead {
		if err := writeLeonardoSSEEvent(s.c.Writer, "error", service.ImagesStreamErrorPayload(errType, code, message)); err != nil {
			s.dead = true
		} else {
			s.c.Writer.Flush()
		}
	}
	return true
}

func writeLeonardoSSEEvent(w io.Writer, name string, payload []byte) error {
	if name != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", name); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", payload)
	return err
}
