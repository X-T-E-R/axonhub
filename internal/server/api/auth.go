package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

type AuthHandlersParams struct {
	fx.In

	AuthService *biz.AuthService
}

func NewAuthHandlers(params AuthHandlersParams) *AuthHandlers {
	return &AuthHandlers{
		AuthService: params.AuthService,
	}
}

type AuthHandlers struct {
	AuthService *biz.AuthService
}

// SignInRequest 登录请求.
type SignInRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// SignInResponse 登录响应.
type SignInResponse struct {
	User  *objects.UserInfo `json:"user"`
	Token string            `json:"token"`
}

type SignUpPolicyResponse struct {
	Enabled               bool `json:"enabled"`
	OIDCSignupAllowed     bool `json:"oidcSignupAllowed"`
	InviteCodeRequired    bool `json:"inviteCodeRequired"`
	PasswordSignupAllowed bool `json:"passwordSignupAllowed"`
	AllowRequestDetails   bool `json:"allowRequestDetails"`
}

type AdminRegistrationPolicyResponse struct {
	Enabled                bool     `json:"enabled"`
	OIDCEnabled            bool     `json:"oidcEnabled"`
	InviteCode             string   `json:"inviteCode"`
	InviteCodeRequired     bool     `json:"inviteCodeRequired"`
	DefaultProjectID       int      `json:"defaultProjectId"`
	AutoJoinFirstProject   bool     `json:"autoJoinFirstProject"`
	DefaultProjectScopes   []string `json:"defaultProjectScopes"`
	AllowRequestDetails    bool     `json:"allowRequestDetails"`
	SelfServicePresetNames []string `json:"selfServicePresetNames"`
	PasswordSignupAllowed  bool     `json:"passwordSignupAllowed"`
}

type UpdateRegistrationPolicyRequest struct {
	Enabled                bool     `json:"enabled"`
	OIDCEnabled            bool     `json:"oidcEnabled"`
	InviteCode             string   `json:"inviteCode"`
	DefaultProjectID       int      `json:"defaultProjectId"`
	AutoJoinFirstProject   bool     `json:"autoJoinFirstProject"`
	DefaultProjectScopes   []string `json:"defaultProjectScopes"`
	AllowRequestDetails    bool     `json:"allowRequestDetails"`
	SelfServicePresetNames []string `json:"selfServicePresetNames"`
}

type SignUpRequest struct {
	Email      string `json:"email" binding:"required,email"`
	Password   string `json:"password" binding:"required"`
	FirstName  string `json:"firstName"`
	LastName   string `json:"lastName"`
	InviteCode string `json:"inviteCode"`
}

// SignIn handles user authentication.
func (h *AuthHandlers) SignIn(c *gin.Context) {
	var (
		ctx = c.Request.Context()
		req SignInRequest
	)

	err := c.ShouldBindJSON(&req)
	if err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	// Authenticate user
	user, err := h.AuthService.AuthenticateUser(ctx, req.Email, req.Password)
	if err != nil {
		if errors.Is(err, biz.ErrInvalidPassword) {
			JSONError(c, http.StatusUnauthorized, errors.New("Invalid email or password"))
			return
		}

		JSONError(c, http.StatusInternalServerError, errors.New("Internal server error"))

		return
	}

	// Generate JWT token
	token, err := h.AuthService.GenerateJWTToken(ctx, user)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Internal server error"))
		return
	}

	response := SignInResponse{
		User:  biz.ConvertUserToUserInfo(ctx, user),
		Token: token,
	}

	c.JSON(http.StatusOK, response)
}

func (h *AuthHandlers) SignUpPolicy(c *gin.Context) {
	policy, err := h.AuthService.RegistrationPolicy(c.Request.Context())
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Failed to load registration policy"))
		return
	}

	c.JSON(http.StatusOK, SignUpPolicyResponse{
		Enabled:               policy.Enabled,
		OIDCSignupAllowed:     policy.OIDCEnabled,
		InviteCodeRequired:    policy.InviteCode != "",
		PasswordSignupAllowed: h.AuthService.PasswordRegistrationEnabled(),
		AllowRequestDetails:   policy.AllowRequestDetails,
	})
}

func (h *AuthHandlers) AdminRegistrationPolicy(c *gin.Context) {
	if !scopes.UserHasScope(c.Request.Context(), scopes.ScopeReadSettings) {
		JSONError(c, http.StatusForbidden, errors.New("permission denied: requires read_settings scope"))
		return
	}

	policy, err := h.AuthService.RegistrationPolicy(c.Request.Context())
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Failed to load registration policy"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": adminRegistrationPolicyResponse(policy, h.AuthService.PasswordRegistrationEnabled())})
}

func (h *AuthHandlers) UpdateRegistrationPolicy(c *gin.Context) {
	if !scopes.UserHasScope(c.Request.Context(), scopes.ScopeWriteSettings) {
		JSONError(c, http.StatusForbidden, errors.New("permission denied: requires write_settings scope"))
		return
	}

	var req UpdateRegistrationPolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	policy := biz.RegistrationConfig{
		Enabled:                req.Enabled,
		OIDCEnabled:            req.OIDCEnabled,
		InviteCode:             req.InviteCode,
		DefaultProjectID:       req.DefaultProjectID,
		AutoJoinFirstProject:   req.AutoJoinFirstProject,
		DefaultProjectScopes:   req.DefaultProjectScopes,
		AllowRequestDetails:    req.AllowRequestDetails,
		SelfServicePresetNames: req.SelfServicePresetNames,
	}

	if err := h.AuthService.SystemService.SetRegistrationConfig(c.Request.Context(), policy); err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}

	policy, err := h.AuthService.RegistrationPolicy(c.Request.Context())
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Failed to load registration policy"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": adminRegistrationPolicyResponse(policy, h.AuthService.PasswordRegistrationEnabled())})
}

func (h *AuthHandlers) SignUp(c *gin.Context) {
	var (
		ctx = c.Request.Context()
		req SignUpRequest
	)

	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	user, err := h.AuthService.SignUp(ctx, biz.SignUpInput{
		Email:      req.Email,
		Password:   req.Password,
		FirstName:  req.FirstName,
		LastName:   req.LastName,
		InviteCode: req.InviteCode,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, biz.ErrRegistrationDisabled) {
			status = http.StatusForbidden
		} else if errors.Is(err, biz.ErrPasswordRegistrationDisabled) {
			status = http.StatusForbidden
		}
		JSONError(c, status, err)
		return
	}

	token, err := h.AuthService.GenerateJWTToken(ctx, user)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Internal server error"))
		return
	}

	c.JSON(http.StatusOK, SignInResponse{
		User:  biz.ConvertUserToUserInfo(ctx, user),
		Token: token,
	})
}

func adminRegistrationPolicyResponse(policy biz.RegistrationConfig, passwordSignupAllowed bool) AdminRegistrationPolicyResponse {
	return AdminRegistrationPolicyResponse{
		Enabled:                policy.Enabled,
		OIDCEnabled:            policy.OIDCEnabled,
		InviteCode:             policy.InviteCode,
		InviteCodeRequired:     policy.InviteCode != "",
		DefaultProjectID:       policy.DefaultProjectID,
		AutoJoinFirstProject:   policy.AutoJoinFirstProject,
		DefaultProjectScopes:   policy.DefaultProjectScopes,
		AllowRequestDetails:    policy.AllowRequestDetails,
		SelfServicePresetNames: policy.SelfServicePresetNames,
		PasswordSignupAllowed:  passwordSignupAllowed,
	}
}
