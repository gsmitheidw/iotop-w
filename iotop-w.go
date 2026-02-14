// iotop-w  G. Smith, Feb2026.
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ----------------------- VERSION -----------------------
const (
	Version     = "1.02"
	DefaultTopN = 5
	MaxTopN     = 20
	RepoURL     = "https://github.com/gsmitheidw/iotop-w"
	License     = "MIT License"
)

// ----------------------- SETTINGS -----------------------
const (
	historyWidth    = 30
	minInterval     = 100 * time.Millisecond
	maxInterval     = 10 * time.Second
	queueSaturation = 2.0
	maxNameLen      = 16 // truncate long process names
)

// ----------------------- SOLARIZED DARK PALETTE -----------------------
var (
	Base03 = "\x1b[38;5;234m"
	Base0  = "\x1b[38;5;250m"
	Green  = "\x1b[38;5;64m"
	Yellow = "\x1b[38;5;136m"
	Orange = "\x1b[38;5;166m"
	Red    = "\x1b[38;5;160m"
	Blue   = "\x1b[38;5;33m"
	Reset  = "\x1b[0m"
)

// ----------------------- BRAILLE SPARKLINE -----------------------
var braille = []rune{
	0x2800, 0x2801, 0x2803, 0x2807,
	0x2817, 0x2837, 0x2877, 0x28F7, 0x28FF,
}

type Cell struct {
	Level float64
	Color string
}

type Ring struct {
	buf  []Cell
	head int
}

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

// ----------------------- HELPERS -----------------------
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
		return Green
	default:
		return Base0
	}
}

// Allowed intervals for stepping
var allowedIntervals = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	300 * time.Millisecond,
	400 * time.Millisecond,
	500 * time.Millisecond,
	600 * time.Millisecond,
	700 * time.Millisecond,
	800 * time.Millisecond,
	900 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// nextInterval strictly steps through allowedIntervals
func nextInterval(curr time.Duration, up bool) time.Duration {
	for i, v := range allowedIntervals {
		if curr == v {
			if up && i < len(allowedIntervals)-1 {
				return allowedIntervals[i+1]
			}
			if !up && i > 0 {
				return allowedIntervals[i-1]
			}
			return curr // at boundary, do nothing
		}
	}
	return curr // do not jump if curr is not exact
}

func key(vk int) bool {
	r, _, _ := pKey.Call(uintptr(vk))
	return r&0x8000 != 0
}

// disableEcho turns off line input & echo, returns restore function
func disableEcho() func() {
	h, _ := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	var mode uint32
	windows.GetConsoleMode(h, &mode)
	orig := mode
	mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT
	windows.SetConsoleMode(h, mode)
	return func() { windows.SetConsoleMode(h, orig) }
}

// ----------------------- WIN32 / PDH -----------------------
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

// ----------------------- TYPES -----------------------
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

type DiskQueue struct {
	query   uintptr
	counter uintptr
}

// ----------------------- DISK QUEUE -----------------------
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

// ----------------------- DISK BAR -----------------------
func renderDiskBar(depth float64, width int, maxQueue float64) string {
	level := depth / maxQueue
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}

	filled := int(level * float64(width))
	bar := make([]string, width)
	for i := range bar {
		if i < filled {
			bar[i] = Red + "█" + Reset
		} else {
			bar[i] = Base03 + "█" + Reset
		}
	}
	return fmt.Sprintf("%sDisk Pressure:%s %s %.2f/%.1f", Blue, Reset, strings.Join(bar, ""), depth, maxQueue)
}

// ----------------------- PROCESS SNAPSHOT -----------------------
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

