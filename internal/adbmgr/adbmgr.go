// Package adbmgr 管理 adb 触达的安卓设备（USB 直连真机 / adb over WiFi）
//
// 定位：web 前端可视化操作 adb 触达的安卓设备（docs/MIANQIAN.md §7.0 增强）。
// 不做完整云手机（无实时屏幕流/远程点击控制），只提供"按需渲染 app 界面"所需的操作：
//   - Screenshot: 截图当前屏幕（按需渲染）
//   - Tap/Swipe/InputText: 轻量操作（完成登录等）
//   - StartApp/ForegroundApp: 启动/查看前台 app
//   - Online: 设备在线检测
//
// 实现：直接调用系统 adb 二进制（-s <serial> 定位设备）。
// USB 真机：serial 为纯设备号（如 "4f2bdc30"），无需 adb connect，直接 get-state 验证。
// adb over WiFi：serial 为 host:port，需 adb connect 后再操作。
package adbmgr

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Device 一台 adb 触达的安卓设备（USB 真机 / adb over WiFi）
type Device struct {
	Serial string // adb serial：USB 直连设备号 "4f2bdc30" 或 TCP "host:port"（adb over WiFi）
	Port   int    // adb 端口（USB 直连为 0；adb over WiFi 使用）
	Name   string // 备注名（来自 personal_devices.name）
}

// Manager adb 操作管理器（线程安全，可并发操作多设备）
type Manager struct {
	adbBin string // adb 可执行路径（默认 "adb"）
	mu     sync.Mutex

	// screenCache 设备实际生效分辨率进程内缓存（serial → 分辨率）。
	// 物理分辨率运行时不变 → 缓存无失效时间；查询/解析失败不写入（下次调用重试）。
	// 独立锁 screenMu 保护：不复用 mu（mu 是 adb 执行串行锁，ScreenSize 需在
	// 不持锁时调 Shell→adb() 再次锁 mu，共用一个非重入锁会死锁）。
	screenMu    sync.Mutex
	screenCache map[string]screenSize
}

// screenSize 设备实际生效分辨率（宽/高，像素）
type screenSize struct {
	w, h int
}

// 参考分辨率：capture/ledgermon 全部硬编码坐标均按 1080x2400 真机实测。
// TapRef/SwipeRef 调用前先按实际生效分辨率等比缩放（见各方法注释）。
const (
	refW = 1080
	refH = 2400
)

// New 创建 adb 管理器；adbBin 为空时使用系统 PATH 中的 adb
func New(adbBin string) *Manager {
	if adbBin == "" {
		adbBin = "adb"
	}
	return &Manager{adbBin: adbBin, screenCache: make(map[string]screenSize)}
}

// adb 执行单条 adb 命令（带超时），返回 stdout。
// 全局串行化：WiFi adb 并发执行多条命令时 connect/命令互相竞争会超时
// （2026-08-28 实测：预热导航与监听轮询并发打 adb，connect context deadline exceeded）。
func (m *Manager) adb(ctx context.Context, timeout time.Duration, serial string, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{"-s", serial}, args...)
	cmd := exec.CommandContext(cctx, m.adbBin, full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("adb %s: %w (%s)", strings.Join(full, " "), err, msg)
	}
	return out.String(), nil
}

// Connect 连接设备（幂等）。失败返回错误（设备离线/容器未起）。
// serial 含 ":" 视为 TCP 地址（host:port，如 127.0.0.1:5555），执行 adb connect；
// 纯 serial（如 USB 直连设备 "4f2bdc30"）跳过 connect，直接 get-state 验证。
func (m *Manager) Connect(ctx context.Context, serial string) error {
	if !strings.Contains(serial, ":") {
		// USB 直连设备：无需 adb connect，验证状态即可
		out, err := m.adb(ctx, 5*time.Second, serial, "get-state")
		if err != nil {
			return err
		}
		if !strings.Contains(out, "device") {
			return fmt.Errorf("adb %s: 设备状态异常 (%s)", serial, strings.TrimSpace(out))
		}
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// 与 adb() 共享全局锁：adb connect 也串行执行，避免并发竞争超时
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := exec.CommandContext(cctx, m.adbBin, "connect", serial)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("adb connect %s: %w", serial, err)
	}
	res := out.String() + errb.String()
	if strings.Contains(res, "cannot connect") || strings.Contains(res, "failed") || strings.Contains(res, "unable") {
		return fmt.Errorf("adb connect %s: %s", serial, strings.TrimSpace(res))
	}
	return nil
}

