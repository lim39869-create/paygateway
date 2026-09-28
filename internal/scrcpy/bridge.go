// WebSocket 桥接：把 scrcpy 实时流转发给浏览器，接收浏览器控制指令
//
// 消息协议：
//
//	服务端 → 客户端（二进制）：H.264 Annex-B 帧数据（SPS/PPS/IDR/P，逐帧转发）
//	服务端 → 客户端（文本 JSON）：连接建立后先发一条 meta
//	客户端 → 服务端（文本 JSON）：
//	  {"type":"touch","action":0|1|2,"x":N,"y":N}
//	  {"type":"key","action":0|1,"keycode":N}
//	  {"type":"text","text":"..."}
//	  {"type":"reset"}
//
// 多查看者：每个设备一个 scrcpy 会话 + 广播 hub，视频循环单读多播。
// 会话生命周期独立于单个 WS 连接（用 context.Background()），
// 避免某个浏览器断开时误杀共享会话。
package scrcpy

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"paygateway/internal/logger"
)

// upgrader WebSocket 升级器
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 128 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// StreamServer 实时流服务（会话池 + WebSocket 入口）
type StreamServer struct {
	mu       sync.Mutex
	sessions map[string]*deviceSession // key: adb serial
	nextPort int
}

// deviceSession 单个设备的会话：scrcpy 连接 + 视频广播
type deviceSession struct {
	owner  *StreamServer
	serial string
	conn   *Connection
	// 会话创建时间（首帧延迟 = videoLoop 启动到第一帧）
	startAt time.Time
	// 视频广播
	subMu    sync.Mutex
	subs     map[chan []byte]struct{}
	stopSub  chan struct{}
	stopOnce sync.Once
	// 最近一个 config 帧（SPS/PPS，新订阅者连接时先发它才能解码 IDR）
	lastConfig []byte
	// 分辨率（videoLoop 解析 session 头后更新）
	width  uint16
	height uint16
}

// NewStreamServer 创建流服务
func NewStreamServer() *StreamServer {
	return &StreamServer{
		sessions: make(map[string]*deviceSession),
		nextPort: 27200,
	}
}

func (s *StreamServer) allocPort() int {
	s.nextPort++
	return s.nextPort
}

// getOrCreate 获取或创建设备会话（后台 ctx，独立于 WS 连接）
func (s *StreamServer) getOrCreate(serial string) (*deviceSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ds, ok := s.sessions[serial]; ok {
		return ds, nil
	}
	sess, err := StartSession(context.Background(), serial, s.allocPort())
	if err != nil {
		return nil, err
	}
	ds := &deviceSession{
		owner:   s,
		serial:  serial,
		conn:    sess.Connection(),
		subs:    make(map[chan []byte]struct{}),
		stopSub: make(chan struct{}),
		startAt: time.Now(),
	}
	s.sessions[serial] = ds
	go ds.videoLoop()
	return ds, nil
}

// removeSession 在 videoLoop 退出时被同一会话主动调用来剔除自己
func (s *StreamServer) removeSession(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, serial)
}

func (s *StreamServer) HasSession(serial string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[serial]
	return ok
}

// videoLoop 单读 scrcpy 视频流，广播给所有订阅者
// 设备断开/读错误时 break：主动从 sessions map 删除自己 + 通知订阅者 + 关会话
// 不删除会导致 getOrCreate 拿到死会话（USB 拔了再插复现）
func (ds *deviceSession) videoLoop() {
	defer ds.handleDead()
	firstFrameAt := time.Time{}
	for {
		frame, err := ds.conn.ReadVideo()
		if err != nil {
			break
		}
		if firstFrameAt.IsZero() {
			firstFrameAt = time.Now()
			logger.Info("scrcpy", "首帧到达（会话启动耗时）", "serial", ds.serial, "delay_ms", time.Since(ds.startAt).Milliseconds())
		}
		// 缓存 config 帧（SPS/PPS），供新订阅者连接时补发
		if frame.Config {
			ds.subMu.Lock()
			ds.lastConfig = frame.Data
			ds.subMu.Unlock()
		}
		// 分辨率：ReadVideo 已解析 session 头并更新 conn.Width/Height
		ds.subMu.Lock()
		if ds.conn.Width > 0 {
			ds.width = ds.conn.Width
			ds.height = ds.conn.Height
		}
		// 封装：1字节类型前缀 + 帧数据
		// 0x01=config(SPS/PPS) 0x02=IDR关键帧 0x03=P帧
		// 前端据此标记 EncodedVideoChunk 的 key/delta，是 WebCodecs 解码的前提
		var tag byte
		switch {
		case frame.Config:
			tag = 0x01
		case frame.KeyFrame:
			tag = 0x02
		default:
			tag = 0x03
		}
		// 关键帧拼接 SPS/PPS：scrcpy 编码器关闭了重复参数集，
		// IDR 帧不含参数集，WebCodecs annexb 模式要求关键帧自带，
		// 否则浏览器无法解码（黑屏）。拼接后 = SPS/PPS + IDR 的合法 Annex-B 流。
		data := frame.Data
		if tag == 0x02 && ds.lastConfig != nil {
			merged := make([]byte, 0, len(ds.lastConfig)+len(data))
			merged = append(merged, ds.lastConfig...)
			merged = append(merged, data...)
			data = merged
		}
		payload := make([]byte, 1+len(data))
		payload[0] = tag
		copy(payload[1:], data)
		for ch := range ds.subs {
			select {
			case ch <- payload:
			default: // 慢消费者丢弃（保持实时性）
			}
		}
		ds.subMu.Unlock()
	}
}

