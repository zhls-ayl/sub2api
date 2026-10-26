package service

import "github.com/Wei-Shaw/sub2api/internal/service/basispoints"

// excelBPSNativeFallbackReason names hosted-tool declarations that BPS cannot
// serve. The bridge always omits them with a capability notice; only the BPS
// tool probe treats them as a reason the probe path is unavailable.
func (a *Account) excelBPSNativeFallbackReason(body []byte) string {
	return basispoints.NativeFallbackReason(body)
}
