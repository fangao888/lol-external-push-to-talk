package main

import (
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// 每 10ms 读取一次物理按键状态；无需向游戏注入代码或安装键盘钩子。
var getAsyncKeyState = windows.NewLazySystemDLL("user32.dll").NewProc("GetAsyncKeyState")

// Snapshot 是不可变的界面快照，音频线程发布后不再修改其中的数据。
type Snapshot struct {
	Config   Config   // 当前生效的设置。
	Devices  []Device // 最近一次枚举得到的可用麦克风。
	Device   string   // 实际控制的麦克风名称。
	Speaking bool     // 已确认解除静音，并且快捷键仍按住。
	Paused   bool     // 用户是否暂停按键发言，暂停期间保持静音。
	Blocked  bool     // 锁屏、休眠或关闭系统期间禁止开麦。
	Ready    bool     // 音频设备是否已成功连接。
	Error    string   // 最近的音频错误，空字符串表示正常。
}

// controlCommand 将所有 COM 操作串行交给音频线程，避免跨线程使用接口。
type controlCommand struct {
	kind   string     // apply、pause、refresh、safety 或 stop。
	config Config     // apply 命令携带的新设置。
	flag   uint32     // safety 命令对应的阻止标志。
	on     bool       // 是否设置阻止标志。
	reply  chan error // 命令执行结果；缓冲区避免界面关闭后阻塞音频线程。
}

// controller 拥有独立音频线程，界面卡顿时也会继续检测松键。
type controller struct {
	commands chan controlCommand      // 用户操作和系统安全事件队列。
	done     chan struct{}            // 音频线程完成清理后关闭。
	snapshot atomic.Pointer[Snapshot] // 供界面读取的最新状态。
}

// newController 启动控制循环；初始状态保持静音，等待用户完整按下一次快捷键。
func newController(cfg Config) *controller {
	c := &controller{commands: make(chan controlCommand, 8), done: make(chan struct{})}
	c.snapshot.Store(&Snapshot{Config: cfg})
	go c.run(cfg)
	return c
}

// request 同步等待命令生效，退出后立即返回，避免向已停止的线程发送操作。
func (c *controller) request(cmd controlCommand) error {
	cmd.reply = make(chan error, 1)
	select {
	case c.commands <- cmd:
	case <-c.done:
		return fmt.Errorf("音频控制线程已停止，请重新启动程序")
	}
	select {
	case err := <-cmd.reply:
		return err
	case <-c.done:
		select {
		case err := <-cmd.reply:
			return err
		default:
			return fmt.Errorf("音频控制线程已停止")
		}
	}
}

// stop 在正常退出时等待静音完成，不直接终止音频线程。
func (c *controller) stop() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	err := c.request(controlCommand{kind: "stop"})
	if err == nil {
		<-c.done
	}
	return err
}

// keyState 使用 GetAsyncKeyState 的高位读取当前状态，自动规避漏收松键消息。
func keyState(key Hotkey) (active, allReleased bool) {
	active, allReleased = true, true
	for _, vk := range key.Keys {
		state, _, _ := getAsyncKeyState.Call(uintptr(vk))
		down := state&0x8000 != 0
		active = active && down
		allReleased = allReleased && !down
	}
	return
}

// run 将 COM 的生命周期绑定到同一 OS 线程。
func (c *controller) run(cfg Config) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(c.done)
	audio, err := openAudio()
	if err != nil {
		c.snapshot.Store(&Snapshot{Config: cfg, Error: "初始化 Windows 音频失败：" + err.Error()})
		return
	}
	defer audio.close()
	c.control(cfg, audio, keyState)
}