// Online 设备是否在线（可连且已认证）。每次调用实时检测。
func (m *Manager) Online(ctx context.Context, serial string) bool {
	if err := m.Connect(ctx, serial); err != nil {
		return false
	}
	out, err := m.adb(ctx, 5*time.Second, serial, "get-state")
	if err != nil {
		return false
	}
	return strings.Contains(out, "device")
}

// 设备状态枚举（来自 `adb devices -l`）
const (
	StateDevice       = "device"       // 已授权可用
	StateUnauthorized = "unauthorized" // 待手机端授权 USB 调试
	StateOffline      = "offline"      // adb 链接异常
)

// USBDevice 一台已连 adb 的真机/虚拟设备的瞬时状态（引导向导用）
type USBDevice struct {
	Serial string // adb serial：USB 设备号（如 "4f2bdc30"）或 WiFi host:port（如 "192.168.1.119:5555"）
	State  string // device / unauthorized / offline
	Model  string // 设备型号（如 "MI_8"），可能为空
	Kind   string // "usb"（USB 直连）或 "wifi"（adb over WiFi，host:port）
}

// ListDevices 列出当前已连到本机的真机（adb devices -l 输出，过滤模拟器/容器）。
// 同时返回 USB 直连设备（serial 不含 ":"）与 adb over WiFi 设备（serial 含 ":"，已 connect 过的）。
func (m *Manager) ListDevices(ctx context.Context) ([]USBDevice, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, m.adbBin, "devices", "-l")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("adb devices: %w (%s)", err, strings.TrimSpace(errb.String()))
	}
	var devs []USBDevice
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		serial := fields[0]
		kind := "usb"
		if strings.Contains(serial, ":") {
			kind = "wifi"
		}
		state := fields[1]
		model := ""
		for _, f := range fields[2:] {
			if strings.HasPrefix(f, "model:") {
				model = strings.TrimPrefix(f, "model:")
				break
			}
		}
		devs = append(devs, USBDevice{Serial: serial, State: state, Model: model, Kind: kind})
	}
	return devs, nil
}

// Screenshot 截图当前屏幕，返回 PNG 字节
func (m *Manager) Screenshot(ctx context.Context, serial string) ([]byte, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, m.adbBin, "-s", serial, "exec-out", "screencap", "-p")
	var out bytes.Buffer
	var errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("screencap %s: %w (%s)", serial, err, strings.TrimSpace(errb.String()))
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("screencap %s: 空输出（设备可能刚启动或锁屏）", serial)
	}
	return out.Bytes(), nil
}

// ListFiles 列出设备端目录（shell ls -lt，按修改时间倒序），返回文件名列表
func (m *Manager) ListFiles(ctx context.Context, serial, dir string) ([]string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return nil, err
	}
	out, err := m.adb(ctx, 10*time.Second, serial, "shell", "ls", "-t", dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// PullFile 从设备拉取文件到本地临时目录（adb pull），返回本地路径
func (m *Manager) PullFile(ctx context.Context, serial, devicePath string) (string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tmp, err := os.CreateTemp("", "adbpull_*.png")
	if err != nil {
		return "", err
	}
	local := tmp.Name()
	tmp.Close()
	cmd := exec.CommandContext(cctx, m.adbBin, "-s", serial, "pull", devicePath, local)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		os.Remove(local)
		return "", fmt.Errorf("adb pull %s: %w (%s)", devicePath, err, strings.TrimSpace(errb.String()))
	}
	return local, nil
}

