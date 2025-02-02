package osin

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

// AccessRequestType is the type for OAuth param `grant_type`
type AccessRequestType string

const (
	AUTHORIZATION_CODE AccessRequestType = "authorization_code"
	REFRESH_TOKEN      AccessRequestType = "refresh_token"
	PASSWORD           AccessRequestType = "password"
	CLIENT_CREDENTIALS AccessRequestType = "client_credentials"
	ASSERTION          AccessRequestType = "assertion"
	ANONYMOUS          AccessRequestType = "anonymous"
	DEVICE             AccessRequestType = "device"
	PLATFORM           AccessRequestType = "platform"
	IMPLICIT           AccessRequestType = "__implicit"
)

// AccessRequest is a request for access tokens
type AccessRequest struct {
	Type          AccessRequestType
	Code          string
	Client        Client
	AuthorizeData *AuthorizeData
	AccessData    *AccessData

	// Force finish to use this access data, to allow access data reuse
	ForceAccessData *AccessData
	RedirectUri     string
	Scope           string
	Username        string
	Password        string
	AssertionType   string
	Assertion       string

	// Set if request is authorized
	Authorized bool

	// Token expiration in seconds. Change if different from default
	Expiration int32

	// Refresh Token expiration in seconds. Change if different from default
	RefreshExpiration int32

	// Set if a refresh token should be generated
	GenerateRefresh bool

	// Data to be passed to storage. Not used by the library.
	UserData interface{}

	// HttpRequest *http.Request for special use
	HttpRequest *http.Request

	// Optional code_verifier as described in rfc7636
	CodeVerifier string

	// Skip set access_token and refresh_token cookies
	SkipSetCookie bool
}

// AccessData represents an access grant (tokens, expiration, client, etc)
type AccessData struct {
	// Client information
	Client Client

	// Authorize data, for authorization code
	AuthorizeData *AuthorizeData

	// Previous access data, for refresh token
	AccessData *AccessData

	// Access token
	AccessToken string

	// Refresh Token. Can be blank
	RefreshToken string

	// Token expiration in seconds
	ExpiresIn int32

	// Refresh Token expiration in seconds
	RefreshExpireIn int32

	// Requested scope
	Scope string

	// Redirect Uri from request
	RedirectUri string

	// Date created
	CreatedAt time.Time

	// Data to be passed to storage. Not used by the library.
	UserData interface{}
}

// IsExpired returns true if access expired
func (d *AccessData) IsExpired() bool {
	return d.IsExpiredAt(time.Now())
}

// IsExpiredAt returns true if access expires at time 't'
func (d *AccessData) IsExpiredAt(t time.Time) bool {
	return d.ExpireAt().Before(t)
}

// ExpireAt returns the expiration date
func (d *AccessData) ExpireAt() time.Time {
	return d.CreatedAt.Add(time.Duration(d.ExpiresIn) * time.Second)
}

// AccessTokenGen generates access tokens
type AccessTokenGen interface {
	GenerateAccessToken(data *AccessData, generaterefresh bool) (accesstoken string, refreshtoken string, err error)
}

// HandleAccessRequest is the http.HandlerFunc for handling access token requests
func (s *Server) HandleAccessRequest(w *Response, r *http.Request) *AccessRequest {
	// Only allow GET or POST
	if r.Method == "GET" {
		if !s.Config.AllowGetAccessRequest {
			w.SetError(E_INVALID_REQUEST, "")
			w.InternalError = errors.New("Request must be POST")
			return nil
		}
	} else if r.Method != "POST" {
		w.SetError(E_INVALID_REQUEST, "")
		w.InternalError = errors.New("Request must be POST")
		return nil
	}

	err := r.ParseForm()
	if err != nil {
		w.SetError(E_INVALID_REQUEST, "")
		w.InternalError = err
		return nil
	}

	grantType := AccessRequestType(r.Form.Get("grant_type"))
	if s.Config.AllowedAccessTypes.Exists(grantType) {
		switch grantType {
		case AUTHORIZATION_CODE:
			return s.handleAuthorizationCodeRequest(w, r)
		case REFRESH_TOKEN:
			return s.handleRefreshTokenRequest(w, r)
		case PASSWORD:
			return s.handlePasswordRequest(w, r)
		case CLIENT_CREDENTIALS:
			return s.handleClientCredentialsRequest(w, r)
		case ASSERTION:
			return s.handleAssertionRequest(w, r)
		case ANONYMOUS:
			return s.handleAnonymousRequest(w, r)
		case DEVICE:
			return s.handleDeviceRequest(w, r)
		case PLATFORM:
			return s.handlePlatformRequest(w, r)
		}
	}

	w.SetError(E_UNSUPPORTED_GRANT_TYPE, "")
	return nil
}

