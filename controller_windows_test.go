package main

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEndpoint 记录控制循环的实际静音操作，不改变本机麦克风。
type fakeEndpoint struct {
	mu             sync.Mutex // 保护测试线程和音频控制线程共享的设备状态。
	dev            Device     // 模拟端点标识。
	mute           bool       // 模拟设备当前静音状态。
	err            error      // 模拟设备拔出或驱动失败。
	volume         float32    // 模拟系统录音音量，单位 dB。
	maximum        float32    // 模拟硬件最高音量。
	volumeErr      error      // 仅音量控制失败，不影响静音。
	volumeWriteErr error      // 只拒绝音量写入，模拟只读驱动。
	volumeStep     float32    // 模拟离散音量步长，0 表示可连续设置。
	volumeWrites   int        // 验证默认设置和重复应用不会写入音量。
}

// info 返回端点的只读标识。
func (f *fakeEndpoint) info() Device { return f.dev }

// close 模拟释放接口，设备本身可以继续由其他接口引用。
func (f *fakeEndpoint) close() {}

// setMute 模拟驱动接受或拒绝静音请求。
func (f *fakeEndpoint) setMute(mute bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.mute = mute
	return nil
}

// muted 同时供音频线程查询状态和测试断言使用。
func (f *fakeEndpoint) muted() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mute, f.err
}

// setError 模拟设备状态变化，所有共享访问都经过互斥锁。
func (f *fakeEndpoint) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// volumeDB 读取虚拟增益，不访问本机音频设备。
func (f *fakeEndpoint) volumeDB() (float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.volume, f.volumeErr
}

// volumeRangeDB 提供可测的驱动上限。
func (f *fakeEndpoint) volumeRangeDB() (float32, float32, float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return -96, f.maximum, f.volumeStep, f.volumeErr
}

// setVolumeDB 只修改虚拟音量，并记录写入次数。
func (f *fakeEndpoint) setVolumeDB(value float32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.volumeErr != nil {
		return f.volumeErr
	}
	if f.volumeWriteErr != nil {
		return f.volumeWriteErr
	}
	if value < -96 || value > f.maximum {
		return fmt.Errorf("音量超出设备范围：%f", value)
	}
	if f.volumeStep > 0 {
		value = min(f.maximum, -96+float32(math.Round(float64((value+96)/f.volumeStep)))*f.volumeStep)
	}
	f.volume = value
	f.volumeWrites++
	return nil
}

// fakeAudio 模拟默认设备改变及设备拔插后产生的新接口。
type fakeAudio struct {
	mu        sync.Mutex               // 保护设备映射。
	items     map[string]*fakeEndpoint // 当前连接的设备。
	defaultID string                   // 当前默认通信设备。
}

// devices 返回模拟的活动设备集合。
func (f *fakeAudio) devices() ([]Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []Device
	for _, item := range f.items {
		result = append(result, item.dev)
	}
	return result, nil
}

// endpoint 按 ID 或默认设备打开模拟端点。
func (f *fakeAudio) endpoint(id string) (audioEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		id = f.defaultID
	}
	if item := f.items[id]; item != nil {
		return item, nil
	}
	return nil, fmt.Errorf("设备未连接：%s", id)
}

// replace 模拟切换默认设备，或同一个设备拔插后更换接口。
func (f *fakeAudio) replace(item *fakeEndpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[item.dev.ID] = item
	f.defaultID = item.dev.ID
}

// waitUntil 在限定时间内等待行为发生，测试依赖实际状态而非假设固定延时。
func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待控制循环达到期望状态超时")
}

// testController 运行真实控制循环，替换键盘与硬件来源，避免测试影响用户输入。
func testController(t *testing.T, backend *fakeAudio, keys *atomic.Bool, reads *atomic.Int64) *controller {
	t.Helper()
	cfg := Config{Hotkey: "F8"}
	c := &controller{commands: make(chan controlCommand, 8), done: make(chan struct{})}
	c.snapshot.Store(&Snapshot{Config: cfg})
	go func() {
		defer close(c.done)
		c.control(cfg, backend, func(Hotkey) (bool, bool) {
			active := keys.Load()
			reads.Add(1)
			return active, !active
		})
	}()
	t.Cleanup(func() {
		if err := c.stop(); err != nil {
			t.Errorf("测试结束时无法恢复麦克风并退出：%v", err)
		}
	})
	return c
}

// releaseKeys 确认控制线程实际观察到松键，再进行下一次按住。
func releaseKeys(t *testing.T, keys *atomic.Bool, reads *atomic.Int64) {
	t.Helper()
	keys.Store(false)
	before := reads.Load()
	waitUntil(t, func() bool { return reads.Load() >= before+2 })
}

