package main

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// audioEndpoint 是控制循环需要的最小端点接口，也允许用虚拟设备验证静音行为。
type audioEndpoint interface {
	info() Device         // 返回当前端点标识。
	setMute(bool) error   // 设置系统静音。
	muted() (bool, error) // 查询实际静音。
	close()               // 释放端点接口。
}

// audioBackend 将设备发现和打开集中在所属 COM 线程。
type audioBackend interface {
	devices() ([]Device, error)             // 枚举活动录音端点。
	endpoint(string) (audioEndpoint, error) // 打开指定端点或默认通信端点。
}

// audioAPIError 保留 HRESULT，区分端点已失效与仍需重试的静音失败。
type audioAPIError struct {
	Code uint32 // Windows 返回的失败 HRESULT。
}

// Error 提供可以写入日志并显示给用户的错误信息。
func (e *audioAPIError) Error() string {
	return fmt.Sprintf("Windows 音频接口错误 HRESULT=0x%08X", e.Code)
}

// endpointGone 仅在系统确认端点不可用时允许丢弃旧接口，避免拔插后无法恢复。
func endpointGone(err error) bool {
	var apiErr *audioAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case 0x88890004, 0x88890010, 0x80010108, 0x800706BA:
		// AUDCLNT_E_DEVICE_INVALIDATED、音频服务停止、COM/RPC 连接断开。
		return true
	}
	return false
}

// Core Audio 的 COM 标识和属性键来自 Windows SDK 的 mmdeviceapi/endpointvolume。
var (
	clsidDeviceEnumerator = ole.NewGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidDeviceEnumerator   = ole.NewGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")
	iidEndpointVolume     = ole.NewGUID("{5CDF2C82-841E-4546-9722-0CF74078229A}")
	friendlyNameKey       = propertyKey{Format: *ole.NewGUID("{A45C254E-DF1C-4EFD-8020-67D146A850E0}"), ID: 14}
	propVariantClear      = windows.NewLazySystemDLL("ole32.dll").NewProc("PropVariantClear")
)

// Device 是可供用户选择的活动录音端点。
type Device struct {
	ID   string `json:"id"`   // 稳定的设备 ID，用于持久化选择。
	Name string `json:"name"` // Windows 中显示的设备名称。
}

// comObject 表示 IUnknown 兼容对象，接口必须在创建它的 COM 线程内使用。
type comObject struct {
	VTable *[32]uintptr // 虚表地址；索引采用各接口在 Windows SDK 中的方法顺序。
}

// invoke 保留传入的 Go 内存直到同步 COM 调用结束，兼容 amd64 与 386。
//
//go:uintptrescapes
func (obj *comObject) invoke(index int, args ...uintptr) error {
	params := append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)
	hr, _, _ := syscall.SyscallN(obj.VTable[index], params...)
	runtime.KeepAlive(obj)
	if int32(hr) < 0 {
		return &audioAPIError{Code: uint32(hr)}
	}
	return nil
}

// release 释放 COM 接口引用；只允许在所属线程调用一次。
func (obj *comObject) release() {
	if obj != nil {
		_ = obj.invoke(2)
	}
}

// propertyKey 与 Windows PROPERTYKEY 的内存布局一致。
type propertyKey struct {
	Format ole.GUID // 属性集合 GUID。
	ID     uint32   // 集合中的属性编号。
}

// propVariant 对应 PROPVARIANT；此处只读取 VT_LPWSTR 字符串。
type propVariant struct {
	Type     uint16    // Windows VARTYPE，31 表示 LPWSTR。
	Reserved [3]uint16 // Windows 规定的保留字段。
	Text     *uint16   // 联合体的字符串地址，由 PropVariantClear 释放。
	Extra    uintptr   // 联合体剩余空间，保证 32/64 位下的结构尺寸正确。
}

// audioSystem 持有设备枚举器，在一个锁定的 OS 线程内管理所有音频接口。
type audioSystem struct {
	enumerator *comObject // IMMDeviceEnumerator 接口引用。
}

// openAudio 初始化当前线程的 COM，并创建录音设备枚举器。
func openAudio() (*audioSystem, error) {
	err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	if err != nil {
		// S_FALSE 表示线程已经采用相同方式初始化，仍然需要配对反初始化。
		if oleErr, ok := err.(*ole.OleError); !ok || oleErr.Code() != 1 {
			return nil, err
		}
	}
	obj, err := ole.CreateInstance(clsidDeviceEnumerator, iidDeviceEnumerator)
	if err != nil {
		ole.CoUninitialize()
		return nil, err
	}
	return &audioSystem{enumerator: (*comObject)(unsafe.Pointer(obj))}, nil
}

