package subtitle

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
)

type ASSFile struct {
	Header string
	Events []ASSEvent
}

type ASSEvent struct {
	StartMs int64
	EndMs   int64
	Fields  string // Style,Name,MarginL,MarginR,MarginV,Effect,Text
}

func ParseASS(path string) (*ASSFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ass := &ASSFile{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	var headerLines []string
	inEvents := false
	pastFormat := false

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.EqualFold(trimmed, "[Events]") {
			inEvents = true
			headerLines = append(headerLines, line)
			continue
		}
		if !inEvents {
			headerLines = append(headerLines, line)
			continue
		}
		if strings.HasPrefix(trimmed, "Format:") {
			headerLines = append(headerLines, line)
			pastFormat = true
			continue
		}
		if !pastFormat {
			headerLines = append(headerLines, line)
			continue
		}
		if strings.HasPrefix(trimmed, "Dialogue:") {
			ev, err := parseASSDialogue(trimmed)
			if err != nil {
				continue
			}
			ass.Events = append(ass.Events, ev)
		}
	}

	ass.Header = strings.Join(headerLines, "\n")
	return ass, scanner.Err()
}

func FormatASSTimestamp(ms int64) string {
	h := ms / mkv.MsPerHour
	m := (ms % mkv.MsPerHour) / mkv.MsPerMinute
	s := (ms % mkv.MsPerMinute) / mkv.MsPerSecond
	cs := (ms % mkv.MsPerSecond) / 10
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs)
}

func ParseASSTimestamp(s string) (int64, bool) {
	var h, m, sec, cs int64
	s = strings.Replace(s, ".", ":", 1)
	n, _ := fmt.Sscanf(s, "%d:%d:%d:%d", &h, &m, &sec, &cs)
	if n != 4 {
		return 0, false
	}
	return h*mkv.MsPerHour + m*mkv.MsPerMinute + sec*mkv.MsPerSecond + cs*10, true
}

func parseASSDialogue(line string) (ASSEvent, error) {
	after := strings.TrimPrefix(line, "Dialogue:")
	after = strings.TrimSpace(after)

	parts := strings.SplitN(after, ",", 10)
	if len(parts) < 10 {
		return ASSEvent{}, fmt.Errorf("not enough fields in Dialogue line")
	}

	start, ok1 := ParseASSTimestamp(strings.TrimSpace(parts[1]))
	end, ok2 := ParseASSTimestamp(strings.TrimSpace(parts[2]))
	if !ok1 || !ok2 {
		return ASSEvent{}, fmt.Errorf("invalid timestamps")
	}

	return ASSEvent{StartMs: start, EndMs: end, Fields: strings.Join(parts[3:], ",")}, nil
}
