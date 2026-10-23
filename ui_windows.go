package main

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"strings"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// Windows 会话、电源与本程序的定时器标识。
const (
	wmSessionChange = 0x02B1
	uiTimerID       = 0x4D50
	blockSession    = 1
	blockSuspend    = 2
	blockShutdown   = 4
	blockRecording  = 8
	captureTimerID  = 0x4D51
)

// appWindow 把设置、托盘和系统通知连接到音频控制线程。
type appWindow struct {
	*walk.MainWindow                        // 原生 Windows 主窗口与消息循环。
	engine            *controller           // 独立于界面的麦克风控制线程。
	path              string                // 当前用户的配置文件路径。
	deviceBox         *walk.ComboBox        // 麦克风设备下拉框。
	hotkeyEdit        *walk.LineEdit        // 只读快捷键显示框，点击后按键录入。
	recordButton      *walk.PushButton      // 开始或取消按键录入。
	hotkeyName        string                // 未保存的标准快捷键名称，与显示文本分离。
	enhancementSlider *walk.Slider          // 增强滑动条，最左为 100%，最右为 200%。
	enhancementLabel  *walk.Label           // 待保存的增强百分比。
	enhancementStatus *walk.Label           // 当前设备实际应用的增强比例。
	recording         bool                  // 是否正在录入，录入期间禁止开麦。
	mouseKeysDown     [3]bool               // 鼠标中键和侧键的前次状态，仅在录入时使用。
	statusLabel       *walk.Label           // 当前是否正在发言。
	deviceLabel       *walk.TextLabel       // 实际使用的麦克风名称。
	errorLabel        *walk.TextLabel       // 音频异常和需要用户处理的提示。
	pauseButton       *walk.PushButton      // 暂停期间保持静音。
	tray              *walk.NotifyIcon      // 系统托盘入口。
	pauseAction       *walk.Action          // 托盘中的暂停菜单项。
	icons             map[string]*walk.Icon // 各种状态的图标，关闭托盘后统一释放。
	deviceIDs         []string              // 下拉框索引与稳定设备 ID 的对应关系。
	deviceStamp       string                // 避免每次刷新状态都重置设备下拉框。
	iconState         string                // 避免重复设置相同托盘图标。
	closing           bool                  // 真正退出时允许销毁窗口。
}

// newAppWindow 只创建界面，不打开麦克风，方便进行无副作用的界面验证。
func newAppWindow(cfg Config, path string) (*appWindow, error) {
	cfg.EnhancementPercent, _ = normalizeEnhancement(cfg.EnhancementPercent)
	a := &appWindow{path: path, hotkeyName: cfg.Hotkey, icons: make(map[string]*walk.Icon)}
	if err := (MainWindow{
		AssignTo: &a.MainWindow,
		Title:    "按键发言工具",
		Size:     Size{Width: 620, Height: 595},
		MinSize:  Size{Width: 580, Height: 565},
		Font:     Font{Family: "Microsoft YaHei UI", PointSize: 10},
		Layout:   VBox{Margins: Margins{Left: 22, Top: 20, Right: 22, Bottom: 18}, Spacing: 12},
		Children: []Widget{
			Label{Text: "按住说话，松开静音", Font: Font{PointSize: 17, Bold: true}},
			Label{AssignTo: &a.statusLabel, Text: "正在连接麦克风…", Font: Font{PointSize: 13, Bold: true}},
			TextLabel{AssignTo: &a.deviceLabel, Text: "麦克风：等待连接", MinSize: Size{Width: 1, Height: 32}},
			GroupBox{Title: "发言设置", Layout: VBox{Spacing: 8}, Children: []Widget{
				Label{Text: "控制的麦克风（需要与英雄联盟中选择的设备一致）"},
				ComboBox{AssignTo: &a.deviceBox, Model: []string{"跟随 Windows 默认通信麦克风"}, CurrentIndex: 0},
				Label{Text: "按住发言的快捷键"},
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					LineEdit{AssignTo: &a.hotkeyEdit, Text: hotkeyDisplayName(cfg.Hotkey), ReadOnly: true, StretchFactor: 1},
					PushButton{AssignTo: &a.recordButton, Text: "录入快捷键", OnClicked: a.toggleHotkeyRecording},
				}},
				TextLabel{Text: "点击录入后，直接按下需要的按键或组合键，支持 · / ~ 键。快捷键仍会传给游戏。", MinSize: Size{Width: 1}},
				Label{AssignTo: &a.enhancementLabel, Text: enhancementText(cfg.EnhancementPercent)},
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					Label{Text: "100%"},
					Slider{AssignTo: &a.enhancementSlider, MinValue: 100, MaxValue: 200, Value: cfg.EnhancementPercent, Tracking: true, StretchFactor: 1, OnValueChanged: a.updateEnhancementLabel},
					Label{Text: "200%"},
				}},
				Label{AssignTo: &a.enhancementStatus, Text: "100% 表示不增强，调整后点击“保存并应用”。"},
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					PushButton{Text: "保存并应用", OnClicked: a.applySettings},
					PushButton{Text: "刷新麦克风", OnClicked: a.refreshDevices},
					HSpacer{},
				}},
			}},
			TextLabel{AssignTo: &a.errorLabel, Text: "", MinSize: Size{Width: 1, Height: 50}, TextColor: walk.RGB(175, 62, 35)},
			TextLabel{Text: "LOL 内将语音设为自由发言，由本工具控制实际发言。系统静音会同时影响使用该麦克风的其他软件。", MinSize: Size{Width: 1}},
			VSpacer{},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				PushButton{AssignTo: &a.pauseButton, Text: "暂停（保持静音）", OnClicked: a.togglePause},
				PushButton{Text: "收起到托盘", OnClicked: func() { a.Hide() }},
				HSpacer{},
				PushButton{Text: "退出（恢复麦克风）", OnClicked: a.quit},
			}},
		},
	}).Create(); err != nil {
		return nil, err
	}
	if err := walk.InitWrapperWindow(a); err != nil {
		a.Dispose()
		return nil, err
	}
	if err := a.installHotkeyInput(); err != nil {
		a.Dispose()
		return nil, err
	}
	a.setDevices(nil, cfg.DeviceID)
	a.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if !a.closing {
			*canceled = true
			a.finishHotkeyRecording(nil)
			a.Hide()
		}
	})
	a.Deactivating().Attach(func() { a.finishHotkeyRecording(nil) })
	return a, nil
}

