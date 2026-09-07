//go:build windows

package privacyscreen

import (
	"sync"
	"syscall"
	"unsafe"
)

// topKeeper 维持遮罩处在 z-order 顶端。
//
// 起因：任务栏悬停缩略图（explorer.exe 的 TaskListThumbnailWnd / TaskListOverlayWnd）
// 是独立的置顶弹出窗口，出现时会压在遮罩之上，把窗口内容露给现场的人看。
// 实测不处理的话，悬停期间约有一半的采样里它压在遮罩上面。
//
// 做法：沿 GW_HWNDPREV 从遮罩往上走，发现有实际面积的可见窗口压在上面就重新置顶。
// 三个要点都是实测踩出来的：
//
//  1. 不能只看紧挨着的那一个。任务管理器这类窗口会长期占在更高的窗口层里，
//     只看相邻窗口会被它挡住视线，后面真正要处理的缩略图就永远发现不了。
//  2. 要跳过零尺寸窗口。系统里有大量「可见但没有面积」的置顶窗口，
//     它们遮不住任何东西，却会让抢置顶每帧都白白触发。
//  3. 反复抢不过的要拉黑。任务管理器的「置顶显示」会和我们来回互抢，
//     一次复检判定不出来，必须按连续失败次数来判。不拉黑的话每帧都在做无用功。
//
// 上面的置顶窗口通常只有个位数，整个过程足够便宜，可以放进主循环每帧调用。
type topKeeper struct {
	mu         sync.Mutex
	unbeatable map[uintptr]string // hwnd -> 类名，抢不过的窗口
	lastTarget uintptr
	sameCount  int
}

// giveUpAfter 连续对同一个窗口抢这么多次仍没解决，就认定抢不过
const giveUpAfter = 5

func newTopKeeper() *topKeeper {
	return &topKeeper{unbeatable: make(map[uintptr]string)}
}

func (k *topKeeper) reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.unbeatable = make(map[uintptr]string)
	k.lastTarget, k.sameCount = 0, 0
}

// uncovered 返回那些盖不住的窗口类名。
// 这些窗口在遮罩期间仍然可见 —— 属于隐私没能完全遮住，要如实告诉用户。
func (k *topKeeper) uncovered() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.unbeatable) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(k.unbeatable))
	out := make([]string, 0, len(k.unbeatable))
	for _, cls := range k.unbeatable {
		if !seen[cls] {
			seen[cls] = true
			out = append(out, cls)
		}
	}
	return out
}

func windowClass(h uintptr) string {
	buf := make([]uint16, 128)
	n, _, _ := pGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return "?"
	}
	return syscall.UTF16ToString(buf[:n])
}

func hasArea(h uintptr) bool {
	var rc rect
	pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
	return rc.R-rc.L > 0 && rc.B-rc.T > 0
}

// ensure 检查并在需要时重新置顶，返回是否真的抢了一次
func (k *topKeeper) ensure(hwnd uintptr) bool {
	k.mu.Lock()
	defer k.mu.Unlock()

	h := hwnd
	for i := 0; i < 32; i++ {
		prev, _, _ := pGetWindow.Call(h, gwHwndPrev)
		if prev == 0 {
			// 已经在最顶上
			k.lastTarget, k.sameCount = 0, 0
			return false
		}
		v, _, _ := pIsWindowVisible.Call(prev)
		if v == 0 || k.unbeatable[prev] != "" || !hasArea(prev) {
			h = prev
			continue
		}

		if prev == k.lastTarget {
			k.sameCount++
		} else {
			k.lastTarget, k.sameCount = prev, 1
		}
		if k.sameCount >= giveUpAfter {
			k.unbeatable[prev] = windowClass(prev)
			k.lastTarget, k.sameCount = 0, 0
			h = prev
			continue
		}

		pSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0,
			swpNoMove|swpNoSize|swpNoActivate)
		return true
	}
	return false
}
