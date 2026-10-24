package usagesync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type scheduleRunner func(input []byte, program string, args ...string) ([]byte, error)

func schedulerCommand(input []byte, program string, args ...string) ([]byte, error) {
	command := exec.Command(program, args...)
	command.Stdin = bytes.NewReader(input)
	command.Env = append(os.Environ(), "LC_ALL=C")
	data, err := command.CombinedOutput()
	if err != nil {
		return data, fmt.Errorf("%s: %w: %s", program, err, data)
	}
	return data, nil
}

// Check for a running daemon, not just a crontab executable that cannot run jobs.
func cronRunning() bool {
	paths, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil && (strings.TrimSpace(string(data)) == "cron" || strings.TrimSpace(string(data)) == "crond") {
			return true
		}
	}
	return false
}

func cronContent(existing, directory string, remove bool) (string, error) {
	if strings.ContainsAny(directory, "\r\n") {
		return "", errors.New("cron state directory cannot contain newlines")
	}
	start, end := "# BEGIN "+label(directory), "# END "+label(directory)
	var result []string
	inside := false
	for _, line := range strings.Split(strings.TrimSuffix(existing, "\n"), "\n") {
		if line == start {
			if inside {
				return "", errors.New("malformed usage sync crontab block")
			}
			inside = true
		} else if line == end {
			if !inside {
				return "", errors.New("malformed usage sync crontab block")
			}
			inside = false
		} else if !inside {
			result = append(result, line)
		}
	}
	if inside {
		return "", errors.New("unterminated usage sync crontab block; refusing to change other jobs")
	}
	if !remove {
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
		command := quote(filepath.Join(directory, "codex-broker-usage")) + " sync --state-dir " + quote(directory) + " >> " + quote(filepath.Join(directory, "sync.log")) + " 2>&1"
		// cron interprets percent signs even inside shell quotes.
		command = strings.ReplaceAll(command, "%", "\\%")
		result = append(result, start, "0 * * * * "+command, end)
	}
	return strings.Join(result, "\n") + "\n", nil
}

func scheduleCron(directory string, remove bool, run scheduleRunner, running func() bool) error {
	if !remove && !running() {
		return errors.New("no running cron/crond daemon; start cron with your system's service manager, or use --no-schedule and an external scheduler")
	}
	data, err := run(nil, "crontab", "-l")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) && remove {
			return nil
		}
		if !strings.HasPrefix(strings.TrimSpace(string(data)), "no crontab for ") {
			return fmt.Errorf("cannot read existing crontab: %w", err)
		}
		data = nil
	}
	if remove && !strings.Contains(string(data), "# BEGIN "+label(directory)) {
		return nil
	}
	content, err := cronContent(string(data), directory, remove)
	if err != nil {
		return err
	}
	_, err = run([]byte(content), "crontab", "-")
	return err
}

func scheduleLinux(home, directory string, remove bool, run scheduleRunner, running func() bool) error {
	name := label(directory)
	units := filepath.Join(home, ".config", "systemd", "user")
	_, probeErr := run(nil, "systemctl", "--user", "show-environment")
	cleanUnits := func() error {
		for _, path := range []string{filepath.Join(units, name+".service"), filepath.Join(units, name+".timer"), filepath.Join(units, "timers.target.wants", name+".timer")} {
			if err := removeIfExists(path); err != nil {
				return err
			}
		}
		return nil
	}
	if remove {
		if probeErr == nil {
			if _, err := os.Stat(filepath.Join(units, name+".timer")); err == nil {
				if _, err := run(nil, "systemctl", "--user", "disable", "--now", name+".timer"); err != nil {
					return err
				}
			}
		}
		if err := cleanUnits(); err != nil {
			return err
		}
		if probeErr == nil {
			if _, err := run(nil, "systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
		}
		return scheduleCron(directory, true, run, running)
	}
	if probeErr != nil {
		if err := scheduleCron(directory, false, run, running); err != nil {
			return fmt.Errorf("user systemd unavailable; cron fallback failed: %w", err)
		}
		return cleanUnits()
	}
	if err := os.MkdirAll(units, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(units, name+".service"), []byte(scheduleContent(directory, "linux", "")), 0600); err != nil {
		return err
	}
	timer := "[Unit]\nDescription=Upload ccusage hourly\n[Timer]\nOnStartupSec=2min\nOnCalendar=hourly\nPersistent=true\nRandomizedDelaySec=120\n[Install]\nWantedBy=timers.target\n"
	if err := os.WriteFile(filepath.Join(units, name+".timer"), []byte(timer), 0600); err != nil {
		return err
	}
	if _, err := run(nil, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := run(nil, "systemctl", "--user", "enable", "--now", name+".timer"); err != nil {
		return err
	}
	// Remove a previous fallback when a user service manager becomes available.
	return scheduleCron(directory, true, run, running)
}