// assertMutedAfterReads 验证在持续按住时不会因恢复或换设备而意外开麦。
func assertMutedAfterReads(t *testing.T, mic *fakeEndpoint, reads *atomic.Int64) {
	t.Helper()
	before := reads.Load()
	waitUntil(t, func() bool { return reads.Load() >= before+5 })
	if mute, err := mic.muted(); err != nil || !mute {
		t.Fatalf("应保持静音：muted=%v, err=%v", mute, err)
	}
}

// TestControllerHoldPauseSafety 验证真实循环的按住、释放、暂停、锁屏和退出。
func TestControllerHoldPauseSafety(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A", Name: "麦克风 A"}}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	keys.Store(true) // 启动时用户已经按住快捷键。
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	assertMutedAfterReads(t, mic, &reads)
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return !mute })
	keys.Store(false)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return mute })
	// 外部程序解除静音时，控制循环会在状态检查周期内纠正。
	_ = mic.setMute(false)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return mute })
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return !mute })
	if err := c.request(controlCommand{kind: "pause"}); err != nil {
		t.Fatal(err)
	}
	assertMutedAfterReads(t, mic, &reads)
	if err := c.request(controlCommand{kind: "pause"}); err != nil {
		t.Fatal(err)
	}
	assertMutedAfterReads(t, mic, &reads)
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return !mute })
	if err := c.request(controlCommand{kind: "safety", flag: blockRecording, on: true}); err != nil {
		t.Fatal(err)
	}
	assertMutedAfterReads(t, mic, &reads)
	if err := c.request(controlCommand{kind: "safety", flag: blockRecording, on: false}); err != nil {
		t.Fatal(err)
	}
	assertMutedAfterReads(t, mic, &reads)
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return !mute })
	for _, cmd := range []controlCommand{
		{kind: "safety", flag: blockSession, on: true},
		{kind: "safety", flag: blockSuspend, on: true},
		{kind: "safety", flag: blockSession, on: false},
	} {
		if err := c.request(cmd); err != nil {
			t.Fatal(err)
		}
	}
	if !c.snapshot.Load().Blocked {
		t.Fatal("解除锁屏时仍处于休眠阻止状态，不应恢复")
	}
	assertMutedAfterReads(t, mic, &reads)
	if err := c.request(controlCommand{kind: "safety", flag: blockSuspend, on: false}); err != nil {
		t.Fatal(err)
	}
	assertMutedAfterReads(t, mic, &reads)
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return !mute })
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	if mute, _ := mic.muted(); mute {
		t.Fatal("正常退出后必须解除静音")
	}
}

// TestControllerDeviceChangeAndReconnect 验证更换设备和拔插后恢复，不沿用长按状态。
func TestControllerDeviceChangeAndReconnect(t *testing.T) {
	first := &fakeEndpoint{dev: Device{ID: "A", Name: "麦克风 A"}}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": first}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := first.muted(); return !mute })
	second := &fakeEndpoint{dev: Device{ID: "B", Name: "麦克风 B"}}
	backend.replace(second)
	if err := c.request(controlCommand{kind: "refresh"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return c.snapshot.Load().Device == "麦克风 B" })
	if mute, _ := first.muted(); !mute {
		t.Fatal("更换设备后，旧麦克风必须静音")
	}
	assertMutedAfterReads(t, second, &reads)
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := second.muted(); return !mute })
	second.setError(&audioAPIError{Code: 0x88890004})
	waitUntil(t, func() bool { return !c.snapshot.Load().Ready })
	reconnected := &fakeEndpoint{dev: Device{ID: "B", Name: "麦克风 B"}}
	backend.replace(reconnected)
	if err := c.request(controlCommand{kind: "refresh"}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	assertMutedAfterReads(t, reconnected, &reads)
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := reconnected.muted(); return !mute })
}

// TestControllerExitUnmutes 验证退出时松键、暂停或启动前已静音都不会让麦克风保持静音。
func TestControllerExitUnmutes(t *testing.T) {
	for _, tc := range []struct {
		name         string // 退出场景名称。
		initialMuted bool   // 工具启动前麦克风是否已静音。
		paused       bool   // 是否从暂停状态退出。
	}{
		{name: "松键时退出"},
		{name: "暂停时退出", paused: true},
		{name: "启动前已静音", initialMuted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mic := &fakeEndpoint{dev: Device{ID: "A", Name: "麦克风 A"}, mute: tc.initialMuted}
			backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
			var keys atomic.Bool
			var reads atomic.Int64
			c := testController(t, backend, &keys, &reads)
			waitUntil(t, func() bool { return c.snapshot.Load().Ready })
			if tc.paused {
				if err := c.request(controlCommand{kind: "pause"}); err != nil {
					t.Fatal(err)
				}
			}
			if mute, _ := mic.muted(); !mute {
				t.Fatal("运行期间未按发言键时应保持静音")
			}
			if err := c.stop(); err != nil {
				t.Fatal(err)
			}
			if mute, _ := mic.muted(); mute {
				t.Fatal("退出完成后，清理逻辑不能再次把麦克风静音")
			}
		})
	}
}

