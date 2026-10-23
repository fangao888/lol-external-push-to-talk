// 按键发言工具使用系统录音端点静音，让游戏保持语音连接但只在按住时发送声音。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// main 保证界面创建与消息循环始终运行在同一 Windows 线程。
func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := run(); err != nil {
		log.Print(err)
		win.MessageBox(0, windows.StringToUTF16Ptr(err.Error()), windows.StringToUTF16Ptr("按键发言工具"), win.MB_OK|win.MB_ICONERROR)
	}
}

// run 初始化配置、单实例、托盘和系统通知，界面准备完成后才接管麦克风。
func run() error {
	diagnostic := flag.Bool("diagnose", false, "只读列出录音设备及所选设备静音状态，不启动控制")
	flag.Parse()
	path, err := configPath()
	if err != nil {
		return fmt.Errorf("无法获取配置目录：%w", err)
	}
	cfg, configErr := loadConfig(path)
	if *diagnostic {
		return diagnose(cfg)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(filepath.Dir(path), "app.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("无法打开日志文件：%w", err)
	}
	defer file.Close()
	log.SetOutput(file)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	mutex, mutexErr := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr("Local\\MicPTT.Singleton.v1"))
	if mutex != 0 {
		defer windows.CloseHandle(mutex)
	}
	if mutexErr == windows.ERROR_ALREADY_EXISTS {
		if hwnd := win.FindWindow(nil, windows.StringToUTF16Ptr("按键发言工具")); hwnd != 0 {
			win.ShowWindow(hwnd, win.SW_RESTORE)
			win.SetForegroundWindow(hwnd)
			return nil
		}
		return fmt.Errorf("按键发言工具已在运行，请从系统托盘打开")
	}
	if mutexErr != nil {
		return fmt.Errorf("创建单实例锁失败：%w", mutexErr)
	}
	a, err := newAppWindow(cfg, path)
	if err != nil {
		return fmt.Errorf("创建设置窗口失败：%w", err)
	}
	defer a.Dispose()
	defer a.disposeResources()
	if err = a.createTray(); err != nil {
		return fmt.Errorf("创建系统托盘失败：%w", err)
	}
	register := windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSRegisterSessionNotification")
	unregister := windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSUnRegisterSessionNotification")
	if ok, _, callErr := register.Call(uintptr(a.Handle()), 0); ok == 0 {
		return fmt.Errorf("注册锁屏通知失败：%w", callErr)
	}
	defer unregister.Call(uintptr(a.Handle()))
	if win.SetTimer(a.Handle(), uiTimerID, 100, 0) == 0 {
		return fmt.Errorf("创建界面状态定时器失败")
	}
	defer win.KillTimer(a.Handle(), uiTimerID)
	if configErr != nil {
		walk.MsgBox(a.MainWindow, "配置读取提示", configErr.Error()+"\n本次暂用默认设置 F8，请重新选择麦克风并保存。", walk.MsgBoxIconWarning)
	}
	a.engine = newController(cfg)
	defer a.engine.stop()
	log.Printf("启动：快捷键=%s", cfg.Hotkey)
	a.Show()
	a.Run()
	log.Print("程序正常退出，麦克风保持静音")
	return nil
}

// diagnose 仅读取设备与静音状态，供排查音频问题或验证 Windows API 使用。
func diagnose(cfg Config) error {
	audio, err := openAudio()
	if err != nil {
		return err
	}
	defer audio.close()
	devices, err := audio.devices()
	if err != nil {
		return err
	}
	result := struct {
		Devices  []Device `json:"devices"`         // 当前可用的录音设备。
		Selected *Device  `json:"selected"`        // 实际选择的端点；无设备时为空。
		Muted    *bool    `json:"muted"`           // 当前系统静音状态；读取失败时为空。
		Error    string   `json:"error,omitempty"` // 选择或读取失败的原因。
	}{Devices: devices}
	mic, err := audio.endpoint(cfg.DeviceID)
	if err != nil {
		result.Error = err.Error()
	} else {
		defer mic.close()
		device := mic.info()
		result.Selected = &device
		muted, readErr := mic.muted()
		if readErr != nil {
			result.Error = readErr.Error()
		} else {
			result.Muted = &muted
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
