package main

import (
	"fmt"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// hotkeyInput 在只读输入框中接收原始键盘消息，避免 Tab、Enter 被界面导航消费。
type hotkeyInput struct {
	*walk.LineEdit            // 原生只读输入框，保留正常的焦点和无障碍支持。
	owner          *appWindow // 录入状态和配置由主窗口持有。
}

// installHotkeyInput 安装录入消息处理，并只对这个控件禁用输入法字符转换。
func (a *appWindow) installHotkeyInput() error {
	input := &hotkeyInput{LineEdit: a.hotkeyEdit, owner: a}
	if err := walk.InitWrapperWindow(input); err != nil {
		return err
	}
	windows.NewLazySystemDLL("imm32.dll").NewProc("ImmAssociateContext").Call(uintptr(input.Handle()), 0)
	input.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton && !a.recording {
			a.toggleHotkeyRecording()
		}
	})
	return nil
}

// toggleHotkeyRecording 先同步静音再接收按键，取消时保留之前的快捷键。
func (a *appWindow) toggleHotkeyRecording() {
	if a.recording {
		a.finishHotkeyRecording(nil)
		return
	}
	if a.engine != nil {
		if err := a.engine.request(controlCommand{kind: "safety", flag: blockRecording, on: true}); err != nil {
			a.showError(err)
			return
		}
	}
	a.recording = true
	for i, vk := range []uint16{0x04, 0x05, 0x06} {
		state, _, _ := getAsyncKeyState.Call(uintptr(vk))
		a.mouseKeysDown[i] = state&0x8000 != 0
	}
	if win.SetTimer(a.Handle(), captureTimerID, 10, 0) == 0 {
		a.finishHotkeyRecording(nil)
		a.showError(fmt.Errorf("无法开始快捷键录入，请重试"))
		return
	}
	_ = a.recordButton.SetText("取消录入")
	_ = a.hotkeyEdit.SetText("请按下按键或组合键…")
	_ = a.hotkeyEdit.SetFocus()
	a.updateStatus()
}

// finishHotkeyRecording 完成或取消录入；解除录入阻止后仍需松键才可重新发言。
func (a *appWindow) finishHotkeyRecording(key *Hotkey) {
	if !a.recording {
		return
	}
	a.recording = false
	win.KillTimer(a.Handle(), captureTimerID)
	if key != nil {
		a.hotkeyName = key.Name
	}
	_ = a.hotkeyEdit.SetText(hotkeyDisplayName(a.hotkeyName))
	_ = a.recordButton.SetText("录入快捷键")
	a.safety(blockRecording, false)
	a.updateStatus()
}

// recordVirtualKey 直接保存虚拟键码，不依赖按键产生的中文、英文或标点字符。
func (a *appWindow) recordVirtualKey(vk uint16) {
	if !a.recording {
		return
	}
	// 单独的修饰键继续等待主键，左右修饰键都归一化处理。
	switch vk {
	case 0x10, 0x11, 0x12, 0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5:
		return
	}
	if win.GetKeyState(0x5B) < 0 || win.GetKeyState(0x5C) < 0 {
		_ = a.hotkeyEdit.SetText("请使用 Ctrl / Alt / Shift 组合键")
		return
	}
	key, err := hotkeyFromVK(vk, win.GetKeyState(0x11) < 0, win.GetKeyState(0x12) < 0, win.GetKeyState(0x10) < 0)
	if err != nil {
		_ = a.hotkeyEdit.SetText(err.Error())
		return
	}
	a.finishHotkeyRecording(&key)
}

// captureMouseHotkey 允许鼠标停留在录入按钮上时按中键或侧键，不改变其他鼠标操作。
func (a *appWindow) captureMouseHotkey() {
	if !a.recording {
		return
	}
	for i, vk := range []uint16{0x04, 0x05, 0x06} {
		state, _, _ := getAsyncKeyState.Call(uintptr(vk))
		down := state&0x8000 != 0
		previous := a.mouseKeysDown[i]
		a.mouseKeysDown[i] = down
		if down && !previous {
			a.recordVirtualKey(vk)
			return
		}
	}
}

// WndProc 在录入时接收完整按键，阻止字符输入及 Alt 快捷键触发窗口行为。
func (input *hotkeyInput) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	a := input.owner
	switch msg {
	case win.WM_GETDLGCODE:
		if a.recording {
			return win.DLGC_WANTALLKEYS | win.DLGC_WANTTAB | win.DLGC_WANTARROWS | win.DLGC_WANTCHARS
		}
	case win.WM_KEYDOWN, win.WM_SYSKEYDOWN:
		if a.recording {
			if lParam&(1<<30) == 0 { // 忽略长按重复，避免把开始录入之前的旧按住状态录进去。
				a.recordVirtualKey(uint16(wParam))
			}
			return 0
		}
	case win.WM_CHAR, win.WM_SYSCHAR, win.WM_DEADCHAR, win.WM_SYSDEADCHAR, win.WM_SYSKEYUP:
		return 0 // 显示框只展示已录入的快捷键，不接受字符编辑或系统菜单激活。
	case win.WM_KILLFOCUS:
		if a.recordButton != nil && win.HWND(wParam) != a.recordButton.Handle() {
			a.finishHotkeyRecording(nil)
		}
	}
	return input.LineEdit.WndProc(hwnd, msg, wParam, lParam)
}