// DumpUI 导出设备当前屏幕的 UI 层级树（uiautomator dump），返回 XML 文本。
// 用途：服务端直接解析账本页/收款记录页等界面（不受 FLAG_SECURE 限制，adb 是系统级通道）。
// 性能说明（2026-08-30 红米k60/MIUI14 实测）：
//   - 单次 dump 固定约 2.3s 为 uiautomator 进程启动 + MIUI 主题初始化开销（无法规避，无 root）；
//   - 复杂页面（支付宝账本 95-104KB）在动画中会 idle 等待至 10s+（实测偶发）；
//   - --compressed 实测仅省 ~8% 体积、无耗时收益，故不用（保持与解析器兼容）；
//   - 优化方向是减少 dump 调用次数（页面无变化时用哈希跳过，见 ledgermon reader），
//     而非压缩单次 dump。
//
// 注意：MIUI 每次 dump 会向 stderr 打印 theme_compatibility.xml 缺失堆栈（约 2KB），
// 不影响结果，属固定开销。
func (m *Manager) DumpUI(ctx context.Context, serial string) (string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return "", err
	}
	if _, err := m.adb(ctx, 8*time.Second, serial, "shell", "uiautomator", "dump", "/sdcard/pgw_ui.xml"); err != nil {
		return "", err
	}
	// dump 落盘后 cat 读取（uiautomator dump 输出在 stdout 提示，XML 写入文件）
	out, err := m.adb(ctx, 8*time.Second, serial, "shell", "cat", "/sdcard/pgw_ui.xml")
	if err != nil {
		return "", err
	}
	return out, nil
}

// Shell 执行任意设备端 shell 命令（超时受控），返回 stdout
func (m *Manager) Shell(ctx context.Context, timeout time.Duration, serial, command string) (string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return "", err
	}
	return m.adb(ctx, timeout, serial, "shell", command)
}

// ScreenSize 查询设备实际生效分辨率（adb shell wm size），返回 (宽, 高) 像素。
// 结果按 serial 进程内缓存：同一设备多次查询不重复走 adb（缓存无失效时间，
// 分辨率运行时不变）。失败（adb 不可达/输出不可解析/数值非法）返回错误——
// 调用方据此跳过本轮操作，不误操作设备。
func (m *Manager) ScreenSize(ctx context.Context, serial string) (w, h int, err error) {
	m.screenMu.Lock()
	if sz, ok := m.screenCache[serial]; ok {
		m.screenMu.Unlock()
		return sz.w, sz.h, nil
	}
	m.screenMu.Unlock()

	out, err := m.Shell(ctx, 5*time.Second, serial, "wm size")
	if err != nil {
		return 0, 0, err
	}
	w, h, err = parseWmSize(out)
	if err != nil {
		return 0, 0, err
	}
	m.screenMu.Lock()
	m.screenCache[serial] = screenSize{w: w, h: h}
	m.screenMu.Unlock()
	return w, h, nil
}

// wmSizeRe 匹配 "WxH" 分辨率标记（如 1080x2400）。包级编译一次复用。
var wmSizeRe = regexp.MustCompile(`(\d+)x(\d+)`)

// parseWmSize 解析 `adb shell wm size` 输出 → 实际生效分辨率 (w, h)。
// 关键：uiautomator dump / input tap 的坐标基于**当前生效分辨率**——设备设置了
// override（开发者选项「最小宽度」/wm size 覆盖）时生效的是 Override 值而非 Physical
// （实测红米 K60：Physical 1440x3200 + Override 1080x2400，UI 坐标按 1080x2400）。
// 解析顺序：优先 "Override size: WxH" 行；缺省回退 "Physical size: WxH"；
// 再兜底任意首个含 "WxH" 的行（裸格式）。均无合法分辨率返回错误。
func parseWmSize(out string) (w, h int, err error) {
	lines := strings.Split(out, "\n")
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "Override size:") {
			if w, h, ok := parseSizeToken(ln); ok {
				return w, h, nil
			}
		}
	}
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "Physical size:") {
			if w, h, ok := parseSizeToken(ln); ok {
				return w, h, nil
			}
		}
	}
	for _, ln := range lines {
		if w, h, ok := parseSizeToken(ln); ok {
			return w, h, nil
		}
	}
	return 0, 0, fmt.Errorf("wm size 输出无法解析出分辨率 (输出: %q)", strings.TrimSpace(out))
}

