package buildinfo

import (
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
)

var (
	ProductVersion = "dev"
	GitSHA         = "unknown"
	GitBranch      = "unknown"
	GitTag         = ""
	ImageTag       = "unknown"
)

// Info is the explicitly allowlisted deployed build and Cloud Run identity.
type Info struct {
	ProductVersion string `json:"product_version"`
	Commit         string `json:"commit"`
	Branch         string `json:"branch"`
	Tag            string `json:"tag"`
	ImageTag       string `json:"image_tag"`
	Service        string `json:"service"`
	Revision       string `json:"revision"`
}

// Current returns build metadata injected by the Docker build and Cloud Run's
// service identity. Local builds retain the package's explicit fallbacks.
func Current() Info {
	return Info{
		ProductVersion: ProductVersion,
		Commit:         GitSHA,
		Branch:         GitBranch,
		Tag:            GitTag,
		ImageTag:       ImageTag,
		Service:        cloudRunValue(os.Getenv("K_SERVICE")),
		Revision:       cloudRunValue(os.Getenv("K_REVISION")),
	}
}

func cloudRunValue(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

// Handler serves GET /api/v1/public/version without authentication.
//
//	@Summary		Build and deployment identity
//	@Description	Returns allowlisted product build metadata and Cloud Run identity. This response never includes image digests, environment variables, credentials, project IDs, or user IDs.
//	@Tags			public
//	@Produce		json
//	@Success		200	{object}	Info
//	@Header			200	{string}	Cache-Control	"no-store"
//	@Router			/api/v1/public/version [get]
func Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, Current())
	}
}
