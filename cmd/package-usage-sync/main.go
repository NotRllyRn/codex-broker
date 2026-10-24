// Build six self-contained zip bundles; no language runtime is needed on clients.
package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/NotRllyRn/codex-broker/internal/usagereport"
	"github.com/NotRllyRn/codex-broker/internal/usagesync"
)

func main() {
	if err := build(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func build() error {
	out := flag.String("out", "dist/usage-sync", "Bundle output directory")
	target := flag.String("target", "all", "all or OS-ARCH")
	flag.Parse()
	if err := os.MkdirAll(*out, 0755); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "usage-sync-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	checksums := []string{}
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			name := platform + "-" + arch
			if *target != "all" && *target != name {
				continue
			}
			fmt.Println("Packaging", name, "ccusage", usagereport.Version)
			suffix := ""
			if platform == "windows" {
				suffix = ".exe"
			}
			binary := filepath.Join(temp, "codex-broker-usage"+suffix)
			command := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", binary, "./cmd/codex-broker-usage")
			command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+platform, "GOARCH="+arch)
			if output, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("build: %w: %s", err, output)
			}
			helper, err := os.ReadFile(binary)
			if err != nil {
				return err
			}
			collector, err := usagesync.Native(context.Background(), platform, arch)
			if err != nil {
				return err
			}
			filename := "codex-broker-usage-" + name + ".zip"
			file, err := os.Create(filepath.Join(*out, filename))
			if err != nil {
				return err
			}
			archive := zip.NewWriter(file)
			files := []struct {
				name string
				data []byte
			}{{"codex-broker-usage" + suffix, helper}, {"ccusage" + suffix, collector}, {"INSTALL.txt", []byte("ccusage " + usagereport.Version + " (pinned). Keep both executables together.\nRun codex-broker-usage install --broker-url https://YOUR-BROKER\nWindows: .\\codex-broker-usage.exe install --broker-url https://YOUR-BROKER\nEnter the generated identity code at the private prompt.\nDocs: https://github.com/NotRllyRn/codex-broker/blob/main/docs/usage-sync.md\n")}}
			for _, item := range files {
				header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
				header.SetMode(0700)
				entry, err := archive.CreateHeader(header)
				if err != nil {
					return err
				}
				if _, err := entry.Write(item.data); err != nil {
					return err
				}
			}
			if err := archive.Close(); err != nil {
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(*out, filename))
			if err != nil {
				return err
			}
			hash := sha256.Sum256(data)
			checksums = append(checksums, fmt.Sprintf("%x  %s", hash, filename))
		}
	}
	if len(checksums) == 0 {
		return fmt.Errorf("unsupported target %s", *target)
	}
	return os.WriteFile(filepath.Join(*out, "SHA256SUMS"), []byte(strings.Join(checksums, "\n")+"\n"), 0644)
}