// parseSizeToken 提取行内首个合法 "WxH" 分辨率标记；无/数值非法返回 ok=false
func parseSizeToken(ln string) (w, h int, ok bool) {
	m := wmSizeRe.FindStringSubmatch(ln)
	if m == nil {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(m[1])
	h, errH := strconv.Atoi(m[2])
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// scaleRef 把参考坐标系（refW x refH）坐标缩放到实际生效分辨率（四舍五入）。
// ScreenSize 查询失败返回错误：宁可跳过本轮点击，也不在未知分辨率下盲点。
func (m *Manager) scaleRef(ctx context.Context, serial string, x, y int) (int, int, error) {
	w, h, err := m.ScreenSize(ctx, serial)
	if err != nil {
		return 0, 0, err
	}
	return (x*w + refW/2) / refW, (y*h + refH/2) / refH, nil
}

// TapRef 按 1080x2400 参考坐标系自动缩放到实际分辨率后点击。
// 用于真机实测于 1080x2400 的硬编码坐标；dump bounds 派生坐标用 Tap 不经缩放。
// ScreenSize 缓存命中零开销；查询失败时返回错误（不盲点）。
func (m *Manager) TapRef(ctx context.Context, serial string, x, y int) error {
	sx, sy, err := m.scaleRef(ctx, serial, x, y)
	if err != nil {
		return fmt.Errorf("参考坐标缩放 (%d,%d) 失败: %w", x, y, err)
	}
	return m.Tap(ctx, serial, sx, sy)
}

// SwipeRef 按 1080x2400 参考坐标系自动缩放到实际分辨率后滑动。
func (m *Manager) SwipeRef(ctx context.Context, serial string, x1, y1, x2, y2, durationMs int) error {
	sx1, sy1, err := m.scaleRef(ctx, serial, x1, y1)
	if err != nil {
		return fmt.Errorf("参考坐标缩放 (%d,%d) 失败: %w", x1, y1, err)
	}
	sx2, sy2, err := m.scaleRef(ctx, serial, x2, y2)
	if err != nil {
		return fmt.Errorf("参考坐标缩放 (%d,%d) 失败: %w", x2, y2, err)
	}
	return m.Swipe(ctx, serial, sx1, sy1, sx2, sy2, durationMs)
}

// StartAppUser 以指定系统用户（双开/分身空间）启动应用；userId<=0 等价 StartApp
func (m *Manager) StartAppUser(ctx context.Context, serial, packageName, activity string, userId int) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	userArg := ""
	if userId > 0 {
		userArg = fmt.Sprintf(" --user %d", userId)
	}
	if activity != "" {
		_, err := m.adb(ctx, 10*time.Second, serial, "shell", "am", "start", userArg, "-n", packageName+"/"+activity)
		return err
	}
	_, err := m.adb(ctx, 10*time.Second, serial, "shell", "am", "start", userArg,
		"-a", "android.intent.action.MAIN",
		"-c", "android.intent.category.LAUNCHER",
		"-p", packageName)
	if err != nil {
		_, err = m.adb(ctx, 10*time.Second, serial, "shell", "monkey", "-p", packageName, "-c", "android.intent.category.LAUNCHER", "1")
	}
	return err
}

// ProcessAlive 目标包名进程是否存活（任意用户空间）。
// 安卓后台 LMK 会杀 App 进程：任务栈随之丢失，热启动 am start 无法恢复到账本页，
// 必须先检测进程再决定走热/冷路径。双开(user 999)进程同样能被 pidof 查到。
func (m *Manager) ProcessAlive(ctx context.Context, serial, packageName string) (bool, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return false, err
	}
	out, err := m.adb(ctx, 8*time.Second, serial, "shell", "pidof", packageName)
	if err != nil {
		// pidof 对不存在的进程返回非零退出码 + 空输出 → 视为进程不存在（非 adb 故障）
		return false, nil
	}
	return strings.TrimSpace(out) != "", nil
}

