package v1

import (
	"errors"
	"net/http"
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
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}
	files, err := wikiStore.ListRawFiles(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "list raw files: " + err.Error()})
		return
	}

	artifact := rawstatus.EmptyArtifact(time.Now())
	data, err := wikiStore.ReadFile(c.Request.Context(), rawstatus.Path)
	if err != nil {
		if !errors.Is(err, storage.ErrObjectNotExist) {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "read raw status: " + err.Error()})
			return
		}
	} else {
		artifact, err = rawstatus.Decode(data)
		if err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "decode raw status: " + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, rawListResponse{Files: rawstatus.Apply(files, artifact)})
}