// enhancementText 标出百分比，并明确最左侧 100% 不改变原始音量。
func enhancementText(percent int) string {
	if percent == 100 {
		return "麦克风增强：100%（不增强）"
	}
	return fmt.Sprintf("麦克风增强：%d%%", percent)
}

// updateEnhancementLabel 只更新待保存的显示，音量由音频线程在应用设置后修改。
func (a *appWindow) updateEnhancementLabel() {
	if a.enhancementSlider != nil && a.enhancementLabel != nil {
		_ = a.enhancementLabel.SetText(enhancementText(a.enhancementSlider.Value()))
	}
}

// createTray 建立始终可用的托盘入口，左键打开设置，右键暂停或退出。
func (a *appWindow) createTray() error {
	for name, c := range map[string]color.RGBA{
		"muted": {R: 196, G: 58, B: 67, A: 255},
		"speak": {R: 25, G: 150, B: 91, A: 255},
		"pause": {R: 103, G: 115, B: 136, A: 255},
		"error": {R: 201, G: 132, B: 24, A: 255},
	} {
		icon, err := microphoneIcon(c)
		if err != nil {
			return err
		}
		a.icons[name] = icon
	}
	var err error
	a.tray, err = walk.NewNotifyIcon(a.MainWindow)
	if err != nil {
		return err
	}
	if err = a.SetIcon(a.icons["muted"]); err != nil {
		return err
	}
	if err = a.tray.SetIcon(a.icons["muted"]); err != nil {
		return err
	}
	if err = a.tray.SetToolTip("按键发言工具：正在连接"); err != nil {
		return err
	}
	a.tray.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			a.showSettings()
		}
	})
	for _, item := range []struct {
		text string        // 菜单显示文字。
		fn   func()        // 在界面线程执行的菜单动作。
		keep **walk.Action // 可选的菜单项引用，用于更新暂停文字。
	}{
		{text: "打开设置", fn: a.showSettings},
		{text: "暂停（保持静音）", fn: a.togglePause, keep: &a.pauseAction},
		{text: "退出（恢复麦克风）", fn: a.quit},
	} {
		action := walk.NewAction()
		if err = action.SetText(item.text); err != nil {
			return err
		}
		action.Triggered().Attach(item.fn)
		if err = a.tray.ContextMenu().Actions().Add(action); err != nil {
			return err
		}
		if item.keep != nil {
			*item.keep = action
		}
	}
	return a.tray.SetVisible(true)
}

// disposeResources 在主窗口销毁前释放托盘与图标句柄。
func (a *appWindow) disposeResources() {
	if a.tray != nil {
		_ = a.tray.Dispose()
	}
	for _, icon := range a.icons {
		icon.Dispose()
	}
}