// RestartAppUser 强制停止并重新启动指定用户空间的应用（确保从干净首页开始，
// 避免微信等 app 恢复到旧页面导致坐标导航落空）。只影响指定 user，不影响其它空间。
// 启动用显式 activity（MAIN/LAUNCHER 类别对双开 user 不可靠，实测启动失败）。
// 启动后轮询 ForegroundApp 直到目标包名出现在前台（最长 waitTimeout），避免固定 sleep 的时序抖动。
func (m *Manager) RestartAppUser(ctx context.Context, serial, packageName, activity string, userId int) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	userArg := ""
	if userId > 0 {
		userArg = fmt.Sprintf(" --user %d", userId)
	}
	if _, err := m.adb(ctx, 10*time.Second, serial, "shell", "am", "force-stop", userArg, packageName); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond)
	if err := m.StartAppUser(ctx, serial, packageName, activity, userId); err != nil {
		return err
	}
	// 轮询前台确认（最长 10s）；确认不了不视为失败（部分 app 前台判定口径差异），
	// 由调用方后续 activity 校验兜底。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fg, err := m.ForegroundApp(ctx, serial)
		if err == nil && fg == packageName {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(800 * time.Millisecond):
		}
	}
	return nil
}

// Rm 删除设备端文件/目录（用于相册旧码清理）
func (m *Manager) Rm(ctx context.Context, serial, path string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	_, err := m.adb(ctx, 10*time.Second, serial, "shell", "rm", "-f", path)
	return err
}

// Tap 模拟点击屏幕坐标（像素，基于截图分辨率）
func (m *Manager) Tap(ctx context.Context, serial string, x, y int) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	_, err := m.adb(ctx, 8*time.Second, serial, "shell", "input", "tap", fmt.Sprintf("%d", x), fmt.Sprintf("%d", y))
	return err
}

// Swipe 模拟滑动
func (m *Manager) Swipe(ctx context.Context, serial string, x1, y1, x2, y2, durationMs int) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	_, err := m.adb(ctx, 8*time.Second, serial, "shell", "input", "swipe",
		fmt.Sprintf("%d", x1), fmt.Sprintf("%d", y1), fmt.Sprintf("%d", x2), fmt.Sprintf("%d", y2),
		fmt.Sprintf("%d", durationMs))
	return err
}

// InputText 输入文本（模拟键盘；特殊字符按 adb input 规则转义）
func (m *Manager) InputText(ctx context.Context, serial, text string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	// adb shell input text 对空格/特殊字符敏感：用 %s 转义
	escaped := strings.ReplaceAll(text, " ", "%s")
	escaped = strings.ReplaceAll(escaped, "&", "\\&")
	_, err := m.adb(ctx, 8*time.Second, serial, "shell", "input", "text", escaped)
	return err
}

// KeyEvent 发送按键（如 KEYCODE_BACK=4, KEYCODE_HOME=3, KEYCODE_ENTER=66, KEYCODE_CLEAR=28）
func (m *Manager) KeyEvent(ctx context.Context, serial string, keycode int) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	_, err := m.adb(ctx, 8*time.Second, serial, "shell", "input", "keyevent", fmt.Sprintf("%d", keycode))
	return err
}