func (s *Server) handleAuthorizationCodeRequest(w *Response, r *http.Request) *AccessRequest {
	auth, err := CheckBasicAuth(r)
	if err != nil {
		w.SetError(E_SERVER_ERROR, err.Error())
		w.InternalError = err
		return nil
	}

	var clientID string
	var client Client
	if auth == nil {
		clientID = r.Form.Get("client_id")
		if clientID == "" {
			w.SetError(E_UNAUTHORIZED_CLIENT, "missing client_id in form body")
			return nil
		}
		client = getClientWithoutSecret(clientID, w.Storage, w)
	} else {
		// get client authentication
		auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
		if auth == nil {
			return nil
		}
		client = getClient(auth, w.Storage, w)
	}

	// generate access token
	ret := &AccessRequest{
		Type:            AUTHORIZATION_CODE,
		Code:            r.Form.Get("code"),
		CodeVerifier:    r.Form.Get("code_verifier"),
		RedirectUri:     r.Form.Get("redirect_uri"),
		GenerateRefresh: true,
		Expiration:      s.Config.AccessExpiration,
		HttpRequest:     r,
	}

	// "code" is required
	if ret.Code == "" {
		w.SetError(E_INVALID_GRANT, "authorization code is empty")
		return nil
	}

	// must have a valid client
	if ret.Client = client; ret.Client == nil {
		return nil
	}

	// must be a valid authorization code
	ret.AuthorizeData, err = w.Storage.LoadAuthorize(ret.Code)
	if err != nil {
		w.SetError(E_INVALID_GRANT, "failed to load authorize data")
		w.InternalError = err
		return nil
	}
	if ret.AuthorizeData == nil {
		w.SetError(E_UNAUTHORIZED_CLIENT, "authorize data is empty")
		return nil
	}
	if ret.AuthorizeData.Client == nil {
		w.SetError(E_UNAUTHORIZED_CLIENT, "client is empty in authorize data")
		return nil
	}
	if ret.AuthorizeData.Client.GetRedirectURI() == "" {
		w.SetError(E_UNAUTHORIZED_CLIENT, "authorize client redirect uri is empty")
		return nil
	}
	if ret.AuthorizeData.IsExpiredAt(s.Now()) {
		w.SetError(E_INVALID_GRANT, "authorization code is expired")
		return nil
	}

	// code must be from the client
	if !CheckClientID(ret.AuthorizeData.Client, ret.Client.GetID()) {
		w.SetError(E_INVALID_GRANT, "authorize client id not match")
		return nil
	}

	// check redirect uri
	if ret.RedirectUri == "" {
		ret.RedirectUri = FirstUri(ret.Client.GetRedirectURI(), s.Config.RedirectUriSeparator)
	}
	if err = ValidateUriList(ret.Client.GetRedirectURI(), ret.RedirectUri, s.Config.RedirectUriSeparator); err != nil {
		w.SetError(E_INVALID_REQUEST, err.Error())
		w.InternalError = err
		return nil
	}
	if ret.AuthorizeData.RedirectUri != ret.RedirectUri {
		w.SetError(E_INVALID_REQUEST, "")
		w.InternalError = errors.New("redirect uri is different")
		return nil
	}

	// Verify PKCE, if present in the authorization data
	if len(ret.AuthorizeData.CodeChallenge) > 0 {
		// https: //tools.ietf.org/html/rfc7636#section-4.6
		codeVerifier := ""
		switch ret.AuthorizeData.CodeChallengeMethod {
		case "", PKCE_PLAIN:
			codeVerifier = ret.CodeVerifier
		case PKCE_S256:
			hash := sha256.Sum256([]byte(ret.CodeVerifier))
			codeVerifier = base64.RawURLEncoding.EncodeToString(hash[:])
		default:
			w.SetError(E_INVALID_REQUEST, "code_challenge_method transform algorithm not supported (rfc7636)")
			return nil
		}
		if codeVerifier != ret.AuthorizeData.CodeChallenge {
			w.SetError(E_INVALID_GRANT, "code_verifier invalid (rfc7636)")
			w.InternalError = errors.New("code_verifier failed comparison with code_challenge")
			return nil
		}
	}

	// set rest of data
	ret.Scope = ret.AuthorizeData.Scope
	ret.UserData = ret.AuthorizeData.UserData

	return ret
}

