package privacyscreen

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"sort"
)

// 内置背景图随守护进程一起打包，用户不选任何文件也有得用。
// 产物由 tech_doc/toolcmd/mkprivacyimg 生成。
//
//go:embed assets/*.png
var builtinImages embed.FS

// BuiltinPresets 内置背景图的标识与显示名。
var BuiltinPresets = []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}{
	{ID: "aurora", Name: "极光深蓝"},
	{ID: "teal", Name: "青碧"},
	{ID: "ember", Name: "暖橙"},
}

// builtinPresetIDs 合法的内置图标识。
var builtinPresetIDs = func() map[string]bool {
	m := make(map[string]bool, len(BuiltinPresets))
	for _, p := range BuiltinPresets {
		m[p.ID] = true
	}
	return m
}()

// IsBuiltinPreset 报告标识是否为内置背景图。
func IsBuiltinPreset(id string) bool {
	return builtinPresetIDs[id]
}

// PresetIDs 返回全部内置图标识，按声明顺序。
func PresetIDs() []string {
	out := make([]string, 0, len(BuiltinPresets))
	for _, p := range BuiltinPresets {
		out = append(out, p.ID)
	}
	sort.Strings(out)
	return out
}

// BuiltinImage 读取内置背景图的 PNG 字节。
func BuiltinImage(id string) ([]byte, error) {
	if !builtinPresetIDs[id] {
		return nil, fmt.Errorf("未知的内置背景图: %q", id)
	}
	data, err := builtinImages.ReadFile("assets/privacy_" + id + ".png")
	if err != nil {
		return nil, fmt.Errorf("读取内置背景图失败: %w", err)
	}
	return data, nil
}

// decodeBuiltinImage 解码内置背景图，供遮罩引擎合成背景。
func decodeBuiltinImage(id string) (image.Image, error) {
	data, err := BuiltinImage(id)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解码内置背景图失败: %w", err)
	}
	return img, nil
}
