// Package rfc8693 implements the token exchange grant type per RFC 8693.
package rfc8693

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/token/jwt"
	"github.com/ory/x/errorsx"

	"go.infratographer.com/identity-api/internal/fositex"
	"go.infratographer.com/identity-api/internal/storage"
	"go.infratographer.com/identity-api/internal/types"
	"go.infratographer.com/x/urnx"
)

const (
	// GrantTypeTokenExchange is the grant type for token exchange per RFC 8693.
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	// TokenTypeJWT is the token type for JWT per RFC 8693.
	TokenTypeJWT = "urn:ietf:params:oauth:token-type:jwt"
	// ParamSubjectToken is the OAuth 2.0 request parameter for the subject token.
	ParamSubjectToken = "subject_token"
	// ParamSubjectTokenType is the OAuth 2.0 request parameter for the subject token type.
	ParamSubjectTokenType = "subject_token_type"
	// ParamActorToken is the OAuth 2.0 request parameter for the actor token.
	ParamActorToken = "actor_token"
	// ParamActorTokenType is the OAuth 2.0 request parameter for the actor token type.
	ParamActorTokenType = "actor_token_type"
	// ClaimClientID is the claim for the client ID.
	ClaimClientID = "client_id"

	responseIssuedTokenType = "issued_token_type"
)

var (
	// ErrJWKSURIProviderNotDefined is returned when the issuer JWKS URI provider is not defined.
	ErrJWKSURIProviderNotDefined = errors.New("no issuer JWKS URI provider defined")
)

func findMatchingKey(ctx context.Context, config fositex.OAuth2Configurator, token *jwt.Token) (interface{}, error) {
	var claims jwt.JWTClaims

	claims.FromMapClaims(token.Claims)

	issuer := claims.Issuer
	if len(issuer) == 0 {
		return nil, &jwt.ValidationError{
			Errors: jwt.ValidationErrorIssuer,
		}
	}

	jwksURIProvider := config.GetIssuerJWKSURIProvider(ctx)
	if jwksURIProvider == nil {
		return nil, &jwt.ValidationError{
			Errors: jwt.ValidationErrorUnverifiable,
			Inner:  ErrJWKSURIProviderNotDefined,
		}
	}

	jwksURI, err := jwksURIProvider.GetIssuerJWKSURI(ctx, issuer)
	if err != nil {
		return nil, &jwt.ValidationError{
			Errors: jwt.ValidationErrorIssuer,
			Inner:  err,
		}
	}

	jwks, err := config.GetJWKSFetcherStrategy(ctx).Resolve(ctx, jwksURI, false)
	if err != nil {
		return nil, &jwt.ValidationError{
			Errors: jwt.ValidationErrorUnverifiable,
			Inner:  err,
		}
	}

	kid, ok := token.Header["kid"].(string)
	if !ok {
		return nil, &jwt.ValidationError{
			Errors: jwt.ValidationErrorMalformed,
		}
	}

	keys := jwks.Key(kid)

	for _, key := range keys {
		if key.Use == "sig" {
			return key, nil
		}
	}

	err = &jwt.ValidationError{
		Errors: jwt.ValidationErrorSignatureInvalid,
	}

	return nil, err
}

// TokenExchangeHandler contains the logic for the token exchange grant type.
// it implements the fosite.TokenEndpointHandler interface.
type TokenExchangeHandler struct {
	accessTokenStrategy oauth2.AccessTokenStrategy
	accessTokenStorage  oauth2.AccessTokenStorage
	config              fositex.OAuth2Configurator
}

// implement the fosite.TokenEndpointHandler interface
var _ fosite.TokenEndpointHandler = new(TokenExchangeHandler)

// NewTokenExchangeHandler works as a fositex.Factory to register this handler.
var _ fositex.Factory = NewTokenExchangeHandler

// NewTokenExchangeHandler creates a new TokenExchangeHandler,
func NewTokenExchangeHandler(config fositex.OAuth2Configurator, storage any, strategy any) any {
	return &TokenExchangeHandler{
		accessTokenStrategy: strategy.(oauth2.AccessTokenStrategy),
		accessTokenStorage:  storage.(oauth2.AccessTokenStorage),
		config:              config,
	}
}

