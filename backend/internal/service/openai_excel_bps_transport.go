package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/util/transportdiag"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

var errExcelBPSProxyUnavailable = errors.New("BPS proxy unavailable")

// Preserve local proxy classification after writing the client error without
// changing its log message or retaining an acquisition cause with credentials.
type excelBPSForwardError struct{ code string }

func (e *excelBPSForwardError) Error() string { return "excel BPS: " + e.code }
func (e *excelBPSForwardError) Unwrap() error {
	if e.code == "basispoints_proxy_unavailable" {
		return errExcelBPSProxyUnavailable
	}
	return nil
}

// The session-proxy pools (managed Mihomo / warm IP pool) are not part of this
// build: every BPS request uses the account's own proxy binding, mirroring the
// upstream behaviour when the per-account session proxy toggle is off.
type excelBPSLease interface {
	Release()
	ReportFailure()
	ReportStreamFailure()
	ReportSuccess()
	ReportUpstreamFailure()
}

type excelBPSSelfReleaseLease struct{}

func (excelBPSSelfReleaseLease) Release()               {}
func (excelBPSSelfReleaseLease) ReportFailure()         {}
func (excelBPSSelfReleaseLease) ReportStreamFailure()   {}
func (excelBPSSelfReleaseLease) ReportSuccess()         {}
func (excelBPSSelfReleaseLease) ReportUpstreamFailure() {}

type excelBPSAcquire func(context.Context, string, ...string) (string, excelBPSLease, error)

// accountExcelBPSAcquire pins the request to the account's proxy URL. The
// lease is a no-op: there is nothing to release or score without session pools.
func accountExcelBPSAcquire(account *Account) excelBPSAcquire {
	proxy := ""
	if account != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	return func(ctx context.Context, _ string, excluded ...string) (string, excelBPSLease, error) {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if len(excluded) != 0 {
			return "", nil, errExcelBPSProxyUnavailable
		}
		return proxy, excelBPSSelfReleaseLease{}, nil
	}
}

// Missing trace is not evidence of safety. Standard net/http emits GetConn
// before dialing and GotConn before handing a connection to request writing.
// Keep evidence for the whole attempt, including any internal reconnects.
type excelBPSWriteEvidence struct{ transportdiag.Trace }

func (e *excelBPSWriteEvidence) request(req *http.Request) *http.Request {
	req = e.Request(req)
	if req.Body != nil {
		req.Body = &excelBPSTrackedBody{ReadCloser: req.Body, mark: e.MarkBodyRead}
	}
	if getBody := req.GetBody; getBody != nil {
		req.GetBody = func() (io.ReadCloser, error) {
			body, err := getBody()
			if err != nil {
				return nil, err
			}
			return &excelBPSTrackedBody{ReadCloser: body, mark: e.MarkBodyRead}, nil
		}
	}
	return req
}
func (e *excelBPSWriteEvidence) unsent() bool { return e.DefinitelyUnsent() }

type excelBPSTrackedBody struct {
	io.ReadCloser
	mark func()
}

func (b *excelBPSTrackedBody) Read(p []byte) (int, error) {
	if len(p) > 0 {
		b.mark()
	}
	return b.ReadCloser.Read(p)
}

func (s *OpenAIGatewayService) doExcelBPSRequest(ctx context.Context, c *gin.Context, account *Account, scope string, body []byte, token, accountID string, acquire excelBPSAcquire) (*http.Response, excelBPSLease, string, error) {
	build := func(ctx context.Context) (*http.Request, error) {
		return newExcelBPSRequest(ctx, body, token, accountID)
	}
	response, lease, proxy, err := s.doExcelBPSRequestTo(ctx, c, account, scope, basispoints.ResponsesURL, build, acquire)
	if err == nil {
		s.guardExcelBPSProgress(ctx, response)
	}
	return response, lease, proxy, err
}

