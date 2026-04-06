package subtitle

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

type SRTEntry struct {
	StartMs int64
	EndMs   int64
	Text    string
}

func ParseSRT(path string) ([]SRTEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []SRTEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || isDigitsOnly(line) {
			continue
		}
		start, end, ok := parseSRTTimeLine(line)
		if !ok {
			continue
		}
		var textLines []string
		for scanner.Scan() {
			tl := scanner.Text()
			if strings.TrimSpace(tl) == "" {
				break
			}
			textLines = append(textLines, tl)
		}
		if len(textLines) > 0 {
			entries = append(entries, SRTEntry{
				StartMs: start, EndMs: end,
				Text: strings.Join(textLines, "\n"),
			})
		}
	}
	return entries, scanner.Err()
}

func FormatSRTTime(ms int64) string {
	h := ms / mkv.MsPerHour
	m := (ms % mkv.MsPerHour) / mkv.MsPerMinute
	s := (ms % mkv.MsPerMinute) / mkv.MsPerSecond
	milli := ms % mkv.MsPerSecond
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, milli)
}

func isDigitsOnly(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

func parseSRTTimeLine(line string) (int64, int64, bool) {
	parts := strings.Split(line, "-->")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, ok1 := ParseSRTTimestamp(strings.TrimSpace(parts[0]))
	end, ok2 := ParseSRTTimestamp(strings.TrimSpace(parts[1]))
	return start, end, ok1 && ok2
}

func ParseSRTTimestamp(s string) (int64, bool) {
	var h, m, sec, ms int64
	s = strings.Replace(s, ",", ":", 1)
	n, _ := fmt.Sscanf(s, "%d:%d:%d:%d", &h, &m, &sec, &ms)
	if n != 4 {
		return 0, false
	}
	return h*mkv.MsPerHour + m*mkv.MsPerMinute + sec*mkv.MsPerSecond + ms, true
}