// control 串行执行所有静音操作；注入设备和按键来源可验证异常与恢复流程。
func (c *controller) control(cfg Config, audio audioBackend, readKeys func(Hotkey) (bool, bool)) {
	state := Snapshot{Config: cfg}
	var mic audioEndpoint
	// 即使普通错误或 panic 让循环退出，也尽力把已打开的端点恢复为静音。
	defer func() {
		if mic != nil {
			if err := mic.setMute(true); err != nil {
				log.Printf("退出时静音失败：%v", err)
			}
			mic.close()
		}
		if recovered := recover(); recovered != nil {
			state.Speaking, state.Ready = false, false
			state.Error = fmt.Sprintf("控制线程异常：%v，请重启程序并检查系统静音状态", recovered)
			c.snapshot.Store(&state)
			log.Print(state.Error)
		}
	}()
	key, _ := parseHotkey(cfg.Hotkey)
	gate := holdGate{}
	var blockFlags uint32
	var lastTick, nextDeviceCheck, nextStateCheck, nextPublish time.Time
	var lastMute = true
	var refreshDevices = true
	// publish 复制状态，防止界面读快照时与控制线程写入发生数据竞争。
	publish := func() {
		copy := state
		c.snapshot.Store(&copy)
	}
	// fail 强制重新等待松键；设备错误时绝不沿用之前的发言状态。
	fail := func(err error) {
		if state.Error != err.Error() {
			log.Print(err)
		}
		state.Error, state.Speaking, state.Ready = err.Error(), false, false
		gate.armed = false
		if mic != nil {
			// 暂留失败端点，下一次检查前继续尝试静音，不静默切换到别的麦克风。
			if muteErr := mic.setMute(true); endpointGone(muteErr) {
				mic.close()
				mic = nil
				state.Device = ""
			}
		}
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case cmd := <-c.commands:
			var cmdErr error
			switch cmd.kind {
			case "apply":
				newKey, parseErr := parseHotkey(cmd.config.Hotkey)
				if parseErr != nil {
					cmdErr = parseErr
					break
				}
				cfg, key = cmd.config, newKey
				state.Config = cfg
				nextDeviceCheck = time.Time{}
				refreshDevices = true
			case "pause":
				state.Paused = !state.Paused
			case "refresh":
				nextDeviceCheck = time.Time{}
				refreshDevices = true
			case "safety":
				if cmd.on {
					blockFlags |= cmd.flag
				} else {
					blockFlags &^= cmd.flag
				}
				state.Blocked = blockFlags != 0
			case "stop":
				if mic != nil {
					cmdErr = mic.setMute(true)
					if endpointGone(cmdErr) {
						cmdErr = nil // 已失效的端点无法继续采集声音，可以正常退出。
					}
				}
				if cmdErr != nil {
					fail(cmdErr)
					publish()
					cmd.reply <- cmdErr
					continue // 静音失败时保持程序运行，让用户看到错误并重试。
				}
				cmd.reply <- nil
				return
			}
			gate.armed, state.Speaking = false, false
			if mic != nil {
				if muteErr := mic.setMute(true); muteErr != nil {
					fail(muteErr)
					cmdErr = muteErr
				} else {
					lastMute = true
				}
			}
			publish()
			cmd.reply <- cmdErr
		case <-ticker.C:
			now := time.Now()
			if !lastTick.IsZero() && now.Sub(lastTick) > 250*time.Millisecond {
				gate.armed = false // 休眠或长时间停顿后必须重新松键，再按下才开麦。
			}
			lastTick = now
			if !now.Before(nextDeviceCheck) {
				nextDeviceCheck = now.Add(time.Second)
				if refreshDevices {
					if devices, listErr := audio.devices(); listErr == nil {
						state.Devices = devices
					}
					refreshDevices = false
				}
				candidate, openErr := audio.endpoint(cfg.DeviceID)
				if openErr != nil {
					fail(openErr)
				} else if mic != nil && mic.info().ID == candidate.info().ID && state.Ready {
					candidate.close()
				} else {
					// 先关闭旧端点的声音，再接管新端点；换设备不会沿用按住状态。
					if mic != nil {
						if muteErr := mic.setMute(true); muteErr != nil && !endpointGone(muteErr) {
							candidate.close()
							fail(muteErr)
							publish()
							continue
						}
						mic.close()
					}
					mic = candidate
					gate.armed = false
					if muteErr := mic.setMute(true); muteErr != nil {
						fail(muteErr)
					} else {
						lastMute = true
						state.Device, state.Ready, state.Error = mic.info().Name, true, ""
					}
				}
			}
			active, released := readKeys(key)
			speaking := gate.update(active, released, state.Ready && !state.Paused && !state.Blocked)
			wantMute := !speaking
			if mic != nil && (wantMute != lastMute || !now.Before(nextStateCheck)) {
				nextStateCheck = now.Add(100 * time.Millisecond)
				actual, readErr := mic.muted()
				if readErr != nil {
					fail(readErr)
				} else if actual != wantMute {
					if muteErr := mic.setMute(wantMute); muteErr != nil {
						fail(muteErr)
					} else {
						lastMute = wantMute
					}
				} else {
					lastMute = actual
				}
			}
			state.Speaking = speaking && state.Ready && !lastMute
			if !now.Before(nextPublish) {
				nextPublish = now.Add(100 * time.Millisecond)
				publish()
			}
		}
	}
}
