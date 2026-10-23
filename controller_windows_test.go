package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEndpoint 记录控制循环的实际静音操作，不改变本机麦克风。
type fakeEndpoint struct {
	mu   sync.Mutex // 保护测试线程和音频控制线程共享的设备状态。
	dev  Device     // 模拟端点标识。
	mute bool       // 模拟设备当前静音状态。
	err  error      // 模拟设备拔出或驱动失败。
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
			t.Errorf("测试结束时无法静音退出：%v", err)
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
	if mute, _ := mic.muted(); !mute {
		t.Fatal("退出时必须保持静音")
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

// TestControllerMuteFailure 阻止在驱动拒绝静音时假报成功退出，恢复后允许重试。
func TestControllerMuteFailure(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A", Name: "麦克风 A"}}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	keys.Store(true)
	waitUntil(t, func() bool { mute, _ := mic.muted(); return !mute })
	mic.setError(errors.New("模拟驱动拒绝静音"))
	defer mic.setError(nil)
	if err := c.stop(); err == nil {
		t.Fatal("静音失败时不应假报成功退出")
	}
	select {
	case <-c.done:
		t.Fatal("静音失败时应继续运行，允许用户处理问题")
	default:
	}
	mic.setError(nil)
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	if mute, _ := mic.muted(); !mute {
		t.Fatal("重试退出后应保持静音")
	}
}
