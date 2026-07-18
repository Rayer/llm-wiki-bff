package v1

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
)

type rawListResponse struct {
	Files []rawstatus.File `json:"files"`
}

func (h *Handler) RawList(c *gin.Context) {
	wikiStore, err := h.GetStore(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	files, err := wikiStore.ListRawFiles(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}

	artifact := rawstatus.EmptyArtifact(time.Now())
	data, err := wikiStore.ReadFile(c.Request.Context(), rawstatus.Path)
	if err != nil {
		if !errors.Is(err, storage.ErrObjectNotExist) {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
			return
		}
	} else {
		artifact, err = rawstatus.Decode(data)
		if err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
			return
		}
	}

	c.JSON(http.StatusOK, rawListResponse{Files: rawstatus.Apply(files, artifact)})
}

func (h *Handler) RawPreview(c *gin.Context) {
	filename := c.Param("filename")
	if err := validateRawUploadFilename(filename); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: err.Error()})
		return
	}

	wikiStore, err := h.GetStore(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "raw file unavailable"})
		return
	}

	data, err := wikiStore.ReadFile(c.Request.Context(), rawUploadRelativePath(filename))
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "raw file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "raw file unavailable"})
		return
	}

	c.Data(http.StatusOK, rawPreviewContentType(filename), data)
}

func rawPreviewContentType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}
