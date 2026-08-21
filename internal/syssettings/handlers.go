package syssettings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

type patchSettingsRequest struct {
	RegistrationEnabled *bool `json:"registration_enabled"`
}

type publishAnnouncementRequest struct {
	AnnouncementMarkdown *string `json:"announcement_markdown" binding:"required"`
}

func bindStrictJSONBody(c *gin.Context, req any) error {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || !utf8.Valid(body) {
		if err != nil {
			return err
		}
		return fmt.Errorf("request is not valid UTF-8")
	}
	if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return fmt.Errorf("request must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(req); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

// PublicConfigHandler serves GET /api/v1/public/config without auth.
//
//	@Summary		Public runtime config
//	@Description	Returns the registration flag and currently published announcement Markdown.
//	@Tags			public
//	@Produce		json
//	@Success		200	{object}	PublicSettings
//	@Router			/api/v1/public/config [get]
func PublicConfigHandler(gate RegistrationGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, err := gate.GetSettings(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, PublicSettings{
			RegistrationEnabled:  settings.RegistrationEnabled,
			AnnouncementMarkdown: settings.AnnouncementMarkdown,
			AnnouncementDigest:   announcementDigest(settings.AnnouncementMarkdown),
		})
	}
}

func announcementDigest(markdown string) *string {
	if markdown == "" {
		return nil
	}
	digest := sha256.Sum256([]byte(markdown))
	announcementDigest := "sha256:" + hex.EncodeToString(digest[:])
	return &announcementDigest
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
		if err := bindStrictJSONBody(c, &req); err != nil || req.RegistrationEnabled == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "valid settings are required"})
			return
		}
		var settings Settings
		var err error
		if req.RegistrationEnabled != nil {
			settings, err = gate.SetRegistrationEnabled(c.Request.Context(), *req.RegistrationEnabled)
		}
		if err != nil {
			if strings.Contains(err.Error(), "exceeds") || strings.Contains(err.Error(), "valid UTF-8") {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, settings)
	}
}

// AdminPublishAnnouncementHandler publishes the supplied announcement Markdown.
//
//	@Summary		Publish announcement (admin)
//	@Accept			json
//	@Tags			admin
//	@Produce		json
//	@Param			body	body	publishAnnouncementRequest	true	"Announcement Markdown"
//	@Success		200	{object}	Settings
//	@Failure		400	{object}	map[string]string
//	@Failure		401	{object}	map[string]string
//	@Failure		403	{object}	map[string]string
//	@Security		BearerAuth
//	@Router			/api/v1/admin/settings/announcement/publish [post]
func AdminPublishAnnouncementHandler(gate RegistrationGate) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req publishAnnouncementRequest
		if err := bindStrictJSONBody(c, &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "announcement_markdown is required"})
			return
		}
		if req.AnnouncementMarkdown == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "announcement_markdown is required"})
			return
		}
		settings, err := gate.PublishAnnouncement(c.Request.Context(), *req.AnnouncementMarkdown)
		if err != nil {
			if strings.Contains(err.Error(), "exceeds") || strings.Contains(err.Error(), "valid UTF-8") {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		c.JSON(http.StatusOK, settings)
	}
}
