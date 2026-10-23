package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestHotkeyParsing 验证用户输入、别名以及不能可靠用于按住发言的组合。
func TestHotkeyParsing(t *testing.T) {
	for _, tc := range []struct {
		input string   // 用户输入。
		name  string   // 期望的标准名称。
		keys  []uint16 // 期望的虚拟键码。
	}{
		{"f8", "F8", []uint16{0x77}},
		{"v", "V", []uint16{0x56}},
		{" control + space ", "Ctrl+SPACE", []uint16{0x11, 0x20}},
		{"shift+alt+f24", "Alt+Shift+F24", []uint16{0x12, 0x10, 0x87}},
		{"Mouse4", "XBUTTON1", []uint16{0x05}},
		{"Ctrl+Mouse5", "Ctrl+XBUTTON2", []uint16{0x11, 0x06}},
		{"·", "OEM_3", []uint16{0xC0}},
		{"`", "OEM_3", []uint16{0xC0}},
		{"~", "OEM_3", []uint16{0xC0}},
		{"ctrl+·", "Ctrl+OEM_3", []uint16{0x11, 0xC0}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			key, err := parseHotkey(tc.input)
			if err != nil || key.Name != tc.name || !reflect.DeepEqual(key.Keys, tc.keys) {
				t.Fatalf("parseHotkey(%q) = %+v, %v", tc.input, key, err)
			}
		})
	}
	for _, input := range []string{"", "Ctrl", "Ctrl+", "V+B", "Ctrl+Control+V", "F25", "未知按键", "Ctrl+Alt+Delete"} {
		if _, err := parseHotkey(input); err == nil {
			t.Errorf("应拒绝无效快捷键 %q", input)
		}
	}
}

// TestRecordedHotkey 验证真实按键码到可保存配置的转换，包含标点和组合键。
func TestRecordedHotkey(t *testing.T) {
	for _, tc := range []struct {
		vk          uint16 // 收到的 Windows 虚拟键码。
		ctrl        bool   // Ctrl 是否按住。
		alt         bool   // Alt 是否按住。
		shift       bool   // Shift 是否按住。
		name        string // 稳定的配置名称。
		displayName string // 展示给用户的键帽文字。
	}{
		{0xC0, false, false, false, "OEM_3", "· / ~"},
		{0xC0, true, false, false, "Ctrl+OEM_3", "Ctrl+· / ~"},
		{0xC0, false, false, true, "Shift+OEM_3", "Shift+· / ~"},
		{0xBB, false, false, false, "OEM_PLUS", "= / +"},
		{0x73, false, true, false, "Alt+F4", "Alt+F4"},
		{0x20, true, false, true, "Ctrl+Shift+SPACE", "Ctrl+Shift+SPACE"},
		{0x05, false, false, false, "XBUTTON1", "Mouse4"},
	} {
		key, err := hotkeyFromVK(tc.vk, tc.ctrl, tc.alt, tc.shift)
		if err != nil || key.Name != tc.name || key.Keys[len(key.Keys)-1] != tc.vk {
			t.Fatalf("录入 0x%X 失败：%+v, %v", tc.vk, key, err)
		}
		if got := hotkeyDisplayName(key.Name); got != tc.displayName {
			t.Fatalf("按键显示=%q，期望=%q", got, tc.displayName)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := saveConfig(path, Config{Hotkey: key.Name}); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path)
		if err != nil || cfg.Hotkey != key.Name {
			t.Fatalf("录入后保存/重启不一致：%+v, %v", cfg, err)
		}
	}
	if _, err := hotkeyFromVK(0x2E, true, true, false); err == nil {
		t.Fatal("不能录入 Windows 保留的 Ctrl+Alt+Delete")
	}
}

// TestHoldGate 验证长按启动、松开、组合键释放和系统恢复后的防误开麦行为。
func TestHoldGate(t *testing.T) {
	gate := holdGate{}
	for i, step := range []struct {
		active   bool // 所有组合键是否按下。
		released bool // 是否全部松开。
		enabled  bool // 设备和系统状态是否允许发言。
		want     bool // 期望是否开麦。
	}{
		{true, false, true, false},  // 启动时已经按住，不能直接开麦。
		{false, false, true, false}, // 只松开组合键的一部分，仍未重新就绪。
		{true, false, true, false},
		{false, true, true, false},
		{true, false, true, true},
		{false, false, true, false}, // 释放任意组合键就静音。
		{true, false, true, true},
		{true, false, false, false}, // 暂停或锁屏时强制静音。
		{true, false, true, false},  // 恢复时仍按住，必须先松开。
		{false, true, true, false},
		{true, false, true, true},
	} {
		if got := gate.update(step.active, step.released, step.enabled); got != step.want {
			t.Fatalf("第 %d 步：开麦=%v，期望=%v", i, got, step.want)
		}
	}
}

// TestConfigRoundTrip 验证首次启动、覆盖保存和损坏配置处理，不触碰真实用户配置。
func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MicPTT", "config.json")
	cfg, err := loadConfig(path)
	if err != nil || cfg.Hotkey != "F8" || cfg.DeviceID != "" || cfg.EnhancementPercent != 100 {
		t.Fatalf("首次启动默认配置不正确：%+v, %v", cfg, err)
	}
	for _, cfg = range []Config{{DeviceID: "设备 A", Hotkey: "v"}, {DeviceID: "设备 B", Hotkey: "ctrl+space"}} {
		if err := saveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		got, err := loadConfig(path)
		key, _ := parseHotkey(cfg.Hotkey)
		if err != nil || got.DeviceID != cfg.DeviceID || got.Hotkey != key.Name {
			t.Fatalf("保存后读取不一致：%+v, %v", got, err)
		}
	}
	if err := saveConfig(path, Config{Hotkey: "Ctrl"}); err == nil {
		t.Fatal("不应保存无主键的配置")
	}
	if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadConfig(path); err == nil || cfg.Hotkey != "F8" {
		t.Fatalf("损坏配置应返回错误及默认设置：%+v, %v", cfg, err)
	}
}

// TestEnhancementConfig 验证旧配置默认不增强、保存比例以及拒绝越界设置。
func TestEnhancementConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"hotkey":"F8"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(path); err != nil || cfg.EnhancementPercent != 100 {
		t.Fatalf("旧配置应默认 100%%：%+v, %v", cfg, err)
	}
	for _, percent := range []int{0, 100, 150, 200} {
		if err := saveConfig(path, Config{Hotkey: "F8", EnhancementPercent: percent}); err != nil {
			t.Fatal(err)
		}
		want := percent
		if want == 0 {
			want = 100
		}
		if cfg, err := loadConfig(path); err != nil || cfg.EnhancementPercent != want {
			t.Fatalf("增强配置未保存：%+v, %v", cfg, err)
		}
	}
	for _, percent := range []int{-1, 99, 201} {
		if err := saveConfig(path, Config{Hotkey: "F8", EnhancementPercent: percent}); err == nil {
			t.Fatalf("不应接受增强比例 %d", percent)
		}
	}
	if err := os.WriteFile(path, []byte(`{"hotkey":"F8","enhancement_percent":201}`), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(path); err == nil || cfg.EnhancementPercent != 100 {
		t.Fatalf("越界配置应报告错误并返回默认设置：%+v, %v", cfg, err)
	}
}
