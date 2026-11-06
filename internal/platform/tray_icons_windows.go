//go:build windows

package platform

import (
	"errors"
	"math"
	"syscall"
	"unsafe"
)

var gdi32 = syscall.NewLazyDLL("gdi32.dll")

type bitmapHeader struct {
	Size                   uint32
	Width, Height          int32
	Planes, BitCount       uint16
	Compression, SizeImage uint32
	XPels, YPels           int32
	Used, Important        uint32
}

type iconInfo struct {
	IsIcon             int32
	XHotspot, YHotspot uint32
	Mask, Color        uintptr
}

func iconMetric(metric, fallback int) int {
	dpi := uintptr(96)
	getDPI := user32.NewProc("GetDpiForSystem")
	if getDPI.Find() == nil {
		if value, _, _ := getDPI.Call(); value != 0 {
			dpi = value
		}
	}
	getMetric := user32.NewProc("GetSystemMetricsForDpi")
	if getMetric.Find() == nil {
		if value, _, _ := getMetric.Call(uintptr(metric), dpi); value >= 16 && value <= 128 {
			return int(value)
		}
	}
	return fallback * int(dpi) / 96
}

func segmentDistance(x, y, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((x-ax)*dx+(y-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(x-ax-t*dx, y-ay-t*dy)
}

// iconPixels 用既有分支形品牌几何和四倍采样生成预乘 BGRA，无外部位图依赖。
func iconPixels(size, kind int) []byte {
	pixels := make([]byte, size*size*4)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var red, green, blue, alpha float64
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					x := (float64(px) + (float64(sx)+.5)/4) * 64 / float64(size)
					y := (float64(py) + (float64(sy)+.5)/4) * 64 / float64(size)
					inside := math.Hypot(math.Max(math.Abs(x-32)-18, 0), math.Max(math.Abs(y-32)-18, 0)) <= 13
					if !inside {
						continue
					}
					r, g, b := float64(36), float64(93), float64(168)
					line := false
					switch kind {
					case 0:
						line = segmentDistance(x, y, 17, 16, 17, 48) < 2 || segmentDistance(x, y, 17, 32, 32, 32) < 2 || segmentDistance(x, y, 32, 32, 47, 17) < 2 || segmentDistance(x, y, 32, 32, 47, 47) < 2
						for _, p := range [][2]float64{{17, 16}, {47, 17}, {47, 47}} {
							d := math.Hypot(x-p[0], y-p[1])
							if d < 6 {
								line = d > 2
							}
						}
					case 1:
						line = (x >= 14 && x <= 50 && ((math.Abs(y-17) < 2) || (math.Abs(y-46) < 2))) || (y >= 17 && y <= 46 && (math.Abs(x-14) < 2 || math.Abs(x-50) < 2)) || (x >= 15 && x <= 49 && math.Abs(y-25) < 2)
					case 2:
						line = segmentDistance(x, y, 15, 34, 24, 34) < 2 || segmentDistance(x, y, 24, 34, 29, 21) < 2 || segmentDistance(x, y, 29, 21, 36, 45) < 2 || segmentDistance(x, y, 36, 45, 42, 31) < 2 || segmentDistance(x, y, 42, 31, 50, 31) < 2
					case 3:
						line = (math.Hypot(x-32, y-33) > 16 && math.Hypot(x-32, y-33) < 20 && !(y < 24 && x > 23 && x < 41)) || segmentDistance(x, y, 32, 12, 32, 33) < 2
					}
					if line {
						r, g, b = 255, 255, 255
					}
					red += r
					green += g
					blue += b
					alpha += 255
				}
			}
			index := (py*size + px) * 4
			pixels[index] = byte(blue / 16)
			pixels[index+1] = byte(green / 16)
			pixels[index+2] = byte(red / 16)
			pixels[index+3] = byte(alpha / 16)
		}
	}
	return pixels
}

func iconBitmap(size, kind int) (uintptr, error) {
	header := bitmapHeader{Size: uint32(unsafe.Sizeof(bitmapHeader{})), Width: int32(size), Height: -int32(size), Planes: 1, BitCount: 32}
	var bits unsafe.Pointer
	bitmap, _, _ := gdi32.NewProc("CreateDIBSection").Call(0, uintptr(unsafe.Pointer(&header)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bitmap == 0 || bits == nil {
		return 0, errors.New("创建托盘位图失败")
	}
	copy(unsafe.Slice((*byte)(bits), size*size*4), iconPixels(size, kind))
	return bitmap, nil
}

func brandTrayIcon(size int) (uintptr, error) {
	bitmap, err := iconBitmap(size, 0)
	if err != nil {
		return 0, err
	}
	defer gdi32.NewProc("DeleteObject").Call(bitmap)
	mask, _, _ := gdi32.NewProc("CreateBitmap").Call(uintptr(size), uintptr(size), 1, 1, 0)
	if mask == 0 {
		return 0, errors.New("创建托盘透明掩码失败")
	}
	defer gdi32.NewProc("DeleteObject").Call(mask)
	info := iconInfo{IsIcon: 1, Mask: mask, Color: bitmap}
	icon, _, _ := user32.NewProc("CreateIconIndirect").Call(uintptr(unsafe.Pointer(&info)))
	if icon == 0 {
		return 0, errors.New("创建品牌托盘图标失败")
	}
	return icon, nil
}
