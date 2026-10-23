package main

import (
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
)

// assertVolume 验证实际 dB 值，避免只验证界面设置而没有真正应用音量。
func assertVolume(t *testing.T, mic *fakeEndpoint, want float64) {
	t.Helper()
	got, err := mic.volumeDB()
	if err != nil || math.Abs(float64(got)-want) > 0.001 {
		t.Fatalf("实际音量=%f dB，期望=%f dB，错误=%v", got, want, err)
	}
}

// TestGainBaselineAndRestore 验证默认无写入、比例换算、不叠加增强和关闭后恢复。
func TestGainBaselineAndRestore(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A"}, volume: -12, maximum: 12}
	gains := make(gainSession)
	actual, warning := gains.apply(mic, 100)
	if actual != 100 || warning != "" || mic.volumeWrites != 0 {
		t.Fatalf("100%% 不应改变音量：actual=%f, warning=%q, writes=%d", actual, warning, mic.volumeWrites)
	}
	actual, warning = gains.apply(mic, 150)
	if math.Abs(actual-150) > 0.01 || warning != "" {
		t.Fatalf("150%% 增强失败：actual=%f, warning=%q", actual, warning)
	}
	assertVolume(t, mic, -12+20*math.Log10(1.5))
	writes := mic.volumeWrites
	gains.apply(mic, 150)
	if mic.volumeWrites != writes {
		t.Fatal("重复应用不能重复写入或叠加增强")
	}
	gains.apply(mic, 200)
	assertVolume(t, mic, -12+20*math.Log10(2))
	gains.apply(mic, 125)
	assertVolume(t, mic, -12+20*math.Log10(1.25))
	actual, warning = gains.apply(mic, 100)
	if actual != 100 || warning != "" || len(gains) != 0 {
		t.Fatalf("关闭增强失败：actual=%f, warning=%q", actual, warning)
	}
	assertVolume(t, mic, -12)
}

// TestGainLimitAndRetry 验证硬件上限提示与驱动失败后保留原始音量重试。
func TestGainLimitAndRetry(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A"}, volume: -2, maximum: 0}
	gains := make(gainSession)
	actual, warning := gains.apply(mic, 200)
	if math.Abs(actual-125.8925) > 0.01 || !strings.Contains(warning, "上限") {
		t.Fatalf("应报告硬件上限与实际增益：actual=%f, warning=%q", actual, warning)
	}
	assertVolume(t, mic, 0)
	mic.volumeErr = errors.New("音量控制失败")
	if err := gains.restore(mic); err == nil || len(gains) != 1 {
		t.Fatal("恢复失败必须保留原始音量供重试")
	}
	mic.volumeErr = nil
	if err := gains.restore(mic); err != nil {
		t.Fatal(err)
	}
	assertVolume(t, mic, -2)
	mic.volumeErr = errors.New("不支持增益")
	if _, warning = gains.apply(mic, 150); warning == "" {
		t.Fatal("不支持增强时应明确提示")
	}
	mic.volumeErr = nil
	gains.apply(mic, 150)
	assertVolume(t, mic, 0)
}

// TestGainReadOnlyAndCoarseDriver 验证只读驱动不会阻止恢复，粗粒度驱动不会明显超出所选增益。
func TestGainReadOnlyAndCoarseDriver(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A"}, volume: -12, maximum: 12, volumeWriteErr: errors.New("不允许设置音量")}
	gains := make(gainSession)
	if _, warning := gains.apply(mic, 200); warning == "" {
		t.Fatal("驱动拒绝写入应显示失败提示")
	}
	if err := gains.restore(mic); err != nil || len(gains) != 0 {
		t.Fatalf("增强未改变音量时不应阻止退出：%v", err)
	}
	mic.volumeWriteErr = nil
	mic.volumeStep = 1.5
	actual, warning := gains.apply(mic, 110)
	if actual > 110.6 || actual < 100 || !strings.Contains(warning, "驱动精度") {
		t.Fatalf("粗粒度驱动应限制实际增益并显示提示：actual=%f, warning=%q", actual, warning)
	}
	if err := gains.restore(mic); err != nil {
		t.Fatal(err)
	}
	assertVolume(t, mic, -12)
}

// TestGainReconnectBaseline 验证重连后使用原始基准，而不是把增强后的值再次放大。
func TestGainReconnectBaseline(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A"}, volume: -12, maximum: 12}
	gains := make(gainSession)
	gains.apply(mic, 200)
	gains.invalidate(mic)
	mic.setError(&audioAPIError{Code: 0x88890004})
	if err := gains.restore(mic); err == nil {
		t.Fatal("设备拔出时应保留恢复信息")
	}
	reconnected := &fakeEndpoint{dev: mic.dev, volume: mic.volume, maximum: 12}
	gains.apply(reconnected, 200)
	assertVolume(t, reconnected, -12+20*math.Log10(2))
	if err := gains.restore(reconnected); err != nil {
		t.Fatal(err)
	}
	assertVolume(t, reconnected, -12)
}

// TestControllerGainSwitchAndExit 验证真实控制循环增强、换设备恢复，以及退出时恢复音量且解除静音。
func TestControllerGainSwitchAndExit(t *testing.T) {
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
	assertVolume(t, first, -12+20*math.Log10(2))
	if err := c.request(controlCommand{kind: "apply", config: Config{Hotkey: "F8", DeviceID: "B", EnhancementPercent: 150}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		s := c.snapshot.Load()
		return s.Device == "麦克风 B" && math.Abs(s.EnhancementActual-150) < 0.01
	})
	assertVolume(t, first, -12)
	assertVolume(t, second, -18+20*math.Log10(1.5))
	if err := c.stop(); err != nil {
		t.Fatal(err)
	}
	assertVolume(t, second, -18)
	if muted, _ := second.muted(); muted {
		t.Fatal("正常退出恢复音量后不能保持静音")
	}
}

// TestControllerGainFailureKeepsPTT 验证音量增强失败不影响按键发言和静音控制。
func TestControllerGainFailureKeepsPTT(t *testing.T) {
	mic := &fakeEndpoint{dev: Device{ID: "A", Name: "麦克风 A"}, volume: -12, maximum: 12, volumeErr: errors.New("增强不可用")}
	backend := &fakeAudio{items: map[string]*fakeEndpoint{"A": mic}, defaultID: "A"}
	var keys atomic.Bool
	var reads atomic.Int64
	c := testController(t, backend, &keys, &reads)
	waitUntil(t, func() bool { return c.snapshot.Load().Ready })
	if err := c.request(controlCommand{kind: "apply", config: Config{Hotkey: "F8", EnhancementPercent: 150}}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return c.snapshot.Load().EnhancementWarning != "" })
	if s := c.snapshot.Load(); !s.Ready || s.Error != "" {
		t.Fatalf("增强失败不能令按键发言不可用：%+v", s)
	}
	releaseKeys(t, &keys, &reads)
	keys.Store(true)
	waitUntil(t, func() bool { muted, _ := mic.muted(); return !muted })
	keys.Store(false)
	waitUntil(t, func() bool { muted, _ := mic.muted(); return muted })
}