// StartApp 启动应用（按包名 + 可选 activity）
// 未指定 activity 时通过 LAUNCHER 类别启动默认入口（避免依赖 monkey，精简镜像无 monkey）
func (m *Manager) StartApp(ctx context.Context, serial, packageName, activity string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	if activity != "" {
		_, err := m.adb(ctx, 10*time.Second, serial, "shell", "am", "start", "-n", packageName+"/"+activity)
		return err
	}
	// 优先：am start 带 MAIN/LAUNCHER 类别（等效 monkey 但更可靠）
	_, err := m.adb(ctx, 10*time.Second, serial, "shell", "am", "start",
		"-a", "android.intent.action.MAIN",
		"-c", "android.intent.category.LAUNCHER",
		"-p", packageName)
	if err != nil {
		// 兜底：monkey（部分镜像支持）
		_, err = m.adb(ctx, 10*time.Second, serial, "shell", "monkey", "-p", packageName, "-c", "android.intent.category.LAUNCHER", "1")
	}
	return err
}

// ForegroundApp 当前前台 app 描述串（形如 "u0 com.eg.android.AlipayGphone/.PayeeQRActivity t30545"）。
// 保留原语义：capture 包用 strings.Contains 匹配 Activity 名（如 "PayeeQRActivity"），
// 只返回包名会破坏这些检查。需要结构化三元组（pkg/activity/userID）请用 ForegroundAppDetail。
func (m *Manager) ForegroundApp(ctx context.Context, serial string) (string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return "", err
	}
	out, err := m.adb(ctx, 8*time.Second, serial, "shell", "dumpsys", "activity", "activities")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mResumedActivity") || strings.HasPrefix(line, "topResumedActivity") || strings.HasPrefix(line, "mFocusedApp") {
			if idx := strings.Index(line, "{"); idx >= 0 {
				rest := line[idx:]
				if p := strings.Index(rest, " "); p >= 0 {
					return strings.TrimPrefix(rest[p+1:], "u0/"), nil
				}
			}
		}
	}
	return "", fmt.Errorf("未找到前台 activity")
}

// ForegroundAppDetail 当前前台 app 三要素：包名 + Activity + 用户空间（0=主空间，999=双开分身）。
//
// dumpsys activity activities 中 mResumedActivity 行实际格式：
//
//	mResumedActivity: ActivityRecord{974988a u0 com.eg.android.AlipayGphone/.XRiverTransActivity t30545}
//	mResumedActivity: ActivityRecord{974988a u999 com.tencent.mm/.ui.LauncherUI t30545}
//
// 双开(user 999)与主空间(user 0)的同一 Activity 名前缀不同（u0 / u999），
// 账本页监听必须按 userID 区分（否则 user0 的监听循环会把 user999 的账本页误判为已就绪 → 读错数据）。
// 解析失败（如前台是桌面/无 resumed activity）返回空三元组 + 错误。
func (m *Manager) ForegroundAppDetail(ctx context.Context, serial string) (pkg, activity string, userID int, err error) {
	if err = m.Connect(ctx, serial); err != nil {
		return "", "", 0, err
	}
	out, err := m.adb(ctx, 8*time.Second, serial, "shell", "dumpsys", "activity", "activities")
	if err != nil {
		return "", "", 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "mResumedActivity") && !strings.HasPrefix(line, "topResumedActivity") && !strings.HasPrefix(line, "mFocusedApp") {
			continue
		}
		if idx := strings.Index(line, "{"); idx >= 0 {
			rest := line[idx:]
			if p := strings.Index(rest, " "); p >= 0 {
				fields := strings.Fields(rest[p+1:])
				if len(fields) == 0 {
					continue
				}
				first := fields[0]
				// 用户空间前缀：u0 / u999（Android 多用户）；无前缀视为主空间
				if strings.HasPrefix(first, "u") && len(first) > 1 && first[1] >= '0' && first[1] <= '9' {
					if n, nerr := strconv.Atoi(first[1:]); nerr == nil {
						userID = n
						if len(fields) >= 2 {
							first = fields[1]
						} else {
							continue
						}
					}
				}
				pkg, activity = splitPackageActivity(first)
				if pkg == "" {
					continue
				}
				return pkg, activity, userID, nil
			}
		}
	}
	return "", "", 0, fmt.Errorf("未找到前台 activity")
}

