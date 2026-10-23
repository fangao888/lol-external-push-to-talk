package main

import (
	"fmt"
	"log"
	"math"
)

// gainSetting 保存首次增强前的音量，切换百分比或设备重连时都以此为基准。
type gainSetting struct {
	original float32 // 增强前的系统录音音量，单位 dB。
	percent  int     // 已成功应用的比例；0 表示需要重试。
	actual   float64 // 根据驱动读回值算出的实际比例。
	warning  string  // 设备上限或驱动精度提示。
}

// gainSession 只在音频线程访问；按稳定设备 ID 保留暂时断开设备的恢复信息。
type gainSession map[string]*gainSetting

// apply 在设备接管或设置变化时应用增强；100% 且从未增强时完全不修改音量。
func (g gainSession) apply(mic audioEndpoint, percent int) (float64, string) {
	setting := g[mic.info().ID]
	if percent == 100 {
		if err := g.restore(mic); err != nil {
			return 0, "恢复原始音量失败：" + err.Error()
		}
		return 100, ""
	}
	if setting != nil && setting.percent == percent {
		return setting.actual, setting.warning
	}
	volume, ok := mic.(endpointGain)
	if !ok {
		return 100, "此麦克风不支持系统音量增强，按键发言仍可使用。"
	}
	minimum, maximum, step, err := volume.volumeRangeDB()
	if err != nil {
		return 0, "读取麦克风增强范围失败：" + err.Error()
	}
	current, err := volume.volumeDB()
	if err != nil {
		return 0, "读取麦克风音量失败：" + err.Error()
	}
	if !finiteDB(minimum) || !finiteDB(maximum) || !finiteDB(current) || !finiteDB(step) || minimum > maximum || current < minimum || current > maximum || step < 0 {
		return 0, "麦克风驱动返回的音量范围无效，无法应用增强。"
	}
	if setting == nil {
		setting = &gainSetting{original: current}
		g[mic.info().ID] = setting // 写入前保存原值，即使驱动写入失败也可恢复。
	}
	setting.percent = 0
	desired := setting.original + float32(20*math.Log10(float64(percent)/100))
	target := min(maximum, desired)
	if target < setting.original || setting.original < minimum {
		return 0, "麦克风音量范围已变化，无法按原始音量增强，请恢复设备后重试。"
	}
	if math.Abs(float64(current-target)) > 0.001 {
		if err = volume.setVolumeDB(target); err != nil {
			return 0, "设置麦克风增强失败：" + err.Error()
		}
	}
	actual, err := volume.volumeDB()
	if err != nil || !finiteDB(actual) {
		return 0, fmt.Sprintf("无法确认麦克风增强后的音量：%v", err)
	}
	// 粗粒度驱动可能向上取整；退一个步长，避免增强明显超过选择的比例。
	if actual > desired+0.05 && step > 0 {
		if err = volume.setVolumeDB(max(setting.original, target-step)); err != nil {
			return 0, "调整麦克风增强失败：" + err.Error()
		}
		if actual, err = volume.volumeDB(); err != nil || !finiteDB(actual) {
			return 0, fmt.Sprintf("无法确认麦克风增强后的音量：%v", err)
		}
	}
	setting.actual = 100 * math.Pow(10, float64(actual-setting.original)/20)
	setting.warning = ""
	if desired > maximum+0.001 {
		setting.warning = fmt.Sprintf("已达到麦克风音量上限，实际增强约 %.0f%%（设置 %d%%）。", setting.actual, percent)
	} else if math.Abs(setting.actual-float64(percent)) >= 1 {
		setting.warning = fmt.Sprintf("受麦克风驱动精度限制，实际增强约 %.0f%%（设置 %d%%）。", setting.actual, percent)
	}
	setting.percent = percent
	return setting.actual, setting.warning
}

// finiteDB 排除驱动返回的 NaN 或无穷值，避免向系统发送无效音量。
func finiteDB(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}

// invalidate 保留基准但清除缓存，使重连后的端点重新应用设置而不叠加增强。
func (g gainSession) invalidate(mic audioEndpoint) {
	if setting := g[mic.info().ID]; setting != nil {
		setting.percent = 0
	}
}

// restore 在关闭增强、换设备或退出前恢复原始音量；失败时保留记录供重试。
func (g gainSession) restore(mic audioEndpoint) error {
	setting := g[mic.info().ID]
	if setting == nil {
		return nil
	}
	setting.percent = 0
	volume, ok := mic.(endpointGain)
	if !ok {
		return fmt.Errorf("%s 不再支持音量控制", mic.info().Name)
	}
	actual, err := volume.volumeDB()
	if err != nil {
		return err
	}
	// 设置增强失败且原值未变时无需再次写入，避免只读驱动阻止正常退出。
	if !finiteDB(actual) || math.Abs(float64(actual-setting.original)) > 0.001 {
		if err := volume.setVolumeDB(setting.original); err != nil {
			return err
		}
		if actual, err = volume.volumeDB(); err != nil {
			return err
		}
	}
	if !finiteDB(actual) || math.Abs(float64(actual-setting.original)) > 0.05 {
		return fmt.Errorf("驱动未恢复原始音量，请检查系统麦克风音量")
	}
	delete(g, mic.info().ID)
	return nil
}

// restoreOthers 重试已切换设备的音量恢复；已拔出的设备只能记日志，不能阻止退出。
func (g gainSession) restoreOthers(audio audioBackend, current audioEndpoint) string {
	var warning string
	for id := range g {
		if current != nil && id == current.info().ID {
			continue
		}
		old, err := audio.endpoint(id)
		if err == nil {
			err = g.restore(old)
			old.close()
		}
		if err != nil {
			warning = "有已切换或断开的麦克风尚未恢复原始音量，请重新连接并保持工具运行。"
		}
	}
	return warning
}

// logUnrestored 在退出时记录无法恢复的断开设备，保留可排查的信息。
func (g gainSession) logUnrestored() {
	for id, setting := range g {
		log.Printf("设备 %s 未能恢复增强前音量 %.3f dB（设备可能已断开）", id, setting.original)
	}
}
