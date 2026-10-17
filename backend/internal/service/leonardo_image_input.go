package service

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	leonardo "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

// 输入图（参考图）的抓取限制。
const (
	leonardoInputImageMaxBytes = 20 << 20 // 20 MiB
	leonardoInputImageTimeout  = 30 * time.Second
)

var errLeonardoInputImageBlocked = errors.New("input image url is not allowed")
var errLeonardoInputImageFetchFailed = errors.New("fetch input image failed")

// 面向调用方的固定文案：不回显连接错误、解析出的 IP 等内部细节。
const (
	leonardoInputImageBlockedUserMessage = "input image url must be a public https domain on port 443 without redirects"
	leonardoInputImageFetchUserMessage   = "failed to fetch input image"
)

// leonardoInputImage 是一张待上传到 Leonardo 的参考图。
type leonardoInputImage struct {
	Data        []byte
	ContentType string
}

// leonardoInputImagePort 是输入图 URL 唯一允许的端口。
const leonardoInputImagePort = "443"

// newLeonardoInputImageClient 构造抓取参考图用的 HTTP 客户端。
//
// 安全要点（SSRF 三层防护）：请求前限定 https+域名+443；Dialer.Control 校验
// 实际连接的 IP（挡 DNS rebinding）；完全不跟随重定向。
func newLeonardoInputImageClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   leonardoInputImageDialControl,
	}
	transport := &http.Transport{
		// 显式不走代理：经代理时 Dialer.Control 看到的是代理地址，校验会失效。
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   leonardoInputImageTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// leonardoInputImageDialControl 在建立连接前校验目标 IP 与端口。
func leonardoInputImageDialControl(_, address string, _ syscall.RawConn) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errLeonardoInputImageBlocked
	}
	if port != leonardoInputImagePort {
		return fmt.Errorf("%w: port %s", errLeonardoInputImageBlocked, port)
	}
	ip := net.ParseIP(host)
	if ip == nil || !isLeonardoPublicUnicastIP(ip) {
		return fmt.Errorf("%w: %s", errLeonardoInputImageBlocked, host)
	}
	return nil
}

// isLeonardoPublicUnicastIP 判定 IP 是否为可对外路由的单播地址。
func isLeonardoPublicUnicastIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsInterfaceLocalMulticast())
}

// validateLeonardoInputImageURL 校验参考图 URL：只接受 https、域名主机（禁止
// IP 字面量）、443 端口。
func validateLeonardoInputImageURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: malformed url", errLeonardoInputImageBlocked)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("%w: scheme must be https, got %q", errLeonardoInputImageBlocked, parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: userinfo is not allowed", errLeonardoInputImageBlocked)
	}
	if port := parsed.Port(); port != "" && port != leonardoInputImagePort {
		return nil, fmt.Errorf("%w: port %s", errLeonardoInputImageBlocked, port)
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return nil, fmt.Errorf("%w: missing host", errLeonardoInputImageBlocked)
	}
	if strings.Contains(host, "%") || net.ParseIP(host) != nil {
		return nil, fmt.Errorf("%w: ip address hosts are not allowed: %s", errLeonardoInputImageBlocked, host)
	}
	if urlvalidator.IsBlockedHost(host) || !isLeonardoInputImageDomain(host) {
		return nil, fmt.Errorf("%w: host %q is not a public domain", errLeonardoInputImageBlocked, host)
	}
	parsed.Scheme = "https"
	return parsed, nil
}

// isLeonardoInputImageDomain 判定 host 是否为合法的多级 ASCII 域名（挡
// 127.1、0x7f.1、0177.0.0.1 等数字 IP 写法与单 label 内网名）。
func isLeonardoInputImageDomain(host string) bool {
	if len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	return tld[0] >= 'a' && tld[0] <= 'z'
}

// fetchLeonardoInputImage 把客户端给的图片引用取成字节。data: URL 本地解码。
func fetchLeonardoInputImage(ctx context.Context, client *http.Client, rawURL string) (*leonardoInputImage, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, errors.New("input image url is empty")
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		return decodeLeonardoDataURL(trimmed)
	}
	parsed, err := validateLeonardoInputImageURL(trimmed)
	if err != nil {
		return nil, err
	}
	return doFetchLeonardoInputImage(ctx, client, parsed)
}

func doFetchLeonardoInputImage(ctx context.Context, client *http.Client, target *url.URL) (*leonardoInputImage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build input image request: %w", err)
	}
	req.Header.Set("Accept", "image/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errLeonardoInputImageFetchFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("%w: redirects are not allowed (status %d)", errLeonardoInputImageBlocked, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: unexpected status %d", errLeonardoInputImageFetchFailed, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, leonardoInputImageMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %w", errLeonardoInputImageFetchFailed, err)
	}
	if len(data) > leonardoInputImageMaxBytes {
		return nil, fmt.Errorf("input image exceeds %d bytes", leonardoInputImageMaxBytes)
	}
	if len(data) == 0 {
		return nil, errors.New("input image is empty")
	}

	contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = detectedImageContentType(data)
		if contentType == "" {
			return nil, errors.New("input image url did not return an image")
		}
	}
	return &leonardoInputImage{Data: data, ContentType: contentType}, nil
}

func decodeLeonardoDataURL(rawURL string) (*leonardoInputImage, error) {
	_, payload, found := strings.Cut(rawURL, ",")
	if !found {
		return nil, errors.New("malformed data url")
	}
	meta := rawURL[len("data:"):strings.Index(rawURL, ",")]
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, errors.New("data url must be base64 encoded")
	}
	payload = strings.TrimSpace(payload)
	if base64.StdEncoding.DecodedLen(len(payload)) > leonardoInputImageMaxBytes+2 {
		return nil, fmt.Errorf("input image exceeds %d bytes", leonardoInputImageMaxBytes)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("decode data url: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("input image is empty")
	}
	if len(data) > leonardoInputImageMaxBytes {
		return nil, fmt.Errorf("input image exceeds %d bytes", leonardoInputImageMaxBytes)
	}

	contentType := strings.TrimSpace(strings.Split(meta, ";")[0])
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		contentType = detectedImageContentType(data)
		if contentType == "" {
			return nil, errors.New("data url does not carry an image")
		}
	}
	return &leonardoInputImage{Data: data, ContentType: contentType}, nil
}

// leonardoInputImageRequestError 把取图错误包装成 Leonardo 终态错误。
func leonardoInputImageRequestError(err error) *leonardo.RequestError {
	reqErr := leonardo.NewRequestError(err.Error())
	switch {
	case errors.Is(err, errLeonardoInputImageBlocked):
		reqErr.UserMessage = leonardoInputImageBlockedUserMessage
	case errors.Is(err, errLeonardoInputImageFetchFailed):
		reqErr.UserMessage = leonardoInputImageFetchUserMessage
	}
	return reqErr
}
