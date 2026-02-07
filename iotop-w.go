// iotop-w [iotop for windows] G. Smith, Feb2026.
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	historyWidth = 30
	topN         = 5
	minInterval  = 100 * time.Millisecond
	maxInterval  = 5 * time.Second
)

// ---------- ANSI ----------
const (
	Green1 = "\x1b[32m"
	Green2 = "\x1b[32;1m"
	Yellow = "\x1b[33m"
	Orange = "\x1b[38;5;208m"
	Red    = "\x1b[31;1m"
	Blue   = "\x1b[34;1m"
	Base0  = "\x1b[37m"
	Reset  = "\x1b[0m"
)

// ---------- Braille ----------
var braille = []rune{
	0x2800, 0x2804, 0x2806, 0x2807,
	0x2847, 0x28C7, 0x28E7, 0x28F7, 0x28FF,
}

// ---------- Win32 ----------
var (
	k32 = windows.NewLazySystemDLL("kernel32.dll")
	u32 = windows.NewLazySystemDLL("user32.dll")

	pGetIOCounters = k32.NewProc("GetProcessIoCounters")
	pGetSystemTime = k32.NewProc("GetSystemTimes")
	pKey           = u32.NewProc("GetAsyncKeyState")
)

// ---------- Types ----------
type Cell struct {
	Level float64
	Color string
}

type Ring struct {
	buf  []Cell
	head int
}

type ProcHist struct {
	Read, Write Ring
	Max         uint64
}

type ProcIO struct {
	PID   uint32
	Name  string
	Read  uint64
	Write uint64
}

type Rates struct {
	PID   uint32
	Name  string
	Read  uint64
	Write uint64
	Total uint64
}

// ---------- Ring ----------
func newRing(n int) Ring { return Ring{buf: make([]Cell, n)} }

func (r *Ring) push(c Cell) {
	r.buf[r.head] = c
	r.head = (r.head + 1) % len(r.buf)
}

func (r Ring) render() string {
	var b strings.Builder
	for i := 0; i < len(r.buf); i++ {
		c := r.buf[(r.head+i)%len(r.buf)]
		b.WriteString(c.Color)
		b.WriteRune(braille[int(c.Level+0.5)])
		b.WriteString(Reset)
	}
	return b.String()
}

// ---------- Helpers ----------
func scale(v, max uint64) float64 {
	if max == 0 {
		return 0
	}
	return float64(v) * float64(len(braille)-1) / float64(max)
}

func colorRatio(r float64) string {
	switch {
	case r > 0.75:
		return Red
	case r > 0.5:
		return Orange
	case r > 0.25:
		return Yellow
	case r > 0.1:
		return Green2
	default:
		return Green1
	}
}

func clamp(d time.Duration) time.Duration {
	if d < minInterval {
		return minInterval
	}
	if d > maxInterval {
		return maxInterval
	}
	return d
}

func key(vk int) bool {
	r, _, _ := pKey.Call(uintptr(vk))
	return r&0x8000 != 0
}

func disableEcho() func() {
	h, _ := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	var mode uint32
	windows.GetConsoleMode(h, &mode)
	orig := mode
	mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT
	windows.SetConsoleMode(h, mode)
	return func() { windows.SetConsoleMode(h, orig) }
}

// ---------- CPU / IO WAIT ----------
type CpuTimes struct {
	Idle, Kernel, User uint64
}

func getCpuTimes() CpuTimes {
	var idle, kernel, user windows.Filetime
	pGetSystemTime.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	return CpuTimes{
		Idle:   uint64(idle.HighDateTime)<<32 | uint64(idle.LowDateTime),
		Kernel: uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime),
		User:   uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime),
	}
}