// close 在释放全部端点后清理当前线程的 COM。
func (a *audioSystem) close() {
	a.enumerator.release()
	ole.CoUninitialize()
}

// deviceID 复制 Windows 分配的设备 ID，并立即释放原始内存。
func deviceID(device *comObject) (string, error) {
	var text *uint16
	if err := device.invoke(5, uintptr(unsafe.Pointer(&text))); err != nil {
		return "", err
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(text)))
	return windows.UTF16PtrToString(text), nil
}

// deviceName 从只读属性存储取得设备友好名称，失败时使用设备 ID。
func deviceName(device *comObject, fallback string) string {
	var store *comObject
	if device.invoke(4, 0, uintptr(unsafe.Pointer(&store))) != nil {
		return fallback
	}
	defer store.release()
	var value propVariant
	if store.invoke(5, uintptr(unsafe.Pointer(&friendlyNameKey)), uintptr(unsafe.Pointer(&value))) != nil {
		return fallback
	}
	defer propVariantClear.Call(uintptr(unsafe.Pointer(&value)))
	if value.Type == 31 && value.Text != nil {
		return windows.UTF16PtrToString(value.Text)
	}
	return fallback
}

// devices 仅列出当前可用的录音设备，不修改设备状态。
func (a *audioSystem) devices() ([]Device, error) {
	var collection *comObject
	// eCapture=1，DEVICE_STATE_ACTIVE=1。
	if err := a.enumerator.invoke(3, 1, 1, uintptr(unsafe.Pointer(&collection))); err != nil {
		return nil, err
	}
	defer collection.release()
	var count uint32
	if err := collection.invoke(3, uintptr(unsafe.Pointer(&count))); err != nil {
		return nil, err
	}
	devices := make([]Device, 0, count)
	for i := uint32(0); i < count; i++ {
		var device *comObject
		if err := collection.invoke(4, uintptr(i), uintptr(unsafe.Pointer(&device))); err != nil {
			continue // 设备恰好拔出时跳过，下一次刷新会重新枚举。
		}
		id, err := deviceID(device)
		if err == nil {
			devices = append(devices, Device{ID: id, Name: deviceName(device, id)})
		}
		device.release()
	}
	return devices, nil
}

// endpoint 打开指定设备；空 ID 使用默认通信录音端点，避免误选默认扬声器。
func (a *audioSystem) endpoint(id string) (audioEndpoint, error) {
	var device *comObject
	var err error
	if id == "" {
		err = a.enumerator.invoke(4, 1, 2, uintptr(unsafe.Pointer(&device)))
	} else {
		text, convErr := windows.UTF16PtrFromString(id)
		if convErr != nil {
			return nil, convErr
		}
		err = a.enumerator.invoke(5, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(&device)))
	}
	if err != nil {
		return nil, fmt.Errorf("找不到麦克风，请连接设备或重新选择：%w", err)
	}
	defer device.release()
	var state uint32
	if err = device.invoke(6, uintptr(unsafe.Pointer(&state))); err != nil {
		return nil, err
	}
	if state != 1 {
		return nil, fmt.Errorf("所选麦克风已断开或被禁用，请重新连接")
	}
	actualID, err := deviceID(device)
	if err != nil {
		return nil, err
	}
	var volume *comObject
	// CLSCTX_ALL=23；IAudioEndpointVolume 控制录音端点的系统静音。
	if err = device.invoke(3, uintptr(unsafe.Pointer(iidEndpointVolume)), 23, 0, uintptr(unsafe.Pointer(&volume))); err != nil {
		return nil, err
	}
	return &microphone{object: volume, device: Device{ID: actualID, Name: deviceName(device, actualID)}}, nil
}

// microphone 封装一个录音端点的 IAudioEndpointVolume。
type microphone struct {
	object *comObject // 音量接口，关闭时释放。
	device Device     // 端点标识，用于检查默认设备是否发生变化。
}

// info 返回已复制的端点信息，不暴露 COM 对象。
func (m *microphone) info() Device {
	return m.device
}

// setMute 只改变静音，不改变麦克风增益和其他应用的录音权限。
func (m *microphone) setMute(muted bool) error {
	var value uintptr
	if muted {
		value = 1
	}
	return m.object.invoke(14, value, 0)
}

// muted 查询实际静音状态，用于发现外部程序对静音状态的修改。
func (m *microphone) muted() (bool, error) {
	var value int32
	err := m.object.invoke(15, uintptr(unsafe.Pointer(&value)))
	return value != 0, err
}

// close 只释放音量接口，不改变端点静音状态。
func (m *microphone) close() {
	m.object.release()
}
