// Package scrcpy 纯 Go 实现的 scrcpy 客户端（协议 v4.1）
//
// 用途：为管理后台提供"云手机式"实时安卓设备操作——
// 通过 adb 启动设备上的 scrcpy-server，接收实时 H.264 视频流，
// 并向前端 WebSocket 转发（视频帧 + 设备 meta），同时把前端触摸/按键消息
// 注入到设备（INJECT_TOUCH_EVENT / INJECT_KEYCODE / INJECT_TEXT）。
//
// 监听端载体为 USB 真机 / adb over WiFi（详见 docs/MIANQIAN.md 决策更新 v0.2）。
// scrcpy 协议本身基于 adb forward，对 USB 直连真机 / adb over WiFi 均适用。
//
// 协议参考（scrcpy v4.1 doc/develop.md + 源码）：
//   - 视频 socket: dummy(1) → deviceName(64) → codecId(u32) → [session(12) | frame(12+data)]*
//   - 控制 socket: dummy(1) → 客户端发控制消息（服务端不返回数据）
//   - 帧头 12B: u64(pts|flags) + u32(size)；flags: SESSION=1<<63, CONFIG=1<<62, KEYFRAME=1<<61
//   - session 头: u32(0x80000000|resizeFlag) + u32(width) + u32(height)
package scrcpy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// 帧头 flags（高位）
const (
	flagSession  uint64 = 1 << 63
	flagConfig   uint64 = 1 << 62
	flagKeyFrame uint64 = 1 << 61
)

// 控制消息类型（scrcpy v4.1 ControlMessage.java）
const (
	msgInjectKeycode   = 0
	msgInjectText      = 1
	msgInjectTouch     = 2
	msgInjectScroll    = 3
	msgBackOrScreenOn  = 4
	msgGetClipboard    = 8
	msgSetClipboard    = 9
	msgSetDisplayPower = 10
	msgRotateDevice    = 11
	msgStartApp        = 16
	msgResetVideo      = 17
)

// VideoFrame 一帧视频数据（带 meta）
type VideoFrame struct {
	PTS      int64  // 微秒
	Config   bool   // CODEC_CONFIG 包（SPS/PPS）
	KeyFrame bool   // IDR 关键帧
	Data     []byte // Annex-B H.264 数据（可能含多个 NAL）
}

// SessionMeta 分辨率/会话信息（来自 session 头）
type SessionMeta struct {
	Width  uint16
	Height uint16
}

// Connection 设备连接（video + control 双 socket）
type Connection struct {
	video   net.Conn
	control net.Conn
	Device  string
	Width   uint16
	Height  uint16
}

// Dial 连接 scrcpy-server（通过已建立的 adb forward tcp 端口）
//
// 协议时序（v4.1）：
//
//	video 连接 → 读 dummy(1) → control 连接（dummy 只在第一个 socket 写一次）
//	→ 服务端所有 socket 连齐后：video 上发 name(64) + codecId(u32) + [session|帧]*
func Dial(addr string) (*Connection, error) {
	video, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("video socket: %w", err)
	}
	c := &Connection{video: video}

	// 握手读取全部带超时（server 中途崩溃时避免永久阻塞）
	hs := func(n int) ([]byte, error) {
		b := make([]byte, n)
		video.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(video, b); err != nil {
			return nil, err
		}
		video.SetReadDeadline(time.Time{})
		return b, nil
	}

	// dummy byte（forward 模式探测，仅在 video socket 上发送一次）
	if _, err := hs(1); err != nil {
		video.Close()
		return nil, fmt.Errorf("dummy byte: %w", err)
	}

	// control socket（服务端在 video+control 全部连齐后才发 name/codec）
	control, err := net.Dial("tcp", addr)
	if err != nil {
		video.Close()
		return nil, fmt.Errorf("control socket: %w", err)
	}
	c.control = control

	// device name (64) —— 现在服务端已开始发送
	name, err := hs(64)
	if err != nil {
		video.Close()
		control.Close()
		return nil, fmt.Errorf("device name: %w", err)
	}
	c.Device = trimNull(name)

	// codec id (u32)
	codec, err := hs(4)
	if err != nil {
		video.Close()
		control.Close()
		return nil, fmt.Errorf("codec id: %w", err)
	}
	if got := binary.BigEndian.Uint32(codec); got != 0x68323634 { // "h264"
		video.Close()
		control.Close()
		return nil, fmt.Errorf("非 H.264 codec: 0x%08x", got)
	}

	return c, nil
}

