package setup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cpa-ustc/internal/ustc"
)

// IwanRelease pins the independently maintained userspace helper reviewed for this release.
const IwanRelease = "v26.9.1"

// InstallIwan requires explicit invocation and verifies GitHub's SHA-256 asset digest.
// No root, shell scripts, system VPN changes or fallback to unsigned assets are used.
func InstallIwan(ctx context.Context, stateDir string) (string, error) {
	architecture := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	platform := map[string]string{"linux": "linux", "darwin": "macos", "windows": "windows"}[runtime.GOOS]
	if architecture == "" || platform == "" {
		return "", errors.New("此平台没有受支持的 iWAN 预编译客户端，请自行安装")
	}
	assetName := "iwan-client-oidc-" + platform + "-" + architecture
	if platform == "linux" {
		assetName += "-musl"
	}
	assetName += ".zip"
	client := &http.Client{Timeout: 2 * time.Minute}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/yyy1mu/ustc-iwan/releases/tags/"+IwanRelease, nil)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "cpa-ustc/"+ustc.Version)
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("无法读取 iWAN 发布信息，请手动安装或使用官方客户端")
	}
	defer response.Body.Close()
	var release struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release) != nil {
		return "", errors.New("无法读取 iWAN 发布信息（可能触发 GitHub 频率限制）")
	}
	for _, asset := range release.Assets {
		if asset.Name != assetName {
			continue
		}
		if !strings.HasPrefix(asset.Digest, "sha256:") || len(asset.Digest) != 71 || !strings.HasPrefix(asset.URL, "https://github.com/yyy1mu/ustc-iwan/releases/download/"+IwanRelease+"/") {
			return "", errors.New("iWAN 附件缺少可信 SHA-256 摘要或来源不匹配，拒绝安装")
		}
		request, _ = http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
		archive, downloadErr := client.Do(request)
		if downloadErr != nil {
			return "", errors.New("iWAN 附件下载失败")
		}
		data, readErr := io.ReadAll(io.LimitReader(archive.Body, 32<<20+1))
		archive.Body.Close()
		if readErr != nil || archive.StatusCode != 200 || len(data) > 32<<20 {
			return "", errors.New("iWAN 附件下载失败或大小异常")
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != strings.TrimPrefix(asset.Digest, "sha256:") {
			return "", errors.New("iWAN 附件 SHA-256 校验失败，拒绝安装")
		}
		bundle, zipErr := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if zipErr != nil {
			return "", zipErr
		}
		for _, entry := range bundle.File {
			base := filepath.Base(entry.Name)
			if entry.FileInfo().IsDir() || entry.Mode()&os.ModeSymlink != 0 || (!strings.HasPrefix(base, "iwan-client-oidc-") && base != "iwan-client-oidc" && base != "iwan-client-oidc.exe") {
				continue
			}
			if entry.UncompressedSize64 > 64<<20 {
				return "", errors.New("iWAN 可执行文件大小异常")
			}
			file, openErr := entry.Open()
			if openErr != nil {
				return "", openErr
			}
			binary, readErr := io.ReadAll(io.LimitReader(file, 64<<20+1))
			file.Close()
			if readErr != nil || len(binary) == 0 || len(binary) > 64<<20 {
				return "", errors.New("iWAN 可执行文件解压失败")
			}
			name := "iwan-client-oidc"
			if platform == "windows" {
				name += ".exe"
			}
			path := filepath.Join(stateDir, "bin", name)
			if err = ustc.WritePrivate(path, binary); err != nil {
				return "", err
			}
			if err = os.Chmod(path, 0700); err != nil {
				return "", err
			}
			return path, nil
		}
		return "", errors.New("iWAN 附件中没有预期的可执行文件")
	}
	return "", errors.New("此平台没有对应的 iWAN 发布附件，请手动安装")
}
