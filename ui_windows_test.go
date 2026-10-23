package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// TestNativeSettingsSmoke 仅在显式启用时验证原生窗口和托盘，全程不控制真实麦克风。
func TestNativeSettingsSmoke(t *testing.T) {
	if os.Getenv("MICPTT_UI_TEST") != "1" {
		t.Skip("设置 MICPTT_UI_TEST=1 运行原生界面烟雾测试")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	a, err := newAppWindow(Config{Hotkey: "F8", DeviceID: "saved-device"}, filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Dispose()
	defer a.disposeResources()
	if a.selectedDevice() != "saved-device" {
		t.Fatal("界面必须保留未连接的设备 ID，不能静默改选默认设备")
	}
	if err := a.createTray(); err != nil {
		t.Fatal(err)
	}
	// 直接向本程序控件发送原生消息，不向系统或其他应用模拟键盘输入。
	for _, vk := range []uintptr{0x09, 0x0D, 0x1B, 0xBB, 0xC0} {
		a.toggleHotkeyRecording()
		if flags := a.hotkeyEdit.SendMessage(win.WM_GETDLGCODE, 0, 0); flags&win.DLGC_WANTALLKEYS == 0 {
			t.Fatal("录入时必须接收 Tab、Enter 等完整键盘消息")
		}
		a.hotkeyEdit.SendMessage(win.WM_KEYDOWN, vk, 1)
		key, _ := parseHotkey(a.hotkeyName)
		if a.recording || key.Keys[len(key.Keys)-1] != uint16(vk) {
			t.Fatalf("原生按键消息未正确录入：vk=0x%X, hotkey=%q", vk, a.hotkeyName)
		}
	}
	if a.hotkeyEdit.Text() != "· / ~" {
		t.Fatalf("· 键展示错误：%q", a.hotkeyEdit.Text())
	}
	a.toggleHotkeyRecording()
	a.hotkeyEdit.SendMessage(win.WM_KEYDOWN, 0x10, 1)
	if !a.recording {
		t.Fatal("只按修饰键时应等待主键")
	}
	a.toggleHotkeyRecording()
	if a.hotkeyName != "OEM_3" {
		t.Fatal("取消录入不能覆盖之前的快捷键")
	}
	a.applySettings()
	if cfg, err := loadConfig(a.path); err != nil || cfg.Hotkey != "OEM_3" {
		t.Fatalf("录入后应用保存失败：%+v, %v", cfg, err)
	}
	// 发布虚拟状态，检查实际使用的状态文字与设备列表更新路径。
	c := &controller{}
	c.snapshot.Store(&Snapshot{Config: Config{Hotkey: "F8"}, Ready: true, Device: "测试麦克风", Devices: []Device{{ID: "test", Name: "测试麦克风"}}})
	a.engine = c
	a.updateStatus()
	a.engine = nil
	if a.statusLabel.Text() != "已静音 · 按住 F8 发言" {
		t.Fatalf("状态显示错误：%s", a.statusLabel.Text())
	}
	var captureErr error
	time.AfterFunc(700*time.Millisecond, func() {
		a.Synchronize(func() {
			// 可选截图只读取本程序窗口，供人工检查布局。
			if path := os.Getenv("MICPTT_SCREENSHOT"); path != "" {
				captureErr = captureWindow(a.MainWindow, path)
			}
			a.quit()
		})
	})
	a.Show()
	_ = a.Activate()
	a.Run()
	if captureErr != nil {
		t.Fatal(captureErr)
	}
}

// captureWindow 只截取指定窗口的客户区，并写入测试指定的 PNG 路径。
func captureWindow(window walk.Window, path string) error {
	size := window.ClientBoundsPixels().Size()
	bitmap, err := walk.NewBitmapForDPI(size, window.DPI())
	if err != nil {
		return err
	}
	defer bitmap.Dispose()
	canvas, err := walk.NewCanvasFromImage(bitmap)
	if err != nil {
		return err
	}
	dc := win.GetDC(window.Handle())
	ok := win.BitBlt(canvas.HDC(), 0, 0, int32(size.Width), int32(size.Height), dc, 0, 0, win.SRCCOPY)
	win.ReleaseDC(window.Handle(), dc)
	canvas.Dispose()
	if !ok {
		return fmt.Errorf("复制窗口客户区失败")
	}
	img, err := bitmap.ToImage()
	if err != nil {
		return err
	}
	// GDI 的屏幕位图没有有效 alpha 通道，PNG 必须显式设置为不透明。
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return png.Encode(file, img)
}
