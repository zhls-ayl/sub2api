package handler

import (
	"context"
	"testing"
	"time"

	leonardopkg "github.com/Wei-Shaw/sub2api/internal/pkg/leonardo"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func leonardoFailoverFor(t *testing.T, failure *leonardopkg.GenerationFailure) *service.UpstreamFailoverError {
	t.Helper()
	failover := (*service.GatewayService)(nil).LeonardoFailover(context.Background(), 0, "", leonardopkg.ClassifyFailureErrorCode(failure))
	require.NotNil(t, failover)
	return failover
}

// 上游模型方审核拦截只重试一次；Leonardo 自身 NSFW 拦截不重试。
func TestLeonardoShouldRetryProviderModeration(t *testing.T) {
	provider := leonardoFailoverFor(t, &leonardopkg.GenerationFailure{ErrorCode: "PROVIDER_MODERATION_ERROR"})
	require.True(t, leonardoShouldRetryProviderModeration(provider, false))
	require.False(t, leonardoShouldRetryProviderModeration(provider, true), "每个请求至多重试一次")

	nsfw := leonardoFailoverFor(t, &leonardopkg.GenerationFailure{ErrorCode: "CC_NSFW_TOTAL_FAILURE"})
	require.False(t, leonardoShouldRetryProviderModeration(nsfw, false))

	temporary := leonardoFailoverFor(t, &leonardopkg.GenerationFailure{ErrorCode: "PROVIDER_TIMEOUT"})
	require.False(t, leonardoShouldRetryProviderModeration(temporary, false))
}

func TestLeonardoWaitStopsWhenClientLeaves(t *testing.T) {
	require.True(t, leonardoWait(context.Background(), time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, leonardoWait(ctx, time.Minute))
}
