package usagesync

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fakeScheduler struct {
	userSystemd bool
	crontab     string
	readErr     error
	calls       []string
}

func (f *fakeScheduler) run(input []byte, program string, args ...string) ([]byte, error) {
	call := program + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if program == "systemctl" {
		if !f.userSystemd {
			return nil, errors.New("no user bus")
		}
		return nil, nil
	}
	if call == "crontab -l" {
		return []byte(f.crontab), f.readErr
	}
	if call == "crontab -" {
		f.crontab = string(input)
		return nil, nil
	}
	return nil, errors.New("unexpected command: " + call)
}

func TestLinuxCronFallbackAndUninstall(t *testing.T) {
	home, directory := t.TempDir(), t.TempDir()
	original := "MAILTO=\"\"\n17 * * * * /existing/job\n"
	f := &fakeScheduler{crontab: original}
	units := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(units, 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(units, label(directory)+".timer")
	if err := os.WriteFile(stale, []byte("old failed install"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := scheduleLinux(home, directory, false, f.run, func() bool { return true }); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(f.crontab, "# BEGIN "+label(directory)) != 1 || !strings.Contains(f.crontab, original) {
		t.Fatal("duplicate job or lost existing jobs", f.crontab)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale systemd unit retained", err)
	}
	for range 2 {
		if err := scheduleLinux(home, directory, true, f.run, func() bool { return false }); err != nil {
			t.Fatal(err)
		}
	}
	if f.crontab != original {
		t.Fatal("uninstall changed other jobs", f.crontab)
	}
}

func TestLinuxSystemdPreferredAndRemovesFallback(t *testing.T) {
	home, directory := t.TempDir(), t.TempDir()
	cron, err := cronContent("5 * * * * /other/job\n", directory, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeScheduler{userSystemd: true, crontab: cron}
	if err := scheduleLinux(home, directory, false, f.run, func() bool { t.Fatal("unexpected cron daemon probe"); return false }); err != nil {
		t.Fatal(err)
	}
	if f.crontab != "5 * * * * /other/job\n" {
		t.Fatal("fallback not removed")
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "enable --now "+label(directory)+".timer") {
		t.Fatal("timer not enabled")
	}
	if err := scheduleLinux(home, directory, true, f.run, func() bool { return false }); err != nil {
		t.Fatal(err)
	}
}

func TestCronFailuresDoNotOverwriteJobs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		text    string
		err     error
	}{
		{"stopped daemon", false, "existing", nil},
		{"permission denied", true, "permission denied", errors.New("exit status 1")},
		{"missing crontab", true, "", exec.ErrNotFound},
		{"malformed block", true, "# BEGIN " + label("/example") + "\nother job\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeScheduler{crontab: tc.text, readErr: tc.err}
			if err := scheduleLinux(t.TempDir(), "/example", false, f.run, func() bool { return tc.running }); err == nil {
				t.Fatal("expected actionable error")
			}
			if f.crontab != tc.text {
				t.Fatal("modified crontab on failure")
			}
		})
	}
}

func TestCronEmptyTableAndSpecialPaths(t *testing.T) {
	directory := "/example/a 'quoted' 50% \\ path"
	f := &fakeScheduler{crontab: "no crontab for example\n", readErr: errors.New("exit status 1")}
	if err := scheduleCron(directory, false, f.run, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.crontab, "50\\%") || !strings.Contains(f.crontab, "'\"'\"'") {
		t.Fatal("unsafe quoting", f.crontab)
	}
	if _, err := cronContent("", "/bad\npath", false); err == nil {
		t.Fatal("accepted newline injection")
	}
	other, err := cronContent(f.crontab, "/another-install", false)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := cronContent(other, directory, true)
	if err != nil || !strings.Contains(removed, label("/another-install")) || strings.Contains(removed, label(directory)) {
		t.Fatal("removed wrong installation", err)
	}
}
