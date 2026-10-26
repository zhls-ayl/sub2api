package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// IsBPSOAuth reports a BPS-only OpenAI account: authorized through the official
// Excel client, it can only serve requests through Excel BPS and never native
// Codex. This build has no independent Excel grant for Codex OAuth accounts.
func (a *Account) IsBPSOAuth() bool {
	return a != nil && a.IsOpenAI() && a.Type == AccountTypeBPSOAuth
}

// IsOpenAIOAuthCredentialAccount reports an OpenAI account that holds OAuth
// credentials (access/refresh token, client_id) maintained by the OpenAI token
// lifecycle: Codex OAuth accounts and BPS-only accounts alike.
func (a *Account) IsOpenAIOAuthCredentialAccount() bool {
	return a != nil && a.IsOpenAI() && (a.Type == AccountTypeOAuth || a.Type == AccountTypeBPSOAuth)
}

// excelBPSRouteEligible: only BPS-only parent accounts on a paid plan route
// through BPS.
func excelBPSRouteEligible(account *Account) bool {
	return account.IsBPSOAuth() && !account.IsShadow() &&
		!strings.EqualFold(strings.TrimSpace(account.GetCredential("plan_type")), "free")
}

// validateBPSOAuthCredentials keeps the account type and the OAuth client in
// step: BPS accepts Excel-client tokens only, and an Excel token can never
// serve native Codex.
func validateBPSOAuthCredentials(platform, accountType string, credentials map[string]any) error {
	if platform != PlatformOpenAI {
		if accountType == AccountTypeBPSOAuth {
			return infraerrors.BadRequest("BPS_OAUTH_INVALID", "BPS OAuth accounts must use the OpenAI platform")
		}
		return nil
	}
	clientID, _ := credentials["client_id"].(string)
	isExcel := strings.TrimSpace(clientID) == openai.ExcelClientID
	switch {
	case accountType == AccountTypeBPSOAuth && !isExcel:
		return infraerrors.BadRequest("BPS_OAUTH_INVALID", "BPS OAuth accounts require credentials issued to the official Excel client")
	case (accountType == AccountTypeOAuth || accountType == AccountTypeSetupToken) && isExcel:
		return infraerrors.BadRequest("BPS_OAUTH_INVALID", "Excel OAuth credentials can only be used by a BPS OAuth account")
	}
	return nil
}

// ExcelBPSDefaultModels are the models a BPS-only account serves while its
// model restriction (credentials.model_mapping) is empty. Anything else must be
// configured on the account, e.g. through "sync upstream models".
var ExcelBPSDefaultModels = []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra"}

func isExcelBPSDefaultModel(model string) bool {
	model = strings.TrimSpace(model)
	for _, candidate := range ExcelBPSDefaultModels {
		if candidate == model {
			return true
		}
	}
	return false
}

// validateBPSOAuthTypeChange keeps BPS-only accounts out of type changes. An
// update may drop client_id (it is not a preserved sensitive key) while the
// stored Excel access token stays, so the credential check alone cannot tell
// a converted account apart.
func validateBPSOAuthTypeChange(currentType, requestedType string) error {
	if requestedType == "" || requestedType == currentType {
		return nil
	}
	if currentType == AccountTypeBPSOAuth || requestedType == AccountTypeBPSOAuth {
		return infraerrors.BadRequest("BPS_OAUTH_INVALID", "BPS OAuth accounts cannot change type; create a new account instead")
	}
	return nil
}

type bpsRouteExcludedKey struct{}

// WithBPSUnsupportedInbound marks a request whose entry point has no Excel BPS
// route (Chat Completions, Anthropic Messages). Those handlers forward through
// the native Codex protocol, so the scheduler never selects a BPS-only account.
func WithBPSUnsupportedInbound(ctx context.Context) context.Context {
	return WithBPSRouteExcluded(ctx)
}

// WithBPSRouteExcluded keeps the rest of a request off BPS once BPS turned out
// unable to carry it (ExcelBPSImageUnsupportedReason): later account switches
// skip BPS-only accounts.
func WithBPSRouteExcluded(ctx context.Context) context.Context {
	return context.WithValue(ctx, bpsRouteExcludedKey{}, true)
}

// isBPSUnsupportedInbound reports a request that BPS must not serve, whether
// for its entry point or for its content.
func isBPSUnsupportedInbound(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(bpsRouteExcludedKey{}).(bool)
	return marked
}

// errBPSUnsupportedInbound is the forward-time backstop of the same rule.
var errBPSUnsupportedInbound = errors.New("BPS OAuth accounts only serve the Responses API")

const BPSUnsupportedInboundReason = GatewayFailureReason("basispoints_inbound_unsupported")

// newBPSUnsupportedInboundFailoverError lets the handler move on to an account
// that can serve the entry point if a BPS-only account was selected anyway.
func newBPSUnsupportedInboundFailoverError() *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		Scope:             GatewayFailureScopeAccount,
		Reason:            BPSUnsupportedInboundReason,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusBadGateway,
		ClientMessage:     errBPSUnsupportedInbound.Error(),
	}
}

var errExcelOAuthRouteUnavailable = errors.New("an enabled Excel BPS route is required for Excel OAuth credentials and the requested model; native Codex is unavailable")

const ExcelOAuthRouteUnavailableReason = GatewayFailureReason("excel_oauth_route_unavailable")

// validateExcelOAuthRoute rejects a BPS-only account for a model it cannot
// serve through BPS (outside its model restriction, a free plan or a shadow
// account).
func validateExcelOAuthRoute(account *Account, model string) error {
	if account.IsBPSOAuth() && (!account.IsExcelBPSEnabledForModel(model) || !account.IsModelSupported(model)) {
		return errExcelOAuthRouteUnavailable
	}
	return nil
}

// newExcelOAuthRouteFailoverError is the forward-time backstop for a BPS-only
// account that cannot serve this request (for example channel mapping or the
// account's model restriction changed after selection): the handler moves on
// to another account instead of answering 400.
func newExcelOAuthRouteFailoverError() *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		Scope:             GatewayFailureScopeAccount,
		Reason:            ExcelOAuthRouteUnavailableReason,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusBadGateway,
		ClientMessage:     errExcelOAuthRouteUnavailable.Error(),
	}
}
