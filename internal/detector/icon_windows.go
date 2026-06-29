package detector

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modShell32 = windows.NewLazyDLL("shell32.dll")
	modGdi32   = windows.NewLazyDLL("gdi32.dll")

	procExtractIconEx      = modShell32.NewProc("ExtractIconExW")
	procDrawIconEx         = user32.NewProc("DrawIconEx")
	procDestroyIcon        = user32.NewProc("DestroyIcon")
	procCreateCompatibleDC = modGdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = modGdi32.NewProc("CreateDIBSection")
	procSelectObject       = modGdi32.NewProc("SelectObject")
	procDeleteObject       = modGdi32.NewProc("DeleteObject")
	procDeleteDC           = modGdi32.NewProc("DeleteDC")
	procGdiFlush           = modGdi32.NewProc("GdiFlush")
)

// bitmapInfoHeader 与 Win32 BITMAPINFOHEADER 内存布局一致。
type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

const iconSize = 32

// ExtractIconDataURI 从 exe 提取应用图标并编码为 PNG data URI（data:image/png;base64,...）。
// 任何一步失败都静默返回空串，由调用方降级为占位图标，绝不阻断主流程。
// 全程释放 HICON / HBITMAP / HDC，避免 GDI 句柄泄漏。
func ExtractIconDataURI(exePath string) string {
	if exePath == "" {
		return ""
	}
	path16, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return ""
	}

	var hLarge, hSmall uintptr
	procExtractIconEx.Call(
		uintptr(unsafe.Pointer(path16)),
		0,
		uintptr(unsafe.Pointer(&hLarge)),
		uintptr(unsafe.Pointer(&hSmall)),
		1,
	)
	defer func() {
		if hLarge != 0 {
			procDestroyIcon.Call(hLarge)
		}
		if hSmall != 0 {
			procDestroyIcon.Call(hSmall)
		}
	}()
	hIcon := hLarge
	if hIcon == 0 {
		hIcon = hSmall
	}
	if hIcon == 0 {
		return ""
	}

	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		return ""
	}
	defer procDeleteDC.Call(hdc)

	bmi := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       iconSize,
		Height:      -iconSize, // 负高度 = top-down，行 0 即顶部
		Planes:      1,
		BitCount:    32,
		Compression: 0, // BI_RGB
	}
	var bits unsafe.Pointer
	hbmp, _, _ := procCreateDIBSection.Call(
		hdc,
		uintptr(unsafe.Pointer(&bmi)),
		0, // DIB_RGB_COLORS
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if hbmp == 0 || bits == nil {
		if hbmp != 0 {
			procDeleteObject.Call(hbmp)
		}
		return ""
	}
	defer procDeleteObject.Call(hbmp)

	old, _, _ := procSelectObject.Call(hdc, hbmp)
	procDrawIconEx.Call(hdc, 0, 0, hIcon, iconSize, iconSize, 0, 0, 0x0003) // DI_NORMAL
	procGdiFlush.Call()
	procSelectObject.Call(hdc, old)

	// DIB 像素为 BGRA，行顺序自上而下（与 bmi.Height<0 对应）
	raw := unsafe.Slice((*byte)(bits), iconSize*iconSize*4)
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	anyAlpha := false
	for i := 0; i < iconSize*iconSize; i++ {
		b, g, r, a := raw[i*4+0], raw[i*4+1], raw[i*4+2], raw[i*4+3]
		if a != 0 {
			anyAlpha = true
		}
		img.Pix[i*4+0] = r
		img.Pix[i*4+1] = g
		img.Pix[i*4+2] = b
		img.Pix[i*4+3] = a
	}
	// 老式图标无 per-pixel alpha：非全黑像素视为不透明
	if !anyAlpha {
		for i := 0; i < iconSize*iconSize; i++ {
			if img.Pix[i*4+0] != 0 || img.Pix[i*4+1] != 0 || img.Pix[i*4+2] != 0 {
				img.Pix[i*4+3] = 255
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}