// handleDead videoLoop 退出时清理
func (ds *deviceSession) handleDead() {
	ds.owner.removeSession(ds.serial)
	if ds.conn != nil {
		ds.conn.Close()
	}
	ds.closeAll()
}

// subscribe 订阅视频流，返回接收 channel 和取消函数
func (ds *deviceSession) subscribe() (chan []byte, func()) {
	ch := make(chan []byte, 16)
	ds.subMu.Lock()
	ds.subs[ch] = struct{}{}
	ds.subMu.Unlock()
	cancel := func() {
		ds.subMu.Lock()
		delete(ds.subs, ch)
		ds.subMu.Unlock()
	}
	return ch, cancel
}

func (ds *deviceSession) closeAll() {
	ds.stopOnce.Do(func() {
		close(ds.stopSub)
	})
}

// CloseSession 外部主动关会话（设备将被删除/冻结时调用）
func (s *StreamServer) CloseSession(serial string) {
	s.mu.Lock()
	ds, ok := s.sessions[serial]
	if ok {
		delete(s.sessions, serial)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if ds.conn != nil {
		ds.conn.Close()
	}
	ds.closeAll()
}

// HandleStream 处理 WebSocket 连接（video 广播订阅 + control 注入）
// GET /stream?serial=127.0.0.1:5556
func (s *StreamServer) HandleStream(w http.ResponseWriter, r *http.Request) {
	serial := r.URL.Query().Get("serial")
	if serial == "" {
		http.Error(w, "缺少 serial 参数", http.StatusBadRequest)
		return
	}

	ds, err := s.getOrCreate(serial)
	if err != nil {
		http.Error(w, "启动设备会话失败: "+err.Error(), http.StatusBadGateway)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// 先订阅：帧开始累积，等待 meta 就绪期间不丢流
	frames, cancelSub := ds.subscribe()
	defer cancelSub()

	// 等 meta 就绪（新会话需解析 session 头；已有会话秒就绪）
	// 等待期间先发 meta 再补 config，然后正常推流
	deadline := time.Now().Add(3 * time.Second)
	sentMeta := false
	for time.Now().Before(deadline) {
		ds.subMu.Lock()
		ready := ds.width > 0
		cfg := ds.lastConfig
		w := ds.width
		h := ds.height
		ds.subMu.Unlock()
		if ready && !sentMeta {
			sentMeta = true
			meta, _ := json.Marshal(metaMsg{
				Type:   "meta",
				Width:  int(w),
				Height: int(h),
				Device: ds.conn.Device,
			})
			if err := ws.WriteMessage(websocket.TextMessage, meta); err != nil {
				return
			}
			// 补发缓存的 config 帧（SPS/PPS，带类型前缀），否则浏览器无法解码后续 IDR
			if cfg != nil {
				payload := make([]byte, 1+len(cfg))
				payload[0] = 0x01 // config
				copy(payload[1:], cfg)
				if err := ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					return
				}
			}
			// 请求关键帧：通知 scrcpy-server 立即输出 IDR
			_ = ds.conn.ResetVideo()
			// 唤醒屏幕：设备可能已熄屏（MediaCodec 无新帧）。BACK_OR_SCREEN_ON=4 唤醒 + 关锁屏，
			// 保证熄屏后再次打开控制台也能立即恢复画面（stay_awake 只能保会话期间的唤醒）。
			_ = ds.conn.WakeScreen()
		}
		if ready {
			break
		}
		// 避免空转：等待期间也从帧 channel 排空，保持连接活跃
		select {
		case <-ds.stopSub:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}

	// 视频转发协程（帧已在等待期间开始累积，此处直接转发）
	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		defer func() {
			if r := recover(); r != nil {
				logger.Error("scrcpy", "视频转发协程 panic（已恢复）", "serial", ds.serial,
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		for {
			select {
			case <-ds.stopSub:
				return
			case data, ok := <-frames:
				if !ok {
					return
				}
				if err := ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
					return
				}
			}
		}
	}()

	// 控制接收：读 WS JSON → 注入设备
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var msg controlMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "touch":
			_ = ds.conn.InjectTouch(byte(msg.Action), int32(msg.X), int32(msg.Y))
		case "key":
			_ = ds.conn.InjectKeycode(byte(msg.Action), int32(msg.Keycode))
		case "text":
			_ = ds.conn.InjectText(msg.Text)
		case "reset":
			_ = ds.conn.ResetVideo()
		}
	}
	<-wsDone
}

// metaMsg 连接建立后发给浏览器的设备信息
type metaMsg struct {
	Type   string `json:"type"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Device string `json:"device"`
}

// controlMsg 浏览器发来的控制指令
type controlMsg struct {
	Type    string `json:"type"`
	Action  int    `json:"action"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Keycode int    `json:"keycode"`
	Text    string `json:"text"`
}
