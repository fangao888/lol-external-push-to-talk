package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是持久化设置；空设备 ID 表示跟随 Windows 默认通信麦克风。
type Config struct {
	DeviceID string `json:"device_id"` // Windows 录音端点 ID，避免同名设备选错。
	Hotkey   string `json:"hotkey"`    // 按住发言的按键或组合键，例如 F8、Ctrl+Space。
}

// configPath 将设置保存在当前用户目录，程序移动后仍可继续使用。
func configPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "MicPTT", "config.json"), nil
}

// loadConfig 首次启动使用 F8；损坏的配置交给界面明确提示。
func loadConfig(path string) (Config, error) {
	cfg := Config{Hotkey: "F8"}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err = json.Unmarshal(data, &cfg); err != nil {
		return Config{Hotkey: "F8"}, fmt.Errorf("配置文件格式错误：%w", err)
	}
	key, err := parseHotkey(cfg.Hotkey)
	if err != nil {
		return Config{Hotkey: "F8"}, err
	}
	cfg.Hotkey = key.Name
	return cfg, nil
}

// saveConfig 先写入同目录临时文件，再替换正式文件，避免留下半份配置。
func saveConfig(path string, cfg Config) error {
	key, err := parseHotkey(cfg.Hotkey)
	if err != nil {
		return err
	}
	cfg.Hotkey = key.Name
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Hotkey 保存标准名称和 Windows 虚拟键码，不会拦截游戏原本的按键。
type Hotkey struct {
	Name string   // 标准化后的组合键文本。
	Keys []uint16 // 必须同时按下的虚拟键码，修饰键排在前面。
}

// namedHotkeys 为录入和配置解析共用的键码表；标点使用稳定的 Windows OEM 键名。
var namedHotkeys = map[string]uint16{
	"SPACE": 0x20, "TAB": 0x09, "ENTER": 0x0D, "ESCAPE": 0x1B,
	"CAPSLOCK": 0x14, "BACKSPACE": 0x08, "INSERT": 0x2D, "DELETE": 0x2E,
	"HOME": 0x24, "END": 0x23, "PAGEUP": 0x21, "PAGEDOWN": 0x22,
	"LEFT": 0x25, "UP": 0x26, "RIGHT": 0x27, "DOWN": 0x28,
	"MIDDLE": 0x04, "XBUTTON1": 0x05, "XBUTTON2": 0x06,
	"OEM_1": 0xBA, "OEM_PLUS": 0xBB, "OEM_COMMA": 0xBC, "OEM_MINUS": 0xBD,
	"OEM_PERIOD": 0xBE, "OEM_2": 0xBF, "OEM_3": 0xC0,
	"OEM_4": 0xDB, "OEM_5": 0xDC, "OEM_6": 0xDD, "OEM_7": 0xDE, "OEM_102": 0xE2,
	"NUMPAD0": 0x60, "NUMPAD1": 0x61, "NUMPAD2": 0x62, "NUMPAD3": 0x63,
	"NUMPAD4": 0x64, "NUMPAD5": 0x65, "NUMPAD6": 0x66, "NUMPAD7": 0x67,
	"NUMPAD8": 0x68, "NUMPAD9": 0x69, "MULTIPLY": 0x6A, "ADD": 0x6B,
	"SUBTRACT": 0x6D, "DECIMAL": 0x6E, "DIVIDE": 0x6F,
	"NUMLOCK": 0x90, "SCROLLLOCK": 0x91, "PAUSE": 0x13, "PRINTSCREEN": 0x2C,
}

// parseHotkey 只接受一个主键以及可选修饰键，避免误把多个主键当成组合键。
func parseHotkey(text string) (Hotkey, error) {
	modifiers := map[string]uint16{"CTRL": 0x11, "ALT": 0x12, "SHIFT": 0x10}
	aliases := map[string]string{
		"CONTROL": "CTRL", "ESC": "ESCAPE", "RETURN": "ENTER", "SPACEBAR": "SPACE", "MOUSE4": "XBUTTON1", "MOUSE5": "XBUTTON2",
		"·": "OEM_3", "`": "OEM_3", "~": "OEM_3", "BACKQUOTE": "OEM_3", "GRAVE": "OEM_3",
	}
	mods := map[string]bool{}
	var mainName string
	var mainKey uint16
	for _, part := range strings.Split(strings.ToUpper(strings.TrimSpace(text)), "+") {
		part = strings.TrimSpace(part)
		if alias, ok := aliases[part]; ok {
			part = alias
		}
		if _, ok := modifiers[part]; ok {
			if mods[part] {
				return Hotkey{}, fmt.Errorf("快捷键包含重复修饰键：%s", part)
			}
			mods[part] = true
			continue
		}
		vk, ok := namedHotkeys[part]
		if len(part) == 1 && ((part[0] >= 'A' && part[0] <= 'Z') || (part[0] >= '0' && part[0] <= '9')) {
			vk, ok = uint16(part[0]), true
		}
		for i := 1; i <= 24; i++ {
			if part == fmt.Sprintf("F%d", i) {
				vk, ok = uint16(0x70+i-1), true
				break
			}
		}
		if !ok || mainName != "" {
			return Hotkey{}, fmt.Errorf("无效快捷键；可填写 F8、V、Ctrl+Space、Mouse4 等")
		}
		mainName, mainKey = part, vk
	}
	if mainName == "" {
		return Hotkey{}, fmt.Errorf("快捷键需要一个主键，例如 F8 或 Ctrl+Space")
	}
	if mods["CTRL"] && mods["ALT"] && mainName == "DELETE" {
		return Hotkey{}, fmt.Errorf("Ctrl+Alt+Delete 由 Windows 保留，请换一个快捷键")
	}
	var result Hotkey
	var names []string
	for _, mod := range []string{"CTRL", "ALT", "SHIFT"} {
		if mods[mod] {
			result.Keys = append(result.Keys, modifiers[mod])
			names = append(names, map[string]string{"CTRL": "Ctrl", "ALT": "Alt", "SHIFT": "Shift"}[mod])
		}
	}
	result.Keys = append(result.Keys, mainKey)
	names = append(names, mainName)
	result.Name = strings.Join(names, "+")
	return result, nil
}

// hotkeyFromVK 按原始虚拟键码生成配置，避免中文输入法输出的字符影响识别。
func hotkeyFromVK(vk uint16, ctrl, alt, shift bool) (Hotkey, error) {
	var name string
	switch {
	case vk >= 'A' && vk <= 'Z', vk >= '0' && vk <= '9':
		name = string(rune(vk))
	case vk >= 0x70 && vk <= 0x87:
		name = fmt.Sprintf("F%d", vk-0x70+1)
	default:
		for candidate, code := range namedHotkeys {
			if code == vk {
				name = candidate
				break
			}
		}
	}
	if name == "" {
		return Hotkey{}, fmt.Errorf("暂不支持该按键，请重新按下其他键")
	}
	if shift {
		name = "Shift+" + name
	}
	if alt {
		name = "Alt+" + name
	}
	if ctrl {
		name = "Ctrl+" + name
	}
	return parseHotkey(name)
}

// hotkeyDisplayName 将内部键名换成键帽上的符号，配置仍保存稳定的 OEM 名称。
func hotkeyDisplayName(name string) string {
	parts := strings.Split(name, "+")
	display := map[string]string{
		"OEM_1": "; / :", "OEM_PLUS": "= / +", "OEM_COMMA": ", / <", "OEM_MINUS": "- / _",
		"OEM_PERIOD": ". / >", "OEM_2": "/ / ?", "OEM_3": "· / ~",
		"OEM_4": "[ / {", "OEM_5": "\\ / |", "OEM_6": "] / }", "OEM_7": "' / \"",
		"XBUTTON1": "Mouse4", "XBUTTON2": "Mouse5", "MIDDLE": "鼠标中键",
	}
	for i, part := range parts {
		if text, ok := display[part]; ok {
			parts[i] = text
		}
	}
	return strings.Join(parts, "+")
}

// holdGate 要求初始化、恢复或换设备后先松开所有相关按键，防止意外开麦。
type holdGate struct {
	armed bool // 只有观察到全部松开后，才允许下一次按住发言。
}

// update 在组合键任意一个键松开、暂停或失去设备时立即关闭发言。
func (g *holdGate) update(active, allReleased, enabled bool) bool {
	if !enabled {
		g.armed = false
		return false
	}
	if allReleased {
		g.armed = true
	}
	return g.armed && active
}
