package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func (h *AuthHandlers) RegistrationStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	enabled, err := h.AuthService.InvitationRegistrationEnabled(c.Request.Context())
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Unable to load registration status"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": enabled})
}

func (h *AuthHandlers) SignUp(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var input objects.RegistrationInput
	if err := decoder.Decode(&input); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid registration fields"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}
	err := h.AuthService.RegisterWithInvite(c.Request.Context(), input)
	switch {
	case err == nil:
		c.JSON(http.StatusCreated, gin.H{"success": true})
	case errors.Is(err, biz.ErrRegistrationClosed):
		JSONError(c, http.StatusForbidden, err)
	case errors.Is(err, biz.ErrRegistrationInput), errors.Is(err, biz.ErrRegistrationInvite), errors.Is(err, biz.ErrRegistrationEmail):
		JSONError(c, http.StatusBadRequest, err)
	default:
		// Do not include DB/constraint text, request data, passwords or codes.
		JSONError(c, http.StatusInternalServerError, errors.New("Registration failed. Try again."))
	}
}