func extraScopes(access_scopes, refresh_scopes string) bool {
	access_scopes_list := strings.Split(access_scopes, ",")
	refresh_scopes_list := strings.Split(refresh_scopes, ",")

	access_map := make(map[string]int)

	for _, scope := range access_scopes_list {
		if scope == "" {
			continue
		}
		access_map[scope] = 1
	}

	for _, scope := range refresh_scopes_list {
		if scope == "" {
			continue
		}
		if _, ok := access_map[scope]; !ok {
			return true
		}
	}
	return false
}

func (s *Server) handleRefreshTokenRequest(w *Response, r *http.Request) *AccessRequest {
	// get client authentication
	auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
	if auth == nil {
		return nil
	}

	// generate access token
	refreshToken := r.Form.Get("refresh_token")
	if refreshToken == "" {
		refreshToken = getRefreshTokenCookie(r)
	}
	ret := &AccessRequest{
		Type:              REFRESH_TOKEN,
		Code:              refreshToken,
		Scope:             r.Form.Get("scope"),
		GenerateRefresh:   true,
		Expiration:        s.Config.AccessExpiration,
		RefreshExpiration: s.Config.RefreshExpiration,
		HttpRequest:       r,
	}

	// "refresh_token" is required
	if ret.Code == "" {
		w.SetError(E_INVALID_GRANT, "refresh_token is empty")
		return nil
	}

	// must have a valid client
	if ret.Client = getClient(auth, w.Storage, w); ret.Client == nil {
		return nil
	}

	// must be a valid refresh code
	var err error
	ret.AccessData, err = w.Storage.LoadRefresh(ret.Code)
	if err != nil {
		w.SetError(E_SERVER_ERROR, "failed to load refresh_token")
		w.InternalError = err
		return nil
	}
	if ret.AccessData == nil {
		w.SetError(E_INVALID_GRANT, "refresh_toke is invalid")
		return nil
	}
	if ret.AccessData.Client == nil {
		w.SetError(E_INVALID_GRANT, "accessData client is empty")
		return nil
	}
	if ret.AccessData.Client.GetRedirectURI() == "" {
		w.SetError(E_INVALID_GRANT, "accessData client redirect uri is empty")
		return nil
	}

	// client must be the same as the previous token
	if !CheckClientID(ret.AccessData.Client, ret.Client.GetID()) {
		w.SetError(E_INVALID_GRANT, "client id must be the same from previous token")
		w.InternalError = errors.New("client id must be the same from previous token")
		return nil

	}

	// set rest of data
	ret.RedirectUri = ret.AccessData.RedirectUri
	ret.UserData = ret.AccessData.UserData
	if ret.Scope == "" {
		ret.Scope = ret.AccessData.Scope
	}

	if extraScopes(ret.AccessData.Scope, ret.Scope) {
		w.SetError(E_ACCESS_DENIED, "the requested scope must not include any scope not originally granted by the resource owner")
		w.InternalError = errors.New("the requested scope must not include any scope not originally granted by the resource owner")
		return nil
	}

	return ret
}

func (s *Server) handlePasswordRequest(w *Response, r *http.Request) *AccessRequest {
	// get client authentication
	auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
	if auth == nil {
		return nil
	}

	// generate access token
	ret := &AccessRequest{
		Type:              PASSWORD,
		Username:          r.Form.Get("username"),
		Password:          r.Form.Get("password"),
		Scope:             r.Form.Get("scope"),
		GenerateRefresh:   true,
		Expiration:        s.Config.AccessExpiration,
		RefreshExpiration: s.Config.RefreshExpiration,
		HttpRequest:       r,
	}

	// "username" and "password" is required
	if ret.Username == "" || ret.Password == "" {
		w.SetError(E_INVALID_GRANT, "username and password is required")
		return nil
	}

	// must have a valid client
	if ret.Client = getClient(auth, w.Storage, w); ret.Client == nil {
		return nil
	}

	// set redirect uri
	ret.RedirectUri = FirstUri(ret.Client.GetRedirectURI(), s.Config.RedirectUriSeparator)

	return ret
}

func (s *Server) handleAnonymousRequest(w *Response, r *http.Request) *AccessRequest {
	// get client authentication
	auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
	if auth == nil {
		return nil
	}

	// generate access token
	ret := &AccessRequest{
		Type:              ANONYMOUS,
		Username:          r.Form.Get("user_id"),
		Scope:             r.Form.Get("scope"),
		GenerateRefresh:   true,
		Expiration:        s.Config.AccessExpiration,
		RefreshExpiration: s.Config.RefreshExpiration,
		HttpRequest:       r,
	}

	// must have a valid client
	if ret.Client = getClient(auth, w.Storage, w); ret.Client == nil {
		return nil
	}

	// set redirect uri
	ret.RedirectUri = FirstUri(ret.Client.GetRedirectURI(), s.Config.RedirectUriSeparator)

	return ret
}

