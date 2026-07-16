package syssettings

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type patchSettingsRequest struct {
	RegistrationEnabled bool `json:"registration_enabled"`
}

// PublicConfigHandler serves GET /api/v1/public/config without auth.
//
//	@Summary		Public runtime config
//	@Description	Returns public feature flags such as registration_enabled.
//	@Tags			public
//	@Produce		json
//	@Success		200	{object}	Settings
//	@Router			/api/v1/public/config [get]
func PublicConfigHandler(gate RegistrationGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, err := gate.GetSettings(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, settings)
	}
}

// AdminGetSettingsHandler serves GET /api/v1/admin/settings.
//
//	@Summary		Get system settings (admin)
//	@Tags			admin
//	@Produce		json
//	@Success		200	{object}	Settings
//	@Failure		401	{object}	map[string]string
//	@Failure		403	{object}	map[string]string
//	@Security		BearerAuth
//	@Router			/api/v1/admin/settings [get]
func AdminGetSettingsHandler(gate RegistrationGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, err := gate.GetSettings(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, settings)
	}
}

// AdminPatchSettingsHandler serves PATCH /api/v1/admin/settings.
//
//	@Summary		Update system settings (admin)
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Param			body	body		patchSettingsRequest	true	"Settings payload"
//	@Success		200		{object}	Settings
//	@Failure		400		{object}	map[string]string
//	@Failure		401		{object}	map[string]string
//	@Failure		403		{object}	map[string]string
//	@Security		BearerAuth
//	@Router			/api/v1/admin/settings [patch]
func AdminPatchSettingsHandler(gate RegistrationGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req patchSettingsRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "registration_enabled is required"})
			return
		}
		settings, err := gate.SetRegistrationEnabled(c.Request.Context(), req.RegistrationEnabled)
		if err != nil {
			if err.Error() == "Firestore client is not configured" {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, settings)
	}
}