// showSettings 从托盘恢复主窗口，同时保留正在执行的按键控制。
func (a *appWindow) showSettings() {
	a.Show()
	win.ShowWindow(a.Handle(), win.SW_RESTORE)
	_ = a.Activate()
}

// showError 将实际操作失败同时写入日志和展示给用户。
func (a *appWindow) showError(err error) {
	log.Print(err)
	walk.MsgBox(a.MainWindow, "按键发言工具", err.Error(), walk.MsgBoxIconError)
}

// selectedDevice 返回下拉框当前选择，设备未连接时也保留其稳定 ID。
func (a *appWindow) selectedDevice() string {
	index := a.deviceBox.CurrentIndex()
	if index >= 0 && index < len(a.deviceIDs) {
		return a.deviceIDs[index]
	}
	return ""
}

// setDevices 更新设备列表并保留当前选择，避免因刷新默默切换麦克风。
func (a *appWindow) setDevices(devices []Device, selected string) {
	names := []string{"跟随 Windows 默认通信麦克风"}
	ids := []string{""}
	index := 0
	for _, device := range devices {
		ids = append(ids, device.ID)
		names = append(names, device.Name)
		if device.ID == selected {
			index = len(ids) - 1
		}
	}
	if selected != "" && index == 0 {
		ids = append(ids, selected)
		names = append(names, "[未连接] 已保存的麦克风（可重新选择）")
		index = len(ids) - 1
	}
	a.deviceIDs = ids
	_ = a.deviceBox.SetModel(names)
	_ = a.deviceBox.SetCurrentIndex(index)
}

// applySettings 在保存前校验快捷键；应用设置会先静音并重新等待松键。
func (a *appWindow) applySettings() {
	a.finishHotkeyRecording(nil)
	key, err := parseHotkey(a.hotkeyName)
	if err != nil {
		a.showError(err)
		return
	}
	cfg := Config{DeviceID: a.selectedDevice(), Hotkey: key.Name, EnhancementPercent: a.enhancementSlider.Value()}
	if err = saveConfig(a.path, cfg); err != nil {
		a.showError(fmt.Errorf("保存设置失败：%w", err))
		return
	}
	_ = a.hotkeyEdit.SetText(hotkeyDisplayName(key.Name))
	if a.engine != nil {
		if err = a.engine.request(controlCommand{kind: "apply", config: cfg}); err != nil {
			a.showError(err)
		}
	}
	_ = a.errorLabel.SetText("设置已保存。先松开全部快捷键，再按住即可发言。")
}

// refreshDevices 重新枚举活动端点，并检查设备是否已重新连接。
func (a *appWindow) refreshDevices() {
	if a.engine != nil {
		if err := a.engine.request(controlCommand{kind: "refresh"}); err != nil {
			a.showError(err)
		}
	}
}

// togglePause 暂停只会静音；恢复后需要先松键再重新按住。
func (a *appWindow) togglePause() {
	if a.engine != nil {
		if err := a.engine.request(controlCommand{kind: "pause"}); err != nil {
			a.showError(err)
		}
		a.updateStatus()
	}
}

// quit 等到音频线程解除静音后才真正退出，失败则保留界面供用户处理。
func (a *appWindow) quit() {
	if a.engine != nil {
		if err := a.engine.stop(); err != nil {
			a.showError(fmt.Errorf("退出前恢复麦克风失败，请检查系统麦克风后重试：%w", err))
			return
		}
	}
	a.closing = true
	walk.App().Exit(0)
}