func (s *Server) handleDeviceRequest(w *Response, r *http.Request) *AccessRequest {
	// get client authentication
	auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
	if auth == nil {
		return nil
	}

	// generate access token
	ret := &AccessRequest{
		Type:              DEVICE,
		Password:          r.Form.Get("device_id"),
		Scope:             r.Form.Get("scope"),
		GenerateRefresh:   true,
		Expiration:        s.Config.AccessExpiration,
		RefreshExpiration: s.Config.RefreshExpiration,
		HttpRequest:       r,
	}

	// must have a valid client
	if ret.Client = getClient(auth, w.Storage, w); ret.Client == nil {
		return nil
	}

	// set redirect uri
	ret.RedirectUri = FirstUri(ret.Client.GetRedirectURI(), s.Config.RedirectUriSeparator)

	return ret
}

func (s *Server) handlePlatformRequest(w *Response, r *http.Request) *AccessRequest {
	auth, err := CheckBasicAuth(r)
	if err != nil {
		w.SetError(E_INVALID_REQUEST, "")
		w.InternalError = err
		return nil
	}

	var clientID string
	var client Client
	if auth == nil {
		clientID = r.Form.Get("client_id")
		if clientID == "" {
			w.SetError(E_UNAUTHORIZED_CLIENT, "client_id is empty in form body")
			return nil
		}
		client = getClientWithoutSecret(clientID, w.Storage, w)
	} else {
		// get client authentication
		auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
		if auth == nil {
			return nil
		}
		client = getClient(auth, w.Storage, w)
	}

	// generate access token
	ret := &AccessRequest{
		Type:              PLATFORM,
		Password:          r.Form.Get("platform_token"),
		Scope:             r.Form.Get("scope"),
		GenerateRefresh:   true,
		Expiration:        s.Config.AccessExpiration,
		RefreshExpiration: s.Config.RefreshExpiration,
		HttpRequest:       r,
	}

	// must have a valid client
	if ret.Client = client; ret.Client == nil {
		return nil
	}

	// set redirect uri
	ret.RedirectUri = FirstUri(ret.Client.GetRedirectURI(), s.Config.RedirectUriSeparator)

	return ret
}

func (s *Server) handleClientCredentialsRequest(w *Response, r *http.Request) *AccessRequest {
	// get client authentication
	auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
	if auth == nil {
		return nil
	}
	if len(auth.Password) == 0 {
		w.SetError(E_INVALID_GRANT, "client secret is empty")
		return nil
	}

	// generate access token
	ret := &AccessRequest{
		Type:            CLIENT_CREDENTIALS,
		Scope:           r.Form.Get("scope"),
		GenerateRefresh: false,
		Expiration:      s.Config.AccessExpiration,
		HttpRequest:     r,
		SkipSetCookie:   true,
	}

	// must have a valid client
	if ret.Client = getClient(auth, w.Storage, w); ret.Client == nil {
		return nil
	}

	// set redirect uri
	ret.RedirectUri = FirstUri(ret.Client.GetRedirectURI(), s.Config.RedirectUriSeparator)

	return ret
}

func (s *Server) handleAssertionRequest(w *Response, r *http.Request) *AccessRequest {
	// get client authentication
	auth := GetClientAuth(w, r, s.Config.AllowClientSecretInParams)
	if auth == nil {
		return nil
	}

	// generate access token
	ret := &AccessRequest{
		Type:            ASSERTION,
		Scope:           r.Form.Get("scope"),
		AssertionType:   r.Form.Get("assertion_type"),
		Assertion:       r.Form.Get("assertion"),
		GenerateRefresh: false, // assertion should NOT generate a refresh token, per the RFC
		Expiration:      s.Config.AccessExpiration,
		HttpRequest:     r,
	}

	// "assertion_type" and "assertion" is required
	if ret.AssertionType == "" || ret.Assertion == "" {
		w.SetError(E_INVALID_GRANT, "")
		return nil
	}

	// must have a valid client
	if ret.Client = getClient(auth, w.Storage, w); ret.Client == nil {
		return nil
	}

	// set redirect uri
	ret.RedirectUri = FirstUri(ret.Client.GetRedirectURI(), s.Config.RedirectUriSeparator)

	return ret
}

