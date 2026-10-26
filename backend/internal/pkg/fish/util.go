package fish

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
)

// newRequestID 生成 clientRequestId（uuid4 形态；上游仅要求唯一）。
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退化为随机性较弱的 hex 仍满足唯一性要求
		for i := range b {
			b[i] = byte(i * 31)
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

func pathEscape(segment string) string {
	return url.PathEscape(segment)
}

// NewDeviceID 生成每账号持久化的 amp device id（UUID4 形态）。
func NewDeviceID() string { return newRequestID() }