func (s *TokenExchangeHandler) validateJWT(ctx context.Context, token string) (*jwt.Token, error) {
	// Side effectful key finding isn't great but neither is parsing the JWT twice
	keyfunc := func(token *jwt.Token) (interface{}, error) {
		return findMatchingKey(ctx, s.config, token)
	}

	parsed, err := jwt.Parse(token, keyfunc)

	if err == nil {
		return parsed, nil
	}

	validationErr, ok := err.(*jwt.ValidationError)
	if !ok {
		return nil, errorsx.WithStack(fosite.ErrServerError.WithDebugf("Unknown error: %s", err))
	}

	switch validationErr.Errors {
	case jwt.ValidationErrorUnverifiable:
		return nil, errorsx.WithStack(fosite.ErrServerError.WithHintf("Server error: %s", err))
	default:
		return nil, errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("Invalid subject token: %s", err))
	}
}

func (s *TokenExchangeHandler) getSubjectClaims(ctx context.Context, token string) (*jwt.JWTClaims, error) {
	validated, err := s.validateJWT(ctx, token)

	if err != nil {
		return nil, err
	}

	var claims jwt.JWTClaims

	claims.FromMapClaims(validated.Claims)

	return &claims, nil
}

func (s *TokenExchangeHandler) getMappedSubjectClaims(ctx context.Context, claims *jwt.JWTClaims) (jwt.JWTClaimsContainer, error) {
	mappingStrategy := s.config.GetClaimMappingStrategy(ctx)

	mappedClaims, err := mappingStrategy.MapClaims(ctx, claims)
	if err != nil {
		return nil, err
	}

	return mappedClaims, nil
}

// HandleTokenEndpointRequest handles a RFC 8693 token request and provides a response that can be used to
// generate a token. Currently only supports JWT subject tokens and impersonation semantics.
func (s *TokenExchangeHandler) HandleTokenEndpointRequest(ctx context.Context, requester fosite.AccessRequester) error {
	form := requester.GetRequestForm()

	subjectToken := form.Get(ParamSubjectToken)
	if len(subjectToken) == 0 {
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("Missing required parameter '%s'.", ParamSubjectToken))
	}

	subjectTokenType := form.Get(ParamSubjectTokenType)
	if len(subjectTokenType) == 0 {
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("Missing required parameter '%s'.", ParamSubjectTokenType))
	}

	actorToken := form.Get(ParamActorToken)
	if len(actorToken) > 0 {
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("Delegation is not supported by this handler."))
	}

	switch subjectTokenType {
	case TokenTypeJWT:
		break
	default:
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("Unsupported subject token type '%s'.", subjectTokenType))
	}

	claims, err := s.getSubjectClaims(ctx, subjectToken)
	if err != nil {
		return err
	}

	mappedClaims, err := s.getMappedSubjectClaims(ctx, claims)
	if err != nil {
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("error mapping claims: %s", err))
	}

	issuer := claims.Issuer

	userInfoSvc := s.config.GetUserInfoStrategy(ctx)

	txManager, ok := userInfoSvc.(storage.TransactionManager)
	if !ok {
		return errorsx.WithStack(fosite.ErrServerError.WithHint("unable to find transaction manager"))
	}

	dbCtx, err := txManager.BeginContext(ctx)
	if err != nil {
		return errorsx.WithStack(fosite.ErrServerError.WithHint("could not start transaction"))
	}

	userInfo, err := s.populateUserInfo(dbCtx, issuer, claims.Subject, subjectToken)
	if err != nil {
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("unable to populate user info: %s", err))
	}

	userInfo, err = userInfoSvc.StoreUserInfo(dbCtx, userInfo)

	if err != nil {
		rbErr := txManager.RollbackContext(dbCtx)
		return errorsx.WithStack(fosite.ErrInvalidRequest.WithHintf("unable to store user info: %s / rollback error: %s", err, rbErr))
	}

	err = txManager.CommitContext(dbCtx)

	if err != nil {
		return errorsx.WithStack(fosite.ErrServerError.WithHintf("could not commit user info: %s", err))
	}

	var newClaims jwt.JWTClaims
	newClaims.Subject = s.formatSubject(userInfo)
	newClaims.Issuer = s.config.GetAccessTokenIssuer(ctx)

	for k, v := range mappedClaims.ToMapClaims() {
		newClaims.Add(k, v)
	}

	expiry := time.Now().Add(s.config.GetAccessTokenLifespan(ctx))
	expiryMap := map[fosite.TokenType]time.Time{
		fosite.AccessToken: expiry,
	}

	var clientID *string

	maybeClientID := requester.GetClient().GetID()
	if len(maybeClientID) > 0 {
		clientID = &maybeClientID
	}

	newClaims.Add(ClaimClientID, clientID)

	kid := s.config.GetSigningKey(ctx).KeyID

	headers := jwt.Headers{}
	headers.Add("kid", kid)

	session := oauth2.JWTSession{
		JWTHeader: &headers,
		JWTClaims: &newClaims,
		ExpiresAt: expiryMap,
		Subject:   claims.Subject,
	}

	userInfoAud, err := url.JoinPath(newClaims.Issuer, "userinfo")
	if err != nil {
		return errorsx.WithStack(fosite.ErrServerError.WithHintf("failed to build userinfo audience: %s", err))
	}

	requester.GrantAudience(userInfoAud)
	requester.SetSession(&session)

	return nil
}