func (s *Server) FinishAccessRequest(w *Response, r *http.Request, ar *AccessRequest) {
	// don't process if is already an error
	if w.IsError {
		return
	}
	redirectUri := r.Form.Get("redirect_uri")
	// Get redirect uri from AccessRequest if it's there (e.g., refresh token request)
	if ar.RedirectUri != "" {
		redirectUri = ar.RedirectUri
	}
	if ar.Authorized {
		var ret *AccessData
		var err error

		if ar.ForceAccessData == nil {
			// generate access token
			ret = &AccessData{
				Client:          ar.Client,
				AuthorizeData:   ar.AuthorizeData,
				AccessData:      ar.AccessData,
				RedirectUri:     redirectUri,
				CreatedAt:       s.Now(),
				ExpiresIn:       ar.Expiration,
				RefreshExpireIn: ar.RefreshExpiration,
				UserData:        ar.UserData,
				Scope:           ar.Scope,
			}

			// generate access token
			ret.AccessToken, ret.RefreshToken, err = s.AccessTokenGen.GenerateAccessToken(ret, ar.GenerateRefresh)
			if err != nil {
				w.SetError(E_SERVER_ERROR, "")
				w.InternalError = err
				return
			}
		} else {
			ret = ar.ForceAccessData
		}

		// save access token
		if err = w.Storage.SaveAccess(ret); err != nil {
			w.SetError(E_SERVER_ERROR, "")
			w.InternalError = err
			return
		}

		// remove authorization token
		if ret.AuthorizeData != nil {
			w.Storage.RemoveAuthorize(ret.AuthorizeData.Code)
		}

		// remove previous access token
		if ret.AccessData != nil && !s.Config.RetainTokenAfterRefresh {
			w.Storage.RemoveAccess(ret.AccessData.AccessToken)
		}

		// output data
		w.Output["access_token"] = ret.AccessToken
		w.Output["token_type"] = s.Config.TokenType
		w.Output["expires_in"] = ret.ExpiresIn
		if ret.RefreshToken != "" {
			w.Output["refresh_token"] = ret.RefreshToken
			w.Output["refresh_expires_in"] = ret.RefreshExpireIn
			if !ar.SkipSetCookie {
				AddTokenInCookie(w, ret.RefreshToken, "refresh_token", int64(int32(time.Now().Unix())+ret.RefreshExpireIn), s.Config.CookieDomain)
			}
		}
		if ret.Scope != "" {
			w.Output["scope"] = ret.Scope
		}

		if !ar.SkipSetCookie {
			AddTokenInCookie(w, ret.AccessToken, "access_token", int64(int32(time.Now().Unix())+ret.ExpiresIn), s.Config.CookieDomain)
		}
	} else {
		w.SetError(E_ACCESS_DENIED, "")
	}
}

// Helper Functions

// getClient looks up and authenticates the basic auth using the given
// storage. Sets an error on the response if auth fails or a server error occurs.
func getClient(auth *BasicAuth, storage Storage, w *Response) Client {
	client, err := storage.GetClient(auth.Username)
	if err != nil && err != ErrNotFound {
		w.SetError(E_SERVER_ERROR, "failed to get oauth client")
		w.InternalError = err
		return nil
	}
	if client == nil {
		w.SetError(E_INVALID_CLIENT, "oauth client is empty")
		return nil
	}

	if !CheckClientSecret(client, auth.Password) {
		w.SetError(E_INVALID_CLIENT, "oauth client secret not match")
		return nil
	}

	if client.GetRedirectURI() == "" {
		w.SetError(E_INVALID_CLIENT, "oauth client redirect uri is empty")
		return nil
	}
	return client
}

// getClientWithoutSecret looks up and authenticates the client using the given
// storage. Sets an error on the response if auth fails or a server error occurs.
func getClientWithoutSecret(clientId string, storage Storage, w *Response) Client {
	client, err := storage.GetClient(clientId)
	if err != nil {
		w.SetError(E_SERVER_ERROR, "failed to get oauth client")
		w.InternalError = err
		return nil
	}
	if client == nil {
		w.SetError(E_UNAUTHORIZED_CLIENT, "oauth client not found")
		return nil
	}

	if client.GetRedirectURI() == "" {
		w.SetError(E_UNAUTHORIZED_CLIENT, "oauth client redirect uri is empty")
		return nil
	}
	return client
}

// getRefreshTokenCookie get refresh token cookie from request header
func getRefreshTokenCookie(request *http.Request) string {
	refreshToken, err := request.Cookie("refresh_token")
	if err != nil {
		return ""
	}
	return refreshToken.Value
}