// ----------------------- MAIN -----------------------
func main() {
	top := DefaultTopN
	help := false
	showVersion := false
	showInfo := false

	for i := 1; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--help" || os.Args[i] == "-h":
			help = true
		case os.Args[i] == "--version" || os.Args[i] == "-v":
			showVersion = true
		case os.Args[i] == "--info" || os.Args[i] == "-i":
			showInfo = true
		case os.Args[i] == "--top":
			if i+1 < len(os.Args) {
				n, err := strconv.Atoi(os.Args[i+1])
				if err == nil && n > 0 {
					if n > MaxTopN {
						top = MaxTopN
					} else {
						top = n
					}
				}
				i++
			}
		case strings.HasPrefix(os.Args[i], "--top="):
			n, err := strconv.Atoi(strings.TrimPrefix(os.Args[i], "--top="))
			if err == nil && n > 0 {
				if n > MaxTopN {
					top = MaxTopN
				} else {
					top = n
				}
			}
		}
	}

	if help {
		fmt.Printf(`iotop-w %s
Usage: iotop-w [options]

Options:
  --help, -h       Show this help message
  --version, -v    Show version
  --info, -i       Show repo info and license
  --top <number>   Show top <number> processes (max %d)
`, Version, MaxTopN)
		return
	}

	if showVersion {
		fmt.Println("iotop-w version", Version)
		return
	}

	if showInfo {
		fmt.Printf("iotop-w repo: %s\nLicense: %s\n", RepoURL, License)
		return
	}

	h, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	var mode uint32
	windows.GetConsoleMode(h, &mode)
	windows.SetConsoleMode(h, mode|0x0004)

	restore := disableEcho()
	defer func() {
		restore()
		windows.FlushConsoleInputBuffer(windows.STD_INPUT_HANDLE)
		fmt.Print("\x1b[H\x1b[J\x1b[?25h") // clear screen & show cursor
	}()
	fmt.Print("\x1b[?25l")

	disk := newDiskQueue()
	defer disk.close()

	interval := 1 * time.Second // start at usable default
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prevIO := snapshotIO()
	procs := map[uint32]*ProcHist{}

	// --- new: key state tracking ---
	var prevQ, prevPlus, prevMinus bool

	for {
		select {
		case <-ticker.C:
			fmt.Print("\x1b[H\x1b[J")
			fmt.Printf("%s〘iotop-w〙 %s %s\n\n", Blue, Version, Reset)

			q := disk.read()
			fmt.Println(renderDiskBar(q, 30, queueSaturation))
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

			fmt.Printf("%s%-5s │ %-*s │ %-*s │ %-*s%s\n",
				Blue, "PID", maxNameLen, "Name", historyWidth, "Read", historyWidth, "Write", Reset)

			now := time.Now()
			for i := 0; i < top && i < len(rates); i++ {
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

				nameRunes := []rune(r.Name)
				if len(nameRunes) > maxNameLen {
					nameRunes = nameRunes[:maxNameLen-1]
					nameRunes = append(nameRunes, '…')
				}
				displayName := string(nameRunes)

				fmt.Printf("%-5d │ %-*s │ %-*s │ %-*s\n",
					r.PID, maxNameLen, displayName, historyWidth, h.Read.render(), historyWidth, h.Write.render())
			}

			for pid, h := range procs {
				if now.Sub(h.LastSeen) > 30*time.Second {
					delete(procs, pid)
				}
			}

			prevIO = curr

			// display interval nicely
			var intervalStr string
			if interval < time.Second {
				intervalStr = fmt.Sprintf("%dms", interval.Milliseconds())
			} else {
				intervalStr = fmt.Sprintf("%.0fs", interval.Seconds())
			}

			fmt.Printf("\n%sInterval: %s  |  +/- to adjust, q to quit%s\n",
				Blue, intervalStr, Reset)

		default:
			// --- handle keys on transition ---
			currQ := key(0x51)                         // Q
			currMinus := key(0xBD) || key(0x6D)        // -
			currPlus := key(0xBB) || key(0x6B)         // +

			if currQ && !prevQ {
				fmt.Print("\x1b[H\x1b[J\x1b[?25h")
				return
			}
			if currMinus && !prevMinus {
				interval = nextInterval(interval, false)
				ticker.Reset(interval)
			}
			if currPlus && !prevPlus {
				interval = nextInterval(interval, true)
				ticker.Reset(interval)
			}

			prevQ, prevMinus, prevPlus = currQ, currMinus, currPlus

			time.Sleep(10 * time.Millisecond)
		}
	}
}