// PopulateTokenEndpointResponse populates the response with a token.
func (s *TokenExchangeHandler) PopulateTokenEndpointResponse(ctx context.Context, requester fosite.AccessRequester, responder fosite.AccessResponder) error {
	if !s.CanHandleTokenEndpointRequest(ctx, requester) {
		return errorsx.WithStack(fosite.ErrUnknownRequest)
	}

	token, _, err := s.accessTokenStrategy.GenerateAccessToken(ctx, requester)
	if err != nil {
		return err
	}

	responder.SetAccessToken(token)
	responder.SetExtra(responseIssuedTokenType, TokenTypeJWT)
	responder.SetTokenType(fosite.BearerAccessToken)
	responder.SetExpiresIn(s.config.GetAccessTokenLifespan(ctx))

	return nil
}

// CanSkipClientAuth always returns true, as client auth is not required for token exchange.
func (s *TokenExchangeHandler) CanSkipClientAuth(_ context.Context, _ fosite.AccessRequester) bool {
	return true
}

// CanHandleTokenEndpointRequest returns true if the grant type is token exchange.
func (s *TokenExchangeHandler) CanHandleTokenEndpointRequest(_ context.Context, requester fosite.AccessRequester) bool {
	return requester.GetGrantTypes().ExactOne(GrantTypeTokenExchange)
}

func (s *TokenExchangeHandler) populateUserInfo(ctx context.Context, issuer string, subject string, token string) (types.UserInfo, error) {
	userInfoSvc := s.config.GetUserInfoStrategy(ctx)
	userInfo, err := userInfoSvc.LookupUserInfoByClaims(ctx, issuer, subject)

	if err != nil {
		// We can handle ErrUserInfoNotFound by hitting the
		// issuers userinfo endpoint, but if some other error
		// came back bail.
		if !errors.Is(err, types.ErrUserInfoNotFound) {
			fmt.Println("couldn't find issuer in lookup")
			return types.UserInfo{}, err
		}
	} else {
		return userInfo, nil
	}

	userInfo, err = userInfoSvc.FetchUserInfoFromIssuer(ctx, issuer, token)
	if err != nil {
		fmt.Println("failed to fetch userinfo")
		return types.UserInfo{}, err
	}

	return userInfo, nil
}

func (s *TokenExchangeHandler) formatSubject(info types.UserInfo) string {
	urn, err := urnx.Build(types.URNNamespace, types.URNResourceTypeUser, info.ID)
	if err != nil {
		// If for some reason we aren't building valid URNs, panic
		panic(err)
	}

	return urn.String()
}
