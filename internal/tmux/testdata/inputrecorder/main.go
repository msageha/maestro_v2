// inputrecorder is a test helper binary that simulates a TUI with bracketed paste.
//
// Usage: inputrecorder <logfile> [processing_delay_ms]
//
// Behavior:
//  1. Sets terminal to raw mode
//  2. Enables bracketed paste mode (ESC[?2004h)
//  3. Prints "READY>" prompt
//  4. Reads stdin byte-by-byte
//  5. Each CR (0x0D) triggers a "submission" — content is written to logfile
//  6. Bracketed paste sequences (ESC[200~ ... ESC[201~) are detected and logged as PASTE
//  7. After processing each submission, prints "READY>" again
//  8. EOF or read error exits
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// termios for darwin (macOS)
type termios struct {
	Iflag  uint64
	Oflag  uint64
	Cflag  uint64
	Lflag  uint64
	Cc     [20]byte
	Ispeed uint64
	Ospeed uint64
}

func tcgetattr(fd int, t *termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(0x40487413), // TIOCGETA on darwin
		uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func tcsetattr(fd int, t *termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(0x80487414), // TIOCSETA on darwin
		uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

func makeRaw(fd int) (*termios, error) {
	var orig termios
	if err := tcgetattr(fd, &orig); err != nil {
		return nil, fmt.Errorf("tcgetattr: %w", err)
	}

	raw := orig
	// Input flags: disable BRKINT, ICRNL, INPCK, ISTRIP, IXON
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	// Output flags: disable OPOST
	raw.Oflag &^= syscall.OPOST
	// Control flags: set CS8
	raw.Cflag |= syscall.CS8
	// Local flags: disable ECHO, ICANON, IEXTEN, ISIG
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	// Read returns after 1 byte, no timeout
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if err := tcsetattr(fd, &raw); err != nil {
		return nil, fmt.Errorf("tcsetattr: %w", err)
	}
	return &orig, nil
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: inputrecorder <logfile> [delay_ms]\n")
		os.Exit(1)
	}
	logPath := os.Args[1]
	delayMs := 0
	if len(os.Args) >= 3 {
		delayMs, _ = strconv.Atoi(os.Args[2])
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create log: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	// Set raw mode
	origTermios, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "raw mode: %v\n", err)
		os.Exit(1)
	}
	defer tcsetattr(int(os.Stdin.Fd()), origTermios)

	// Enable bracketed paste mode
	os.Stdout.WriteString("\x1b[?2004h")
	// Print prompt (with \r\n for raw mode)
	os.Stdout.WriteString("READY>\r\n")

	buf := make([]byte, 1)
	var accum []byte
	inPaste := false  // parser state: true between start/end markers
	sawPaste := false // submission state: true if any paste content was seen since last CR
	submissionID := 0

	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 && err != nil {
			break
		}
		if n == 0 {
			continue
		}
		b := buf[0]

		// Accumulate
		accum = append(accum, b)

		// Check for paste start/end sequences at the end of the accumulator.
		// Only detect pasteStart when not already in paste, and pasteEnd when in paste,
		// to avoid stripping literal sequences outside a paste block.
		s := string(accum)
		if !inPaste && strings.HasSuffix(s, pasteStart) {
			inPaste = true
			sawPaste = true
			accum = accum[:len(accum)-len(pasteStart)]
			continue
		}
		if inPaste && strings.HasSuffix(s, pasteEnd) {
			inPaste = false
			accum = accum[:len(accum)-len(pasteEnd)]
			continue
		}

		// CR (0x0D) = submission trigger (in raw mode, Enter sends CR)
		if b == '\r' {
			submissionID++
			content := string(accum[:len(accum)-1]) // trim the CR
			kind := "TYPED"
			if sawPaste {
				kind = "PASTE"
			}
			if len(content) > 0 {
				// Use %q for content to safely escape newlines, tabs, and
				// other special characters in the TSV log format.
				fmt.Fprintf(logFile, "%d\t%s\t%s\t%q\n",
					submissionID,
					time.Now().UTC().Format(time.RFC3339Nano),
					kind,
					content)
				logFile.Sync()
			}
			accum = nil
			inPaste = false
			sawPaste = false

			// Echo submission for pane capture visibility
			os.Stdout.WriteString(fmt.Sprintf("RECV[%d]:%s:%s\r\n", submissionID, kind, truncate(content, 60)))

			// Processing delay simulation
			if delayMs > 0 {
				time.Sleep(time.Duration(delayMs) * time.Millisecond)
			}
			os.Stdout.WriteString("READY>\r\n")
		}

		if err != nil {
			break
		}
	}

	// Disable bracketed paste
	os.Stdout.WriteString("\x1b[?2004l")
}

func truncate(s string, max int) string {
	// Replace newlines for display
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
