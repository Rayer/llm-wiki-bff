package v1

import (
	"errors"
	"net/http"

	cloudstorage "cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	internalstorage "github.com/rayer/llm-wiki-bff/internal/storage"
)

const (
	generatedDataUnavailableMessage     = "generated data unavailable"
	pipelineUnavailableMessage          = "pipeline unavailable"
	pipelineStatusUnavailableMessage    = "pipeline status unavailable"
	pipelineExecutionUnavailableMessage = "pipeline execution unavailable"
	projectStatisticsUnavailableMessage = "project statistics unavailable"
)

func writeGeneratedReadError(c *gin.Context, err error, notFoundMessage string) {
	if errors.Is(err, internalstorage.ErrDeclaredObjectUnavailable) {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	if errors.Is(err, internalstorage.ErrObjectNotExist) || errors.Is(err, cloudstorage.ErrObjectNotExist) {
		c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: notFoundMessage})
		return
	}
	c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
}
