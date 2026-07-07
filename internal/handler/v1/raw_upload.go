package v1

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxRawUploadSize          = 5 << 20
	maxRawUploadRequestSize   = maxRawUploadSize + (1 << 20)
	rawUploadRelativeDir      = "raw"
	rawUploadReadyStatus      = "ready"
	rawUploadMarkdownSuffix   = ".md"
	rawUploadMaxFilenameBytes = 512
)

var (
	rawUploadFilenamePattern = regexp.MustCompile(`^[^/\x00]+\.(md|txt|html?|csv|json|xml|ya?ml|toml|ini|cfg|log|rst|org|tex)$`)
	errRawUploadTooLarge     = errors.New("raw upload file too large")
	errRawUploadEmptyFile    = errors.New("raw upload file is empty")
)

type rawUploadResponse struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

// RawUpload handles POST /api/v1/raw/upload.
func (h *Handler) RawUpload(c *gin.Context) {
	userID := strings.TrimSpace(c.GetString("userID"))
	projectID := strings.TrimSpace(c.GetString("projectID"))
	if userID == "" {
		c.JSON(http.StatusUnauthorized, handler.ErrorResponse{Error: "user not authenticated"})
		return
	}
	if projectID == "" {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "project ID is required"})
		return
	}
	if h.firestore == nil || h.firestore.Raw() == nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "Firestore client is not configured"})
		return
	}

	ready, err := h.rawUploadProjectReady(c, userID, projectID)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}
	if !ready {
		c.JSON(http.StatusServiceUnavailable, handler.ErrorResponse{Error: "project not ready"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRawUploadRequestSize)
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) || strings.Contains(strings.ToLower(err.Error()), "request body too large") {
			c.JSON(http.StatusRequestEntityTooLarge, handler.ErrorResponse{Error: "file too large (max 5 MiB)"})
			return
		}
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "file field is required"})
		return
	}
	defer file.Close()
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}

	filename := ""
	if header != nil {
		filename = header.Filename
	}
	if err := validateRawUploadFilename(filename); err != nil {
		c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: err.Error()})
		return
	}

	data, size, digest, err := readRawUploadBody(file)
	if err != nil {
		switch {
		case errors.Is(err, errRawUploadTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, handler.ErrorResponse{Error: "file too large (max 5 MiB)"})
		case errors.Is(err, errRawUploadEmptyFile):
			c.JSON(http.StatusBadRequest, handler.ErrorResponse{Error: "empty file"})
		default:
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "read file: " + err.Error()})
		}
		return
	}

	gcsClient, err := h.GetGCSClient(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}
	relPath := rawUploadRelativePath(filename)
	if _, err := gcsClient.WriteBytes(c.Request.Context(), data, relPath); err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "upload to GCS: " + err.Error()})
		return
	}

	c.JSON(http.StatusCreated, newRawUploadResponse(userID, projectID, filename, size, digest))
}

func (h *Handler) rawUploadProjectReady(c *gin.Context, userID, projectID string) (bool, error) {
	snap, err := h.firestore.Raw().Collection("projects").Doc(projectDocID(userID, projectID)).Get(c.Request.Context())
	if err != nil {
		return false, err
	}
	statusValue, _ := snap.Data()["status"].(string)
	return statusValue == rawUploadReadyStatus, nil
}

func validateRawUploadFilename(filename string) error {
	switch {
	case filename == "":
		return fmt.Errorf("filename is empty")
	case len(filename) > rawUploadMaxFilenameBytes:
		return fmt.Errorf("filename too long (max %d bytes)", rawUploadMaxFilenameBytes)
	case !rawUploadFilenamePattern.MatchString(filename):
		return fmt.Errorf("unsupported file type (accepted: .md, .txt, .html, .csv, .json, .xml, .yaml, .toml)")
	case filename == rawUploadMarkdownSuffix:
		return fmt.Errorf("filename is only '.md'")
	default:
		return nil
	}
}

func readRawUploadBody(r io.Reader) ([]byte, int64, string, error) {
	var buf bytes.Buffer
	hasher := sha256.New()
	size, err := copyRawUploadBody(&buf, hasher, r)
	if err != nil {
		return nil, 0, "", err
	}
	if size == 0 {
		return nil, 0, "", errRawUploadEmptyFile
	}
	return buf.Bytes(), size, fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func copyRawUploadBody(dst *bytes.Buffer, hasher hash.Hash, src io.Reader) (int64, error) {
	var size int64
	chunk := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(chunk)
		if n > 0 {
			size += int64(n)
			if size > maxRawUploadSize {
				return 0, errRawUploadTooLarge
			}
			part := chunk[:n]
			if _, err := dst.Write(part); err != nil {
				return 0, err
			}
			if _, err := hasher.Write(part); err != nil {
				return 0, err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return size, nil
			}
			return 0, readErr
		}
	}
}

func rawUploadRelativePath(filename string) string {
	return rawUploadRelativeDir + "/" + filename
}

func newRawUploadResponse(userID, projectID, filename string, bytes int64, digest string) rawUploadResponse {
	return rawUploadResponse{
		Filename: filename,
		Path:     fmt.Sprintf("users/%s/projects/%s/%s", userID, projectID, rawUploadRelativePath(filename)),
		Bytes:    bytes,
		SHA256:   digest,
	}
}
