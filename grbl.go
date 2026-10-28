package main

import (
	"strconv"
	"strings"
)

// Real-time bytes: GRBL acts on them the moment they arrive, outside the
// line buffer, so they work even while it is full.
const (
	rtStatus    = '?'
	rtCycle     = '~'
	rtHold      = '!'
	rtReset     = 0x18
	rtJogCancel = 0x85
)

// Status is one parsed `<…>` report.
type Status struct {
	State string     `json:"state"` // "Idle", "Run", "Hold:0", "Alarm", …
	MPos  [3]float64 `json:"mpos"`
	WPos  [3]float64 `json:"wpos"`
	Feed  float64    `json:"feed"`
	Power float64    `json:"power"`
}

// parseStatus reads `<Idle|MPos:1,2,3|FS:0,0|WCO:…>`. GRBL reports either
// MPos or WPos (per $10) and WCO only now and then, so the last offset is
// passed in and the updated one returned.
func parseStatus(line string, wco [3]float64) (Status, [3]float64, bool) {
	if !strings.HasPrefix(line, "<") || !strings.HasSuffix(line, ">") {
		return Status{}, wco, false
	}
	fields := strings.Split(line[1:len(line)-1], "|")
	s := Status{State: fields[0]}
	var mpos, wpos *[3]float64
	for _, f := range fields[1:] {
		key, value, ok := strings.Cut(f, ":")
		if !ok {
			continue
		}
		nums := parseNums(value)
		switch key {
		case "MPos":
			p := toPos(nums)
			mpos = &p
		case "WPos":
			p := toPos(nums)
			wpos = &p
		case "WCO":
			wco = toPos(nums)
		case "FS":
			if len(nums) > 0 {
				s.Feed = nums[0]
			}
			if len(nums) > 1 {
				s.Power = nums[1]
			}
		case "F":
			if len(nums) > 0 {
				s.Feed = nums[0]
			}
		}
	}
	switch {
	case mpos != nil:
		s.MPos = *mpos
		for i := range s.WPos {
			s.WPos[i] = s.MPos[i] - wco[i]
		}
	case wpos != nil:
		s.WPos = *wpos
		for i := range s.MPos {
			s.MPos[i] = s.WPos[i] + wco[i]
		}
	}
	return s, wco, true
}

func parseNums(s string) []float64 {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		if v, err := strconv.ParseFloat(part, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func toPos(n []float64) [3]float64 {
	var p [3]float64
	copy(p[:], n)
	return p
}

// cleanLine strips comments and spaces (GRBL ignores spaces; every byte
// saved is buffer room). Empty result means nothing to send.
func cleanLine(raw string) string {
	var b strings.Builder
	inParen := false
	for _, r := range raw {
		if inParen {
			if r == ')' {
				inParen = false
			}
			continue
		}
		switch r {
		case '(':
			inParen = true
		case ';':
			return finish(b.String())
		case ' ', '\t', '\r':
		default:
			b.WriteRune(r)
		}
	}
	return finish(b.String())
}

func finish(s string) string {
	s = strings.ToUpper(s)
	if s == "%" {
		return ""
	}
	return s
}

// maxLine is GRBL's line buffer (80 bytes) less the newline.
const maxLine = 79

// prepareJob turns uploaded G-code into lines ready to stream.
func prepareJob(text string) ([]string, error) {
	var lines []string
	for i, raw := range strings.Split(text, "\n") {
		line := cleanLine(raw)
		if line == "" {
			continue
		}
		if len(line) > maxLine {
			return nil, &jobError{"line " + strconv.Itoa(i+1) + " is longer than GRBL accepts (79 characters)"}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, &jobError{"there's no G-code in it"}
	}
	return lines, nil
}

type jobError struct{ msg string }

func (e *jobError) Error() string { return e.msg }

// grblErrors are the GRBL 1.1 error codes worth naming to an operator.
var grblErrors = map[int]string{
	1: "line has a value with no letter", 2: "number missing or badly formatted",
	3: "unknown $ command", 8: "$ command needs the machine idle",
	9: "G-code is locked out during alarm or jog", 11: "line too long",
	15: "jog target is outside the machine's travel", 20: "unsupported G-code command",
	22: "feed rate not set", 33: "motion target is invalid",
}

var grblAlarms = map[int]string{
	1: "hard limit hit — home the machine", 2: "move would go past the machine's travel",
	3: "reset while moving — position may be lost", 8: "homing failed: could not pull off the switch",
	9: "homing failed: limit switch not found",
}

func describe(table map[int]string, code int) string {
	if s, ok := table[code]; ok {
		return s
	}
	return "code " + strconv.Itoa(code)
}