// ---------- PROCESS IO ----------
func snapshotIO() map[uint32]ProcIO {
	out := make(map[uint32]ProcIO)

	snap, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if windows.Process32First(snap, &pe) != nil {
		return out
	}

	for {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err == nil {
			var io windows.IO_COUNTERS
			if r, _, _ := pGetIOCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&io))); r != 0 {
				out[pe.ProcessID] = ProcIO{
					PID:   pe.ProcessID,
					Name:  windows.UTF16ToString(pe.ExeFile[:]),
					Read:  io.ReadTransferCount,
					Write: io.WriteTransferCount,
				}
			}
			windows.CloseHandle(h)
		}
		if windows.Process32Next(snap, &pe) != nil {
			break
		}
	}
	return out
}

// ---------- Main ----------
func main() {
	// enable VT
	h, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	var mode uint32
	windows.GetConsoleMode(h, &mode)
	windows.SetConsoleMode(h, mode|0x0004)

	restore := disableEcho()
	defer restore()

	fmt.Print("\x1b[?25l")
	defer fmt.Print("\x1b[?25h")

	interval := time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prevCPU := getCpuTimes()
	waitHist := newRing(historyWidth)
	prevIO := snapshotIO()
	procs := map[uint32]*ProcHist{}

	for {
		select {
		case <-ticker.C:
			fmt.Print("\x1b[H\x1b[J")

			// ---- HEADER ----
			header := fmt.Sprintf("%s=== iotop-w ===%s", Blue, Reset)
			fmt.Println(header, "\n")

			// ---- IO WAIT ----
			nowCPU := getCpuTimes()
			busy := (nowCPU.Kernel - prevCPU.Kernel) - (nowCPU.Idle - prevCPU.Idle)
			total := (nowCPU.Kernel - prevCPU.Kernel) + (nowCPU.User - prevCPU.User)
			wait := 0.0
			if total > 0 {
				wait = float64(busy) / float64(total)
			}
			waitHist.push(Cell{
				Level: wait * float64(len(braille)-1),
				Color: colorRatio(wait),
			})
			prevCPU = nowCPU

			fmt.Printf("%sDisk I/O Wait%s  %3.0f%%\n", Blue, Reset, wait*100)
			fmt.Println(waitHist.render(), "\n")

			// ---- PROCESS IO ----
			curr := snapshotIO()
			var rates []Rates

			for pid, now := range curr {
				old, ok := prevIO[pid]
				if !ok {
					continue
				}
				r := now.Read - old.Read
				w := now.Write - old.Write
				if r+w == 0 {
					continue
				}
				rates = append(rates, Rates{pid, now.Name, r, w, r + w})
			}

			sort.Slice(rates, func(i, j int) bool {
				return rates[i].Total > rates[j].Total
			})

			fmt.Printf("%sI/O Top%s  Interval: %v  (- faster / + slower, q quit)\n", Blue, Reset, interval)

			for i := 0; i < topN && i < len(rates); i++ {
				r := rates[i]
				h := procs[r.PID]
				if h == nil {
					h = &ProcHist{
						Read:  newRing(historyWidth),
						Write: newRing(historyWidth),
						Max:   1,
					}
					procs[r.PID] = h
				}

				if r.Total > h.Max {
					h.Max = r.Total
				} else {
					h.Max = uint64(float64(h.Max) * 0.95)
				}

				h.Read.push(Cell{
					Level: scale(r.Read, h.Max),
					Color: colorRatio(float64(r.Read) / float64(h.Max)),
				})
				h.Write.push(Cell{
					Level: scale(r.Write, h.Max),
					Color: colorRatio(float64(r.Write) / float64(h.Max)),
				})

				fmt.Printf("%d %s\nR %s\nW %s\n",
					r.PID, r.Name,
					h.Read.render(),
					h.Write.render(),
				)
			}

			prevIO = curr

		default:
			switch {
			case key(0x51):
				return
			case key(0xBD) || key(0x6D): // -
				interval = clamp(interval - 100*time.Millisecond)
				ticker.Reset(interval)
			case key(0xBB) || key(0x6B): // +
				interval = clamp(interval + 100*time.Millisecond)
				ticker.Reset(interval)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

