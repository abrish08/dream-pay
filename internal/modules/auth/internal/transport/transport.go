package transport

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"paygate/internal/contracts/auth"
	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/auth/internal/service"
	"paygate/internal/platform/authn"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/httpx"
)

type HTTP struct {
	svc  *service.Service
	jwks *authn.Verifier
	log  *slog.Logger
}

func NewHTTP(svc *service.Service, jwks *authn.Verifier, log *slog.Logger) *HTTP {
	return &HTTP{svc: svc, jwks: jwks, log: log}
}

// RegisterOpen: no credentials needed.
//
//	POST /auth/signup           {business_name, name, email, password, phone?, website?, verification_channel?}
//	                            -> pending merchant + owner; a 6-digit code is sent (no token yet)
//	POST /auth/verify           {email, code}      -> account verified, logged in
//	POST /auth/verify/resend    {email, channel?}  -> always 202
//	POST /auth/login/mfa        {mfa_token, code}  -> second step when an authenticator is on
//	POST /auth/oauth/{google|apple} {id_token, nonce?, business_name?} -> sign in / link / sign up
//	POST /auth/password/forgot  {email}            -> always 202 (emails a reset link if the account exists)
//	POST /auth/password/reset   {token, new_password}
//	POST /auth/login            {email, password}  -> access + refresh token
//	POST /auth/refresh          {refresh_token}    -> new pair (old refresh token stops working)
//	POST /auth/logout           {refresh_token}
//	GET  /.well-known/jwks.json  public keys to verify access tokens
func (h *HTTP) RegisterOpen(r chi.Router) {
	r.Post("/auth/signup", h.signup)
	r.Post("/auth/verify", h.verify)
	r.Post("/auth/verify/resend", h.resendCode)
	r.Post("/auth/login/mfa", h.loginMFA)
	r.Post("/auth/oauth/{provider}", h.external)
	r.Post("/auth/password/forgot", h.forgot)
	r.Post("/auth/password/reset", h.reset)
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/logout", h.logout)
	r.Get("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		httpx.WriteJSON(w, http.StatusOK, h.jwks.JWKS())
	})
}

// RegisterAuthenticated: any valid credentials.
func (h *HTTP) RegisterAuthenticated(r chi.Router) {
	r.Get("/auth/me", h.me)
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireUser(h.log))
		r.Post("/auth/password", h.changePassword) // needs an authenticator code
		r.Get("/auth/mfa", h.mfaStatus)
		r.Post("/auth/mfa/totp/setup", h.totpSetup)
		r.Post("/auth/mfa/totp/confirm", h.totpConfirm)
		r.Post("/auth/mfa/totp/disable", h.totpDisable)
	})
}

// RegisterPublic: /v1, merchant callers. Keys, team members and roles can
// only be managed by a logged-in person, never with an API key.
//
//	POST   /v1/api-keys                          create (secret shown once)
//	GET    /v1/api-keys                          list
//	DELETE /v1/api-keys/{id}                     revoke
//	POST   /v1/api-keys/{id}/roll                replace, old key works for a grace period
//
//	POST   /v1/team/members                      {email, name, password, roles:[id|name]}
//	GET    /v1/team/members
//	DELETE /v1/team/members/{id}                 disable
//	POST   /v1/team/members/{id}/roles           {role: id|name}   assign a role
//	DELETE /v1/team/members/{id}/roles/{roleID}  remove a role
//
//	GET    /v1/permissions                       what merchant roles can contain
//	GET    /v1/roles                             built-in + this merchant's custom roles
//	POST   /v1/roles                             {name, description, permissions:[...]}
//	GET    /v1/roles/{id}
//	PATCH  /v1/roles/{id}                        {name?, description?}
//	DELETE /v1/roles/{id}                        custom roles not assigned to anyone
//	POST   /v1/roles/{id}/permissions            {permissions:[...]}  add
//	DELETE /v1/roles/{id}/permissions/{code}     remove
//	GET    /v1/audit-log                         role and team changes
func (h *HTTP) RegisterPublic(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireUser(h.log), httpx.Require(auth.APIKeysManage, h.log))
		r.Post("/api-keys", h.createKey)
		r.Get("/api-keys", h.listOwnKeys)
		r.Delete("/api-keys/{id}", h.revokeOwnKey)
		r.Post("/api-keys/{id}/roll", h.rollKey)
	})
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireUser(h.log), httpx.Require(auth.TeamManage, h.log))
		r.Post("/team/members", h.createUser)
		r.Get("/team/members", h.listTeam)
		r.Delete("/team/members/{id}", h.disableUser)
		r.Post("/team/members/{id}/roles", h.assignRole)
		r.Delete("/team/members/{id}/roles/{roleID}", h.unassignRole)
	})
	h.registerRoles(r)
}