// updateStatus 在 UI 线程更新文字与托盘，音频控制不依赖此定时器。
func (a *appWindow) updateStatus() {
	if a.engine == nil {
		return
	}
	s := a.engine.snapshot.Load()
	var stamp strings.Builder
	for _, device := range s.Devices {
		stamp.WriteString(device.ID + "\x00" + device.Name + "\x00")
	}
	if stamp.String() != a.deviceStamp {
		a.setDevices(s.Devices, a.selectedDevice())
		a.deviceStamp = stamp.String()
	}
	keyName := hotkeyDisplayName(s.Config.Hotkey)
	text, iconState := "已静音 · 按住 "+keyName+" 发言", "muted"
	c := walk.RGB(174, 49, 60)
	switch {
	case s.Error != "":
		text, iconState, c = "麦克风异常 · 请检查下方提示", "error", walk.RGB(173, 112, 20)
	case !s.Ready:
		text, iconState = "正在连接麦克风…", "pause"
	case a.recording:
		text, iconState = "正在录入快捷键 · 麦克风保持静音", "pause"
	case s.Blocked:
		text, iconState = "系统会话暂停 · 保持静音", "pause"
	case s.Paused:
		text, iconState = "已暂停 · 麦克风保持静音", "pause"
	case s.Speaking:
		text, iconState, c = "正在发言 · 松开 "+keyName+" 静音", "speak", walk.RGB(22, 132, 78)
	}
	_ = a.statusLabel.SetText(text)
	a.statusLabel.SetTextColor(c)
	deviceName := s.Device
	if deviceName == "" {
		deviceName = "等待连接"
	}
	_ = a.deviceLabel.SetText("麦克风：" + deviceName)
	message := s.Error
	if s.EnhancementWarning != "" {
		if message != "" {
			message += "\n"
		}
		message += s.EnhancementWarning
	}
	_ = a.errorLabel.SetText(message)
	enhancementStatus := "等待确认麦克风增强状态"
	if s.EnhancementActual > 0 && s.Ready {
		enhancementStatus = fmt.Sprintf("已应用：%d%%，设备实际约 %.0f%%", s.Config.EnhancementPercent, s.EnhancementActual)
	}
	_ = a.enhancementStatus.SetText(enhancementStatus)
	pauseText := "暂停（保持静音）"
	if s.Paused {
		pauseText = "恢复按键发言"
	}
	_ = a.pauseButton.SetText(pauseText)
	if a.pauseAction != nil {
		_ = a.pauseAction.SetText(pauseText)
	}
	if a.tray != nil {
		_ = a.tray.SetToolTip("按键发言工具：" + text)
		if a.iconState != iconState {
			_ = a.tray.SetIcon(a.icons[iconState])
			a.iconState = iconState
		}
	}
}

// safety 在收到锁屏、电源或关闭系统消息时，同步完成静音再返回 Windows。
func (a *appWindow) safety(flag uint32, on bool) {
	if a.engine != nil {
		if err := a.engine.request(controlCommand{kind: "safety", flag: flag, on: on}); err != nil {
			log.Printf("处理系统状态变化失败：%v", err)
		}
	}
}

// WndProc 在 Windows 主消息循环中处理会话和电源通知。
func (a *appWindow) WndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case win.WM_TIMER:
		if wParam == captureTimerID {
			a.captureMouseHotkey()
			return 0
		}
		if wParam == uiTimerID {
			a.updateStatus()
			return 0
		}
	case wmSessionChange:
		switch wParam {
		case 2, 4, 6, 7: // 控制台/远程断开、注销、锁屏。
			a.safety(blockSession, true)
		case 1, 3, 8: // 控制台/远程连接、解锁。
			a.safety(blockSession, false)
		}
	case win.WM_POWERBROADCAST:
		switch wParam {
		case 4: // PBT_APMSUSPEND。
			a.safety(blockSuspend, true)
		case 6, 7, 18: // 从待机或休眠恢复。
			a.safety(blockSuspend, false)
		}
		return 1
	case win.WM_QUERYENDSESSION:
		a.safety(blockShutdown, true)
		return 1
	case win.WM_ENDSESSION:
		if wParam == 0 { // 用户取消了系统关闭，重新允许按键控制。
			a.safety(blockShutdown, false)
		}
	}
	return a.MainWindow.WndProc(hwnd, msg, wParam, lParam)
}

// microphoneIcon 用原生位图绘制麦克风图标，运行时无需外部图片文件。
func microphoneIcon(background color.RGBA) (*walk.Icon, error) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			dx, dy := x-32, y-32
			if dx*dx+dy*dy <= 31*31 {
				img.SetRGBA(x, y, background)
			}
			// 胶囊形话筒、下方 U 形支架以及底座。
			top, bottom := y-19, y-30
			body := (dx*dx+top*top <= 49) || (dx*dx+bottom*bottom <= 49) || (x >= 25 && x <= 39 && y >= 19 && y <= 30)
			arcY := y - 30
			dist := dx*dx + arcY*arcY
			arc := y >= 30 && dist >= 14*14 && dist <= 17*17
			sides := ((x >= 15 && x <= 18) || (x >= 46 && x <= 49)) && y >= 25 && y <= 30
			stem := x >= 30 && x <= 34 && y >= 46 && y <= 54
			base := x >= 23 && x <= 41 && y >= 53 && y <= 56
			if body || arc || sides || stem || base {
				img.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}
	return walk.NewIconFromImageForDPI(img, 96)
}
