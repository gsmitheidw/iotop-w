// iotop-w [iotop-w with disk pressure bar] G. Smith, Feb2026.
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
	historyWidth    = 30
	topN            = 5
	minInterval     = 100 * time.Millisecond
	maxInterval     = 5 * time.Second
	queueSaturation = 2.0
)

// ---------- ANSI ----------
const (
	Green1 = "\x1b[32m"
	Green2 = "\x1b[32;1m"
	Yellow = "\x1b[33m"
	Orange = "\x1b[38;5;208m"
	Red    = "\x1b[31;1m"
	Blue   = "\x1b[34;1m"
	Reset  = "\x1b[0m"
)

// ---------- Braille ----------
var braille = []rune{
	0x2800, 0x2804, 0x2806, 0x2807,
	0x2847, 0x28C7, 0x28E7, 0x28F7, 0x28FF,
}

// ---------- Win32 / PDH ----------
var (
	k32 = windows.NewLazySystemDLL("kernel32.dll")
	u32 = windows.NewLazySystemDLL("user32.dll")
	pdh = windows.NewLazySystemDLL("pdh.dll")

	pGetIOCounters = k32.NewProc("GetProcessIoCounters")
	pKey           = u32.NewProc("GetAsyncKeyState")

	pPdhOpenQuery  = pdh.NewProc("PdhOpenQueryW")
	pPdhAddCounter = pdh.NewProc("PdhAddEnglishCounterW")
	pPdhCollect    = pdh.NewProc("PdhCollectQueryData")
	pPdhGetValue   = pdh.NewProc("PdhGetFormattedCounterValue")
	pPdhCloseQuery = pdh.NewProc("PdhCloseQuery")
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
	LastSeen    time.Time
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
		idx := int(c.Level + 0.5)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(braille) {
			idx = len(braille) - 1
		}
		b.WriteString(c.Color)
		b.WriteRune(braille[idx])
		b.WriteString(Reset)
	}
	return b.String()
}

// ---------- Helpers ----------
func scale(v, max float64) float64 {
	if max <= 0 {
		return 0
	}
	return v * float64(len(braille)-1) / max
}

func colorRatio(r float64) string {
	switch {
	case r > 1.0:
		return Red
	case r > 0.75:
		return Orange
	case r > 0.5:
		return Yellow
	case r > 0.25:
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

// ---------- PDH Disk Queue ----------
type DiskQueue struct {
	query   uintptr
	counter uintptr
}

func newDiskQueue() *DiskQueue {
	var q uintptr
	pPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q)))
	path, _ := windows.UTF16PtrFromString(`\PhysicalDisk(_Total)\Avg. Disk Queue Length`)
	var c uintptr
	pPdhAddCounter.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c)))
	pPdhCollect.Call(q)
	return &DiskQueue{query: q, counter: c}
}

func (d *DiskQueue) read() float64 {
	pPdhCollect.Call(d.query)
	var val struct {
		CStatus     uint32
		DoubleValue float64
	}
	pPdhGetValue.Call(d.counter, 0x00000200, 0, uintptr(unsafe.Pointer(&val)))
	return val.DoubleValue
}

func (d *DiskQueue) close() {
	pPdhCloseQuery.Call(d.query)
}

// ---------- Solid Disk Bar ----------
func renderDiskPressureBar(depth float64, width int, maxQueue float64) string {
	level := depth / maxQueue
	if level > 1 {
		level = 1
	}
	filled := int(level * float64(width))
	bar := strings.Repeat("█", filled) + strings.Repeat(" ", width-filled)
	return colorRatio(level) + bar + Reset
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
	h, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	var mode uint32
	windows.GetConsoleMode(h, &mode)
	windows.SetConsoleMode(h, mode|0x0004)

	restore := disableEcho()
	defer restore()
	fmt.Print("\x1b[?25l")
	defer fmt.Print("\x1b[?25h")

	disk := newDiskQueue()
	defer disk.close()

	interval := time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prevIO := snapshotIO()
	procs := map[uint32]*ProcHist{}

	for {
		select {
		case <-ticker.C:
			fmt.Print("\x1b[H\x1b[J")
			fmt.Printf("%s=== iotop-w ===%s\n\n", Blue, Reset)

			q := disk.read()
			fmt.Printf("%sDisk Pressure%s  Queue: %.2f\n", Blue, Reset, q)
			fmt.Println(renderDiskPressureBar(q, historyWidth, queueSaturation))
			fmt.Println()

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
			sort.Slice(rates, func(i, j int) bool { return rates[i].Total > rates[j].Total })

			fmt.Printf("%sI/O Top%s  Interval: %v  (- / +, q quit)\n", Blue, Reset, interval)

			now := time.Now()
			for i := 0; i < topN && i < len(rates); i++ {
				r := rates[i]
				h := procs[r.PID]
				if h == nil {
					h = &ProcHist{
						Read:     newRing(historyWidth),
						Write:    newRing(historyWidth),
						Max:      1,
						LastSeen: now,
					}
					procs[r.PID] = h
				}
				h.LastSeen = now
				if r.Total > h.Max {
					h.Max = r.Total
				} else {
					h.Max = uint64(float64(h.Max) * 0.95)
				}
				h.Read.push(Cell{scale(float64(r.Read), float64(h.Max)), colorRatio(float64(r.Read) / float64(h.Max))})
				h.Write.push(Cell{scale(float64(r.Write), float64(h.Max)), colorRatio(float64(r.Write) / float64(h.Max))})
				fmt.Printf("%d %s\nR %s\nW %s\n", r.PID, r.Name, h.Read.render(), h.Write.render())
			}

			for pid, h := range procs {
				if now.Sub(h.LastSeen) > 30*time.Second {
					delete(procs, pid)
				}
			}

			prevIO = curr

		default:
			switch {
			case key(0x51):
				return
			case key(0xBD), key(0x6D):
				interval = clamp(interval - 100*time.Millisecond)
				ticker.Reset(interval)
			case key(0xBB), key(0x6B):
				interval = clamp(interval + 100*time.Millisecond)
				ticker.Reset(interval)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