// ReadVideo 读取下一帧（解析 12 字节帧头 + 数据）
// 返回 nil 表示连接正常结束。
func (c *Connection) ReadVideo() (*VideoFrame, error) {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(c.video, hdr); err != nil {
		return nil, err
	}
	ptsFlags := binary.BigEndian.Uint64(hdr[0:8])
	size := binary.BigEndian.Uint32(hdr[8:12])

	if ptsFlags&flagSession != 0 {
		// session 头（12 字节）：flags(u32) + width(u32) + height(u32)
		// 这里 hdr 已含前 12 字节，其中 u32 部分在 hdr[0:4]... 需要重读
		// 实际 session 头是: u32(flags) + u32(w) + u32(h)，与帧头格式不同，
		// 但都在同一个 12 字节缓冲里。hdr[4:8]=width, hdr[8:12]=height
		// 注意：session 的 size 字段无意义，用 flags 判断。
		c.Width = uint16(binary.BigEndian.Uint32(hdr[4:8]))
		c.Height = uint16(binary.BigEndian.Uint32(hdr[8:12]))
		return c.ReadVideo() // session 头不是媒体帧，继续读
	}

	if size > 64*1024*1024 {
		return nil, fmt.Errorf("帧大小异常: %d", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(c.video, data); err != nil {
		return nil, err
	}
	return &VideoFrame{
		PTS:      int64(ptsFlags &^ (flagConfig | flagKeyFrame)),
		Config:   ptsFlags&flagConfig != 0,
		KeyFrame: ptsFlags&flagKeyFrame != 0,
		Data:     data,
	}, nil
}

// ReadSessionMeta 读取首个 session 头（分辨率），返回 meta
// 带 5s 超时：video socket 需要后续持续读帧，避免永久阻塞
func (c *Connection) ReadSessionMeta() (*SessionMeta, error) {
	c.video.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.video.SetReadDeadline(time.Time{})
	// 读帧头，若是 session 返回
	for {
		hdr := make([]byte, 12)
		if _, err := io.ReadFull(c.video, hdr); err != nil {
			return nil, err
		}
		ptsFlags := binary.BigEndian.Uint64(hdr[0:8])
		if ptsFlags&flagSession != 0 {
			w := uint16(binary.BigEndian.Uint32(hdr[4:8]))
			h := uint16(binary.BigEndian.Uint32(hdr[8:12]))
			c.Width, c.Height = w, h
			return &SessionMeta{Width: w, Height: h}, nil
		}
		// 非 session（config 帧），跳过数据
		size := binary.BigEndian.Uint32(hdr[8:12])
		if _, err := io.CopyN(io.Discard, c.video, int64(size)); err != nil {
			return nil, err
		}
	}
}

// SendControl 发送一条控制消息
func (c *Connection) SendControl(msg []byte) error {
	_, err := c.control.Write(msg)
	return err
}

// InjectTouch 注入触摸事件
// action: 0=DOWN 1=UP 2=MOVE；x/y 为设备坐标（相对 Width/Height）
func (c *Connection) InjectTouch(action byte, x, y int32) error {
	msg := make([]byte, 0, 31)
	msg = append(msg, msgInjectTouch, action)
	msg = appendU64(msg, 1) // pointerId
	msg = appendI32(msg, x)
	msg = appendI32(msg, y)
	msg = appendU16(msg, c.Width)
	msg = appendU16(msg, c.Height)
	msg = appendU16(msg, 0xFFFF) // pressure
	msg = appendI32(msg, 1)      // actionButton
	if action == 1 {             // UP: buttons=0
		msg = appendI32(msg, 0)
	} else {
		msg = appendI32(msg, 1)
	}
	return c.SendControl(msg)
}

// InjectKeycode 注入按键
// action: 0=DOWN 1=UP 2=REPEAT；keycode: Android KeyEvent 码
func (c *Connection) InjectKeycode(action byte, keycode int32) error {
	msg := []byte{msgInjectKeycode, action}
	msg = appendI32(msg, keycode)
	msg = appendI32(msg, 0) // repeat
	msg = appendI32(msg, 0) // metaState
	return c.SendControl(msg)
}

// InjectText 注入文本（UTF-8，u32 长度前缀）
func (c *Connection) InjectText(text string) error {
	data := []byte(text)
	msg := []byte{msgInjectText}
	msg = appendU32(msg, uint32(len(data)))
	msg = append(msg, data...)
	return c.SendControl(msg)
}

// ResetVideo 请求关键帧（新客户端加入时）
func (c *Connection) ResetVideo() error {
	return c.SendControl([]byte{msgResetVideo})
}

// WakeScreen 唤醒屏幕（BACK_OR_SCREEN_ON：屏幕熄灭时点亮，锁屏时模拟返回键解锁）。
// 设备熄屏后 MediaCodec 停止产帧，唤醒是恢复视频流的前提。
func (c *Connection) WakeScreen() error {
	// msgBackOrScreenOn=4：action(1B)=0(SCREEN_ON)
	return c.SendControl([]byte{msgBackOrScreenOn, 0})
}

// Close 关闭连接
func (c *Connection) Close() {
	if c.control != nil {
		c.control.Close()
	}
	if c.video != nil {
		c.video.Close()
	}
}

// ---------- 字节序辅助 ----------

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}
func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
func appendI32(b []byte, v int32) []byte {
	return appendU32(b, uint32(v))
}
func appendU64(b []byte, v uint64) []byte {
	return append(b, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func trimNull(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