// RegisterAdmin: /admin, platform staff. Same role API; ?audience=merchant
// (or "audience" in the body) works on the merchant templates instead of
// platform roles.
func (h *HTTP) RegisterAdmin(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.Require(auth.UsersManage, h.log))
		r.Post("/users", h.createUser)
		r.Get("/users", h.listUsers)
		r.Delete("/users/{id}", h.disableUser)
		r.Post("/users/{id}/roles", h.assignRole)
		r.Delete("/users/{id}/roles/{roleID}", h.unassignRole)
	})
	h.registerRoles(r)
	// Support can see and revoke a merchant's keys (e.g. a reported leak) but
	// never create them: only the merchant's own users can.
	r.With(httpx.Require(merchants.PermRead, h.log)).Get("/merchants/{merchantID}/api-keys", h.listMerchantKeys)
	r.With(httpx.Require(merchants.PermManage, h.log)).Delete("/merchants/{merchantID}/api-keys/{id}", h.revokeMerchantKey)
}

func (h *HTTP) registerRoles(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireUser(h.log), httpx.Require(auth.RolesRead, h.log))
		r.Get("/permissions", h.listPermissions)
		r.Get("/roles", h.listRoles)
		r.Get("/roles/{id}", h.getRole)
		r.Get("/audit-log", h.auditLog)
	})
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireUser(h.log), httpx.Require(auth.RolesManage, h.log))
		r.Post("/roles", h.createRole)
		r.Patch("/roles/{id}", h.updateRole)
		r.Delete("/roles/{id}", h.deleteRole)
		r.Post("/roles/{id}/permissions", h.addRolePermissions)
		r.Delete("/roles/{id}/permissions/{code}", h.removeRolePermission)
	})
}

// RegisterBus answers auth requests on the central bus.
func RegisterBus(b bus.Bus, svc *service.Service) {
	bus.Handle(b, func(ctx context.Context, q auth.ExchangeAPIKey) (auth.AccessToken, error) {
		return svc.ExchangeAPIKey(ctx, q.Key)
	})
	bus.Handle(b, svc.RegisterPermissions)
}

// ---- handlers ----

func (h *HTTP) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	t, err := h.svc.Login(r.Context(), body.Email, body.Password)
	h.respond(w, r, http.StatusOK, t, err)
}

func (h *HTTP) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	t, err := h.svc.VerifyAccount(r.Context(), body.Email, body.Code)
	h.respond(w, r, http.StatusOK, t, err)
}

func (h *HTTP) resendCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email   string `json:"email"`
		Channel string `json:"channel"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if err := h.svc.ResendVerification(r.Context(), body.Email, body.Channel); err != nil {
		h.log.Error("resend verification", "err", err)
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{
		"message": "If this account is waiting for verification, a new code is on its way."})
}

func (h *HTTP) loginMFA(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MFAToken string `json:"mfa_token"`
		Code     string `json:"code"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	t, err := h.svc.LoginMFA(r.Context(), body.MFAToken, body.Code)
	h.respond(w, r, http.StatusOK, t, err)
}

