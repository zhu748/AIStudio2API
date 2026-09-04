package sidecar

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// singboxVersion 是自动下载的 sing-box 版本(与生成的配置 schema 实测匹配)
const singboxVersion = "1.14.0"

// singboxChecksums 是发布包 sha256(GitHub Releases 实测下载校验)
var singboxChecksums = map[string]string{
	"linux-amd64": "2375de6999f4f56ab46b4fc5ddf26a6aba1d3e61a0f4e7ddec2f4690457d5f63",
	"linux-arm64": "04d9b40bc98dc55b6f509ce3292145c65478f65866bea64826ebb2f382385088",
}

// resolveBinary 定位 sing-box 可执行文件。
//
// 查找顺序:
//  1. SINGBOX_PATH 环境变量(显式路径)
//  2. /usr/local/bin/sing-box(Docker 镜像预置)
//  3. PATH(exec.LookPath)
//  4. 用户缓存目录 aistudio2api/sing-box(此前自动下载的产物)
//  5. allowDownload 且未禁用时自动下载(linux amd64/arm64)
//
// 找不到时返回带操作指引的错误。
func resolveBinary(allowDownload bool) (string, error) {
	if path := strings.TrimSpace(os.Getenv("SINGBOX_PATH")); path != "" {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && isExecutable(info) {
			return path, nil
		}
		return "", fmt.Errorf("SINGBOX_PATH 指向的文件不存在或不可执行: %s", path)
	}
	for _, candidate := range []string{"/usr/local/bin/sing-box"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && isExecutable(info) {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath("sing-box"); err == nil {
		return path, nil
	}
	if cached, err := cachedBinaryPath(); err == nil {
		if info, err := os.Stat(cached); err == nil && !info.IsDir() && isExecutable(info) {
			return cached, nil
		}
	}
	if allowDownload && downloadEnabled() {
		if path, err := downloadBinary(); err == nil {
			return path, nil
		} else {
			return "", err
		}
	}
	return "", fmt.Errorf("未找到 sing-box 可执行文件: 请安装 sing-box 后设置 SINGBOX_PATH 指向其二进制(Docker 镜像已预置;或设置 SINGBOX_DOWNLOAD=1 由程序自动下载 v%s)", singboxVersion)
}

// downloadEnabled 报告是否允许自动下载(SINGBOX_DOWNLOAD=0/false 关闭)
func downloadEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("SINGBOX_DOWNLOAD")))
	switch value {
	case "", "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// cachedBinaryPath 返回缓存目录中的 sing-box 路径
func cachedBinaryPath() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "aistudio2api", "sing-box"), nil
}

// downloadBinary 下载并安装固定版本的 sing-box 到用户缓存目录,
// 下载后校验 sha256,失败时不留半成品。
func downloadBinary() (string, error) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	checksum, ok := singboxChecksums[platform]
	if !ok {
		return "", fmt.Errorf("sing-box 自动下载仅支持 linux amd64/arm64(当前 %s): 请手动安装后设置 SINGBOX_PATH", platform)
	}
	target, err := cachedBinaryPath()
	if err != nil {
		return "", fmt.Errorf("定位缓存目录失败: %w", err)
	}
	// 已存在(可能是并发或此前的下载产物)
	if info, err := os.Stat(target); err == nil && !info.IsDir() && isExecutable(info) {
		return target, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("创建缓存目录失败: %w", err)
	}
	url := fmt.Sprintf("https://github.com/SagerNet/sing-box/releases/download/v%s/sing-box-%s-%s-%s.tar.gz",
		singboxVersion, singboxVersion, runtime.GOOS, runtime.GOARCH)
	archive, err := httpDownload(url)
	if err != nil {
		return "", fmt.Errorf("下载 sing-box v%s 失败(%s): %w", singboxVersion, runtime.GOOS+"/"+runtime.GOARCH, err)
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != checksum {
		return "", fmt.Errorf("sing-box 下载包 sha256 校验失败(预期 %s)", checksum)
	}
	binary, err := extractSingboxBinary(archive)
	if err != nil {
		return "", err
	}
	temp := target + ".tmp"
	if err := os.WriteFile(temp, binary, 0o755); err != nil {
		return "", fmt.Errorf("写入 sing-box 失败: %w", err)
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("安装 sing-box 失败: %w", err)
	}
	return target, nil
}

// httpDownload 拉取完整文件内容
func httpDownload(url string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 128<<20))
}

// extractSingboxBinary 从 release tar.gz 提取 sing-box 二进制
func extractSingboxBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("解压 sing-box 发布包失败: %w", err)
	}
	defer func() {
		_ = gz.Close()
	}()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 sing-box 发布包失败: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(header.Name) == "sing-box" {
			return io.ReadAll(io.LimitReader(reader, 128<<20))
		}
	}
	return nil, fmt.Errorf("sing-box 发布包中未找到二进制")
}

// isExecutable 判断文件是否具备可执行位
func isExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