// splitPackageActivity 拆分 "com.pkg/.Activity" → (包名, Activity 名)
// Activity 可能为 ".Xxx"（相对）或 "com.pkg.Xxx"（全名）；无 Activity 时 activity 为空。
func splitPackageActivity(full string) (pkg, activity string) {
	if i := strings.Index(full, "/"); i >= 0 {
		pkg = full[:i]
		activity = full[i+1:]
		if strings.HasPrefix(activity, ".") {
			activity = pkg + activity
		}
		return pkg, activity
	}
	return full, ""
}

// WakeUp 唤醒屏幕并解锁（模拟电源键 + 滑动解锁）
func (m *Manager) WakeUp(ctx context.Context, serial string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	m.adb(ctx, 5*time.Second, serial, "shell", "input", "keyevent", "224") // KEYCODE_WAKEUP
	return nil
}

// GoHome 派发 HOME 键回桌面，清掉前一个账户驻留的前台。
// 冷启动导航前调用：MIUI 双用户隔离下，跨 user（如 u999 微信占前台时）冷启动另一
// 用户（u0 支付宝）会被驻留前台挡住，am start 无法抢占（实测 30s 前台纹丝不动）。
// 先清 PINNED（若残留 task lock 会拦截 HOME），再发 HOME、等桌面稳定。
func (m *Manager) GoHome(ctx context.Context, serial string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	_, _ = m.adb(ctx, 5*time.Second, serial, "shell", "am", "task", "lock", "stop") // 清 PINNED（幂等）
	time.Sleep(300 * time.Millisecond)
	_, err := m.adb(ctx, 5*time.Second, serial, "shell", "input", "keyevent", "3") // KEYCODE_HOME
	return err
}

// PinCurrentTask 屏幕固定当前前台任务（task lock，2026-08-28 真机验证）：
// 固定期间其他 app 无法抢占前台（am start 被拦截，error 101 START_CANCELED），
// 用于冷启动导航等长操作期间防止监听 app/系统弹窗打断。
// 返回当前前台 taskId（固定成功后该 task 即被锁定）。
func (m *Manager) PinCurrentTask(ctx context.Context, serial string) (string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return "", err
	}
	out, err := m.adb(ctx, 8*time.Second, serial, "shell", "dumpsys", "activity", "activities")
	if err != nil {
		return "", err
	}
	taskID := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "topResumedActivity") || strings.HasPrefix(line, "mResumedActivity") {
			if i := strings.LastIndex(line, "t"); i >= 0 {
				cand := strings.TrimSuffix(line[i+1:], "}")
				if cand != "" {
					taskID = cand
				}
			}
			break
		}
	}
	if taskID == "" {
		return "", fmt.Errorf("未找到前台任务 taskId")
	}
	if _, err := m.adb(ctx, 8*time.Second, serial, "shell", "am", "task", "lock", taskID); err != nil {
		return "", fmt.Errorf("固定前台任务失败: %w", err)
	}
	return taskID, nil
}

// UnpinTask 解除屏幕固定（am task lock stop）。解锁后其他 app 可正常抢占前台。
// 幂等：非固定状态调用无副作用（返回 nil）。
func (m *Manager) UnpinTask(ctx context.Context, serial string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	_, err := m.adb(ctx, 8*time.Second, serial, "shell", "am", "task", "lock", "stop")
	return err
}