func (h *HTTP) mfaStatus(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.MFAStatus(r.Context(), httpx.Principal(r))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) totpSetup(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.SetupTOTP(r.Context(), httpx.Principal(r))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) totpConfirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	v, err := h.svc.ConfirmTOTP(r.Context(), httpx.Principal(r), body.Code)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) totpDisable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	h.noContent(w, r, h.svc.DisableTOTP(r.Context(), httpx.Principal(r), body.Password, body.Code))
}

func (h *HTTP) signup(w http.ResponseWriter, r *http.Request) {
	var body service.SignupInput
	if !h.decode(w, r, &body) {
		return
	}
	t, err := h.svc.Signup(r.Context(), body)
	h.respond(w, r, http.StatusCreated, t, err)
}

func (h *HTTP) external(w http.ResponseWriter, r *http.Request) {
	var body service.ExternalInput
	if !h.decode(w, r, &body) {
		return
	}
	t, created, err := h.svc.ExternalSignIn(r.Context(), chi.URLParam(r, "provider"), body)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.respond(w, r, status, t, err)
}

func (h *HTTP) forgot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if err := h.svc.ForgotPassword(r.Context(), body.Email, httpx.ClientIP(r, false)); err != nil {
		h.log.Error("forgot password", "err", err) // never tell the client: the answer is always the same
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{
		"message": "If an account uses this email, a reset link is on its way. It expires soon and works once."})
}

func (h *HTTP) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	h.noContent(w, r, h.svc.ResetPassword(r.Context(), body.Token, body.NewPassword))
}

func (h *HTTP) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	t, err := h.svc.Refresh(r.Context(), body.RefreshToken)
	h.respond(w, r, http.StatusOK, t, err)
}

func (h *HTTP) logout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	h.noContent(w, r, h.svc.Logout(r.Context(), body.RefreshToken))
}

func (h *HTTP) me(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Me(r.Context(), httpx.Principal(r))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
		TOTPCode        string `json:"totp_code"` // from Microsoft Authenticator (or any TOTP app)
	}
	if !h.decode(w, r, &body) {
		return
	}
	h.noContent(w, r, h.svc.ChangePassword(r.Context(), httpx.Principal(r), body.CurrentPassword, body.NewPassword, body.TOTPCode))
}

func (h *HTTP) createUser(w http.ResponseWriter, r *http.Request) {
	var body service.CreateUserInput
	if !h.decode(w, r, &body) {
		return
	}
	u, err := h.svc.CreateUser(r.Context(), httpx.Principal(r), body)
	h.respond(w, r, http.StatusCreated, u, err)
}

func (h *HTTP) assignRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"` // id or name
	}
	if !h.decode(w, r, &body) {
		return
	}
	u, err := h.svc.AssignRole(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), body.Role)
	h.respond(w, r, http.StatusOK, u, err)
}

func (h *HTTP) unassignRole(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.UnassignRole(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), chi.URLParam(r, "roleID"))
	h.respond(w, r, http.StatusOK, u, err)
}

// ---- roles ----

func (h *HTTP) listPermissions(w http.ResponseWriter, r *http.Request) {
	ps, err := h.svc.ListPermissions(r.Context(), httpx.Principal(r), r.URL.Query().Get("audience"))
	h.respond(w, r, http.StatusOK, map[string]any{"data": ps}, err)
}

func (h *HTTP) listRoles(w http.ResponseWriter, r *http.Request) {
	rs, err := h.svc.ListRoles(r.Context(), httpx.Principal(r), r.URL.Query().Get("audience"))
	h.respond(w, r, http.StatusOK, map[string]any{"data": rs}, err)
}

func (h *HTTP) getRole(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.GetRole(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) createRole(w http.ResponseWriter, r *http.Request) {
	var body service.CreateRoleInput
	if !h.decode(w, r, &body) {
		return
	}
	v, err := h.svc.CreateRole(r.Context(), httpx.Principal(r), body)
	h.respond(w, r, http.StatusCreated, v, err)
}

func (h *HTTP) updateRole(w http.ResponseWriter, r *http.Request) {
	var body service.UpdateRoleInput
	if !h.decode(w, r, &body) {
		return
	}
	v, err := h.svc.UpdateRole(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), body)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) deleteRole(w http.ResponseWriter, r *http.Request) {
	h.noContent(w, r, h.svc.DeleteRole(r.Context(), httpx.Principal(r), chi.URLParam(r, "id")))
}