// TestControllerExitRestoreFailure 阻止在驱动拒绝解除静音时假报退出成功，恢复后允许重试。
func TestControllerExitRestoreFailure(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A", Name: "麦克风 A"}}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return !mute })
	mic.setError(errors.New("模拟驱动拒绝解除静音"))
	defer mic.setError(nil)
	if err := c.stop(); err == nil {
		t.Fatal("解除静音失败时不应假报成功退出")
	}
	select {
	case <-c.done:
		t.Fatal("解除静音失败时应继续运行，允许用户处理问题")
	default:
	}
	mic.setError(nil)
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	if mute, _ := mic.muted(); mute {
		t.Fatal("重试退出后应解除静音")
	}
}

// TestControllerExitRestoresAllDevices 验证退出会恢复本次使用过的所有仍连接麦克风。
func TestControllerExitRestoresAllDevices(t *testing.T) {
	first := &fakeEndpoint{dev: Device{ID: "A", Name: "麦克风 A"}, volume: -12, maximum: 12}
	second := &fakeEndpoint{dev: Device{ID: "B", Name: "麦克风 B"}, volume: -18, maximum: 12}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": first, "B": second}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	if err := c.request(controlCommand{kind: "apply", config: Config{Hotkey: "F8", EnhancementPercent: 200}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return c.snapshot.Load().EnhancementActual > 199 })
	if err := c.request(controlCommand{kind: "apply", config: Config{Hotkey: "F8", DeviceID: "B", EnhancementPercent: 150}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return c.snapshot.Load().Device == "麦克风 B" })
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	for _, mic := range []*fakeEndpoint{first, second} {
		if muted, err := mic.muted(); err != nil || muted {
			t.Fatalf("退出后设备 %s 未恢复：muted=%v, err=%v", mic.info().Name, muted, err)
		}
	}
	assertVolume(t, first, -12)
	assertVolume(t, second, -18)
}

// TestControllerExitGainFailureStillUnmutes 验证音量恢复失败也先解除静音，等待重试期间不会再次接管。
func TestControllerExitGainFailureStillUnmutes(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A", Name: "测试麦克风"}, volume: -12, maximum: 12}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	if err := c.request(controlCommand{kind: "apply", config: Config{Hotkey: "F8", EnhancementPercent: 200}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return c.snapshot.Load().EnhancementActual > 199 })
	mic.mu.Lock()
	mic.volumeWriteErr = errors.New("模拟驱动拒绝恢复音量")
	mic.mu.Unlock()
	defer func() {
		mic.mu.Lock()
		mic.volumeWriteErr = nil
		mic.mu.Unlock()
	}()
	if err := c.stop(); err == nil {
		t.Fatal("恢复音量失败时应返回错误供用户重试")
	}
	before := reads.Load()
	time.Sleep(150 * time.Millisecond) // 超过原来的静音检查周期，确认退出失败后不会恢复静音循环。
	if muted, _ := mic.muted(); muted || reads.Load() != before {
		t.Fatal("关闭失败后不应继续按键检测或把麦克风再次静音")
	}
	mic.mu.Lock()
	mic.volumeWriteErr = nil
	mic.mu.Unlock()
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	assertVolume(t, mic, -12)
}

// TestControllerCloseRetryReconnect 验证关闭失败后重连设备，再次关闭能使用新的端点接口恢复。
func TestControllerCloseRetryReconnect(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A", Name: "测试麦克风"}, volume: -12, maximum: 12}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	if err := c.request(controlCommand{kind: "apply", config: Config{Hotkey: "F8", EnhancementPercent: 200}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return c.snapshot.Load().EnhancementActual > 199 })
	boosted, _ := mic.volumeDB()
	mic.setError(errors.New("模拟关闭时驱动拒绝恢复"))
	if err := c.stop(); err == nil {
		t.Fatal("恢复失败应等待重试")
	}
	mic.setError(&audioAPIError{Code: 0x88890004})
	reconnected := &fakeEndpoint{dev: mic.dev, volume: boosted, maximum: 12, mute: true}
	backend.replace(reconnected)
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	if muted, _ := reconnected.muted(); muted {
		t.Fatal("退出重试必须恢复重新连接的端点")
	}
	assertVolume(t, reconnected, -12)
}

// TestControllerPanicRestores 验证控制线程可捕获的异常也会恢复音量并解除静音。
func TestControllerPanicRestores(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A", Name: "测试麦克风"}, volume: -12, maximum: 12}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
	c := &controller{commands: make(chan controlCommand, 8), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		c.control(Config{Hotkey: "F8", EnhancementPercent: 200}, backend, func(Hotkey) (bool, bool) {
			panic("模拟音频控制线程异常")
		})
	}()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("控制线程异常后没有结束")
	}
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	if muted, err := mic.muted(); err != nil || muted {
		t.Fatalf("异常清理后麦克风没有恢复：muted=%v, err=%v", muted, err)
	}
	assertVolume(t, mic, -12)
}