// doExcelBPSRequestTo applies the exit and no-replay rules to a BPS endpoint;
// build must return a fresh request for each attempt.
func (s *OpenAIGatewayService) doExcelBPSRequestTo(ctx context.Context, c *gin.Context, account *Account, scope, upstreamURL string, build func(context.Context) (*http.Request, error), acquire excelBPSAcquire) (*http.Response, excelBPSLease, string, error) {
	proxy := ""
	if account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	if acquire == nil {
		acquire = accountExcelBPSAcquire(account)
	}
	const attempt = 1
	if err := ctx.Err(); err != nil {
		return nil, nil, proxy, err
	}
	proxy, lease, err := acquire(ctx, scope)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, proxy, ctx.Err()
		}
		recordExcelBPSTransportFailureAt(ctx, c, account, upstreamURL, scope, proxy, err, "proxy_acquisition", attempt, false)
		return nil, nil, proxy, err
	}
	req, err := build(ctx)
	if err != nil {
		lease.Release()
		return nil, nil, proxy, err
	}
	c.Set("excel_bps_upstream_attempt", attempt)
	evidence := &excelBPSWriteEvidence{}
	resp, err := s.httpUpstream.Do(evidence.request(req), proxy, account.ID, account.Concurrency)
	if err == nil {
		return resp, lease, proxy, nil
	}
	// Even an unusual response+error result makes replay unsafe.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		lease.ReportFailure()
	}
	lease.Release()
	recordExcelBPSTransportFailureAt(ctx, c, account, upstreamURL, scope, proxy, err, "transport", attempt, false, evidence)
	return nil, nil, proxy, err
}

func recordExcelBPSTransportFailure(ctx context.Context, c *gin.Context, account *Account, scope, proxy string, err error, stage string, attempt int, retry bool, evidence ...*excelBPSWriteEvidence) {
	recordExcelBPSTransportFailureAt(ctx, c, account, basispoints.ResponsesURL, scope, proxy, err, stage, attempt, retry, evidence...)
}

func recordExcelBPSTransportFailureAt(ctx context.Context, c *gin.Context, account *Account, upstreamURL, scope, proxy string, err error, stage string, attempt int, retry bool, evidence ...*excelBPSWriteEvidence) {
	if isExcelBPSClientCancellation(c, err) {
		logger.FromContext(ctx).Info("excel_bps.client_canceled",
			zap.Int64("account_id", account.ID), zap.String("stage", stage))
		return
	}
	kind := transportdiag.Classify(err)
	if errors.Is(err, errExcelBPSProxyUnavailable) {
		kind = "proxy_unavailable"
	}
	digest := sha256.Sum256([]byte(scope))
	sessionHash := hex.EncodeToString(digest[:8])
	diagnostics := map[string]any{
		"error_kind": kind, "error_type": fmt.Sprintf("%T", err),
		"session_hash": sessionHash, "attempt": attempt, "retry_before_send": retry,
	}
	if len(evidence) > 0 && evidence[0] != nil {
		diagnostics["transport"] = evidence[0].Snapshot()
	}
	detail, _ := json.Marshal(diagnostics)
	message := "Excel BPS " + stage + " failed: " + kind
	// Keep UI client errors generic; persist only explicitly safe diagnostics.
	if !retry {
		setOpsUpstreamError(c, 0, message, string(detail))
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID,
		UpstreamURL: upstreamURL, Kind: "request_error", Stage: stage,
		Scope: "excel_bps", Reason: kind, Message: message, Detail: string(detail),
	})
	logger.FromContext(ctx).Warn("excel_bps.transport_failed",
		zap.Int64("account_id", account.ID), zap.String("stage", stage),
		zap.String("error_kind", kind), zap.String("error_type", fmt.Sprintf("%T", err)),
		zap.String("session_hash", sessionHash),
		zap.Int("attempt", attempt), zap.Bool("retry_before_send", retry),
		zap.Any("transport", diagnostics["transport"]))
}
