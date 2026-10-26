package basispoints

import (
	"encoding/base64"
	"fmt"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"strings"

	_ "golang.org/x/image/webp"
)

// Native attachment limits. The values and the data URL rules match the
// upstream package's image relay, whose temporary storage and download
// service this build does not include.
const (
	imageRelayMaxImageBytes    = 20 << 20
	imageRelayMaxRequestBytes  = 32 << 20
	imageRelayMaxRequestImages = 20
	imageRelayMaxPixels        = 64 * 1024 * 1024
)

func relayImagePayload(raw string, maxImageMiB int) (string, string, error) {
	header, payload, ok := strings.Cut(raw[len("data:"):], ",")
	if !ok || !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return "", "", fmt.Errorf("basispoints inline image requires a base64 image data URL")
	}
	declared, params, err := mime.ParseMediaType(header[:len(header)-len(";base64")])
	if err != nil || len(params) != 0 {
		return "", "", fmt.Errorf("basispoints inline image has an invalid media type")
	}
	switch declared {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return "", "", fmt.Errorf("basispoints inline images must be PNG, JPEG, GIF or WebP")
	}
	if len(payload) > base64.StdEncoding.EncodedLen(maxImageMiB<<20) {
		return "", "", fmt.Errorf("basispoints inline image exceeds the %d MiB limit", maxImageMiB)
	}
	if payload == "" {
		return "", "", fmt.Errorf("basispoints inline image contains invalid base64 data")
	}
	return declared, payload, nil
}