func (h *HTTP) addRolePermissions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Permissions []string `json:"permissions"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	v, err := h.svc.AddRolePermissions(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), body.Permissions)
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) removeRolePermission(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.RemoveRolePermission(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), chi.URLParam(r, "code"))
	h.respond(w, r, http.StatusOK, v, err)
}

func (h *HTTP) auditLog(w http.ResponseWriter, r *http.Request) {
	es, err := h.svc.AuditLog(r.Context(), httpx.Principal(r))
	h.respond(w, r, http.StatusOK, map[string]any{"data": es}, err)
}

func (h *HTTP) listUsers(w http.ResponseWriter, r *http.Request) {
	us, err := h.svc.ListUsers(r.Context(), r.URL.Query().Get("merchant_id"))
	h.respond(w, r, http.StatusOK, map[string]any{"data": us}, err)
}

func (h *HTTP) listTeam(w http.ResponseWriter, r *http.Request) {
	us, err := h.svc.ListUsers(r.Context(), httpx.Principal(r).MerchantID)
	h.respond(w, r, http.StatusOK, map[string]any{"data": us}, err)
}

func (h *HTTP) disableUser(w http.ResponseWriter, r *http.Request) {
	h.noContent(w, r, h.svc.DisableUser(r.Context(), httpx.Principal(r), chi.URLParam(r, "id")))
}

func (h *HTTP) createKey(w http.ResponseWriter, r *http.Request) {
	var body service.CreateKeyInput
	if !h.decode(w, r, &body) {
		return
	}
	if body.Environment == "" {
		body.Environment = "test"
	}
	k, err := h.svc.CreateKey(r.Context(), httpx.Principal(r), body)
	h.respond(w, r, http.StatusCreated, k, err)
}

func (h *HTTP) listOwnKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := h.svc.ListKeys(r.Context(), httpx.Principal(r).MerchantID)
	h.respond(w, r, http.StatusOK, map[string]any{"data": ks}, err)
}

func (h *HTTP) revokeOwnKey(w http.ResponseWriter, r *http.Request) {
	p := httpx.Principal(r)
	h.noContent(w, r, h.svc.RevokeKey(r.Context(), p, p.MerchantID, chi.URLParam(r, "id")))
}

func (h *HTTP) rollKey(w http.ResponseWriter, r *http.Request) {
	body := struct {
		GraceHours    *int `json:"grace_hours"`
		ExpiresInDays int  `json:"expires_in_days"`
	}{}
	if r.ContentLength != 0 && !h.decode(w, r, &body) {
		return
	}
	grace := 24
	if body.GraceHours != nil {
		grace = *body.GraceHours
	}
	k, err := h.svc.RollKey(r.Context(), httpx.Principal(r), chi.URLParam(r, "id"), grace, body.ExpiresInDays)
	h.respond(w, r, http.StatusCreated, k, err)
}

func (h *HTTP) listMerchantKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := h.svc.ListKeys(r.Context(), chi.URLParam(r, "merchantID"))
	h.respond(w, r, http.StatusOK, map[string]any{"data": ks}, err)
}

func (h *HTTP) revokeMerchantKey(w http.ResponseWriter, r *http.Request) {
	h.noContent(w, r, h.svc.RevokeKey(r.Context(), httpx.Principal(r), chi.URLParam(r, "merchantID"), chi.URLParam(r, "id")))
}

// ---- helpers ----

func (h *HTTP) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := httpx.DecodeJSON(r, dst); err != nil {
		httpx.WriteError(w, r, h.log, err)
		return false
	}
	return true
}

func (h *HTTP) respond(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // responses may contain secrets or tokens
	httpx.WriteJSON(w, status, v)
}

func (h *HTTP) noContent(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		httpx.WriteError(w, r, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
