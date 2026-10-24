package usagesync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/NotRllyRn/codex-broker/internal/usagereport"
)

// npm's published SHA-512 integrity values are pinned alongside the version.
var integrities = map[string]string{
	"linux-amd64":   "C44a/Lay833Smjd2YV3SeqGCGCBjTXus81PHd949fOv6bbgw9xSUDdlJIzK1RT4MdlpOi8VmmnBydeMx9dlpng==",
	"linux-arm64":   "I5RnkWVB3nkHgw8Y5Ga8NhoPw951iWPxjQN6kfNryre10awVP7kTy/xcZ/H74GwO5qsjtz6/XCj9vvRZaXpBoQ==",
	"darwin-amd64":  "gnCOs5LMY/fDqumSZghQd3BuHCC2kqL4+pQehOs3jq1t4NuxL5DXga7vWpCQ4wXliBo3sHZyrd0TkTNEWkn2+A==",
	"darwin-arm64":  "Rjg8t6QN6TQM7fYeTW2TxZ273Jh9+ct6F8IqWc9sdUZMrNur9QlHh9RHPlgBKsU+AQ9yapa/UeN3+naGT5LEBw==",
	"windows-amd64": "DUUCQ/rdZedqW85ACZuC/Wamka4SRnrHInfmCjN5RWBzCBjFRJQyq8b3ZRJ42ct2f1a63s5IsLh0yQRdOWNVZg==",
	"windows-arm64": "RmLT545qBHG7tRRLCi/Kj3wJeVsPVWNKxfo2guIfKXpIOaL3g0VPydbzipLJuGxICAFOfwYz3lTExJ54BNZ30g==",
}

func Native(ctx context.Context, platform, arch string) ([]byte, error) {
	integrity, ok := integrities[platform+"-"+arch]
	if !ok {
		return nil, errors.New("unsupported platform; use Linux/macOS/Windows amd64 or arm64")
	}
	name := platform
	if name == "windows" {
		name = "win32"
	}
	cpu := arch
	if cpu == "amd64" {
		cpu = "x64"
	}
	address := fmt.Sprintf("https://registry.npmjs.org/@ccusage/ccusage-%s-%s/-/ccusage-%s-%s-%s.tgz", name, cpu, name, cpu, usagereport.Version)
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("native package redirect rejected") }}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("native package download HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	return unpackNative(data, integrity, platform)
}

func unpackNative(data []byte, integrity, platform string) ([]byte, error) {
	if len(data) > 32<<20 {
		return nil, errors.New("native package exceeds limit")
	}
	sum := sha512.Sum512(data)
	if base64.StdEncoding.EncodeToString(sum[:]) != integrity {
		return nil, errors.New("ccusage package SHA-512 mismatch")
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	tarReader := tar.NewReader(reader)
	target := "package/bin/ccusage"
	if platform == "windows" {
		target += ".exe"
	}
	for {
		header, err := tarReader.Next()
		if err != nil {
			return nil, err
		}
		if header.Name != target {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > 64<<20 {
			return nil, errors.New("invalid native executable")
		}
		return io.ReadAll(io.LimitReader(tarReader, header.Size))
	}
}
