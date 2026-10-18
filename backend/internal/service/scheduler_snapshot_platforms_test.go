//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestSchedulerSnapshotPlatformsPreserveKiroAndGrok(t *testing.T) {
	platforms := schedulerSnapshotPlatforms()

	require.Contains(t, platforms, PlatformKiro)
	require.Contains(t, platforms, PlatformGrok)
	// 调度快照沿用组合优先级（gemini 先于 openai），与额度白名单展示顺序成员相同。
	// 946878cd7 曾要求两份名单逐项相等；3b89cc08d 起快照改走 CompositePrecedencePlatformIDs。
	require.Equal(t, domain.CompositePrecedencePlatformIDs(), platforms)
	require.ElementsMatch(t, AllowedQuotaPlatforms, platforms)
}
