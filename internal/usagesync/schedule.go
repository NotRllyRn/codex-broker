package usagesync

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf16"
)

func escaped(value string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(value))
	return out.String()
}
func systemdQuote(value string) string {
	return strconv.Quote(strings.ReplaceAll(strings.ReplaceAll(value, "%", "%%"), "$", "$$"))
}
func windowsQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\v\"") {
		return value
	}
	var result strings.Builder
	result.WriteByte('"')
	slashes := 0
	for _, ch := range value {
		if ch == '\\' {
			slashes++
			continue
		}
		if ch == '"' {
			result.WriteString(strings.Repeat("\\", slashes*2+1))
			result.WriteRune(ch)
		} else {
			result.WriteString(strings.Repeat("\\", slashes))
			result.WriteRune(ch)
		}
		slashes = 0
	}
	result.WriteString(strings.Repeat("\\", slashes*2))
	result.WriteByte('"')
	return result.String()
}
func label(directory string) string {
	hash := sha256.Sum256([]byte(directory))
	return fmt.Sprintf("codex-broker-usage-%x", hash[:6])
}

// scheduleContent is pure so all OS definitions can be checked on every CI host.
func scheduleContent(directory, platform, identity string) string {
	binary := filepath.Join(directory, "codex-broker-usage")
	if platform == "windows" {
		binary += ".exe"
	}
	args := []string{binary, "sync", "--state-dir", directory}
	switch platform {
	case "linux":
		return "[Unit]\nDescription=Codex Broker ccusage sync\n[Service]\nType=oneshot\nExecStart=" + strings.Join([]string{systemdQuote(args[0]), "sync", "--state-dir", systemdQuote(directory)}, " ") + "\n"
	case "darwin":
		arguments := ""
		for _, arg := range args {
			arguments += "<string>" + escaped(arg) + "</string>"
		}
		log := escaped(filepath.Join(directory, "sync.log"))
		return `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>` + label(directory) + `</string><key>ProgramArguments</key><array>` + arguments + `</array><key>RunAtLoad</key><true/><key>StartInterval</key><integer>3600</integer><key>StandardOutPath</key><string>` + log + `</string><key>StandardErrorPath</key><string>` + log + `</string></dict></plist>`
	case "windows":
		return `<?xml version="1.0" encoding="UTF-16"?><Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + escaped(identity) + `</UserId></LogonTrigger><CalendarTrigger><Repetition><Interval>PT1H</Interval></Repetition><StartBoundary>2026-01-01T00:00:00</StartBoundary><Enabled>true</Enabled><ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay></CalendarTrigger></Triggers><Principals><Principal id="Author"><UserId>` + escaped(identity) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><ExecutionTimeLimit>PT30M</ExecutionTimeLimit></Settings><Actions Context="Author"><Exec><Command>` + escaped(binary) + `</Command><Arguments>` + escaped("sync --state-dir "+windowsQuote(directory)) + `</Arguments></Exec></Actions></Task>`
	}
	return ""
}

func schedule(directory string, remove bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	name := label(directory)
	run := func(program string, args ...string) error {
		command := exec.Command(program, args...)
		if data, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", program, err, data)
		}
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", name+".plist")
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_ = run("launchctl", "bootout", domain+"/"+name)
		if remove {
			return removeIfExists(path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(scheduleContent(directory, "darwin", "")), 0600); err != nil {
			return err
		}
		return run("launchctl", "bootstrap", domain, path)
	case "windows":
		if remove {
			return run("schtasks", "/Delete", "/TN", name, "/F")
		}
		current, err := user.Current()
		if err != nil {
			return err
		}
		data := []byte{0xff, 0xfe}
		for _, word := range utf16.Encode([]rune(scheduleContent(directory, "windows", current.Uid))) {
			data = binary.LittleEndian.AppendUint16(data, word)
		}
		path := filepath.Join(directory, "schedule.xml")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return err
		}
		defer os.Remove(path)
		return run("schtasks", "/Create", "/TN", name, "/XML", path, "/F")
	case "linux":
		return scheduleLinux(home, directory, remove, schedulerCommand, cronRunning)

	}
	return errors.New("unsupported scheduler; install with --no-schedule and schedule the sync command yourself")
}
func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