// FindLedgerTaskID 扫描 dumpsys 查询指定 user 的账本页任务栈 ID（如 lite01 亲和栈）。
// 在 dumpsys activity activities 输出中找包含 actSubstr 且匹配 userID 的 ActivityRecord，
// 提取其 taskId（如 "22233"）。账本页不存在/进程被清时返回空 taskID+错误。
func (m *Manager) FindLedgerTaskID(ctx context.Context, serial, pkg, actSubstr string, userID int) (string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return "", err
	}
	out, err := m.adb(ctx, 8*time.Second, serial, "shell", "dumpsys", "activity", "activities")
	if err != nil {
		return "", fmt.Errorf("dumpsys 失败: %w", err)
	}
	userPrefix := fmt.Sprintf("u%d", userID)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// 匹配 ActivityRecord{... uN pkg/...actSubstr...} tNNNN}
		// 示例：ActivityRecord{fe2acf2 u999 com.eg.android.AlipayGphone/...XRiverActivity$App01} t22233}
		if !strings.Contains(line, userPrefix) || !strings.Contains(line, actSubstr) {
			continue
		}
		// 从行尾提取 tNNNN（如 "t22233}" → "22233"）
		if i := strings.LastIndex(line, " t"); i >= 0 {
			cand := strings.TrimSuffix(line[i+2:], "}")
			// 校验纯数字，避免匹配其他含 t 的 token
			if cand != "" && cand[0] >= '0' && cand[0] <= '9' {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("未找到匹配的账本页任务栈（pkg=%s, act=%s, user=%d）", pkg, actSubstr, userID)
}

// TaskLock 用 am task lock 锁定并唤醒指定任务栈到前台。相当于微信热启动方案
// taskLock(serial, taskID)，~160ms。
// 注意：锁定后设备进入 PINNED 模式，会拦截 input tap/swipe，调用方应在确认
// 前台后调用 UnpinTask 释放。
func (m *Manager) TaskLock(ctx context.Context, serial, taskID string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	_, err := m.adb(ctx, 8*time.Second, serial, "shell", "am", "task", "lock", taskID)
	return err
}

// TCPIP 开启设备的无线 adb（`adb tcpip <port>`）。
// USB 直连时调用：设备进入 TCP/IP 监听模式，之后同一局域网可通过 <ip>:<port> 连接。
// 幂等：已开启会输出 "restarting in TCP mode port: N"，重复调用无副作用。
func (m *Manager) TCPIP(ctx context.Context, serial string, port int) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	out, err := m.adb(ctx, 10*time.Second, serial, "tcpip", fmt.Sprintf("%d", port))
	if err != nil {
		return fmt.Errorf("adb tcpip 失败: %w", err)
	}
	if !strings.Contains(out, "restarting in TCP mode port") {
		return fmt.Errorf("adb tcpip 未确认成功: %s", strings.TrimSpace(out))
	}
	return nil
}

// WifiIP 获取设备 WiFi 局域网 IP（引导向导自动配置 WiFi 备用通道用）。
// 依次尝试多种方式：wlan0 网卡 → eth0 → getprop（部分 ROM 无 ip 命令时的兜底）。
// 返回 "" 表示无法确定（设备未连 WiFi 或命令不可用）。
func (m *Manager) WifiIP(ctx context.Context, serial string) string {
	// 1. `ip addr show wlan0`（标准 Android）
	if out, err := m.adb(ctx, 6*time.Second, serial, "shell", "ip", "addr", "show", "wlan0"); err == nil {
		if ip := parseIPv4(out); ip != "" {
			return ip
		}
	}
	// 2. eth0（部分设备 wlan0 名为 eth0）
	if out, err := m.adb(ctx, 6*time.Second, serial, "shell", "ip", "addr", "show", "eth0"); err == nil {
		if ip := parseIPv4(out); ip != "" {
			return ip
		}
	}
	// 3. dhcp getprop（无 ip 命令的精简 ROM）
	if out, err := m.adb(ctx, 6*time.Second, serial, "shell", "getprop", "dhcp.wlan0.ipaddress"); err == nil {
		if ip := strings.TrimSpace(out); ip != "" {
			return ip
		}
	}
	return ""
}

// parseIPv4 从 `ip addr` 输出提取首个内网 IPv4（跳过 127.0.0.1）
func parseIPv4(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "inet ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		addr := fields[1]
		if idx := strings.Index(addr, "/"); idx >= 0 {
			addr = addr[:idx]
		}
		if addr == "" || strings.HasPrefix(addr, "127.") {
			continue
		}
		return addr
	}
	return ""
}
