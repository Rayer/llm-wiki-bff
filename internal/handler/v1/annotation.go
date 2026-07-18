package v1

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

const maxAnnotationBytes = 16 << 10
const maxAnnotationRequestBytes = 256 << 10

func (h *Handler) annotationTarget(c *gin.Context) (store.Store, store.ConditionalWriter, string, string, bool) {
	s, err := h.GetStore(c)
	if err != nil {
		c.JSON(500, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return nil, nil, "", "", false
	}
	w, ok := s.(store.ConditionalWriter)
	if !ok {
		c.JSON(500, handler.ErrorResponse{Error: "annotation storage is not configured"})
		return nil, nil, "", "", false
	}
	dual, err := h.getIDRoutingMap(c.Request.Context(), s)
	if err != nil {
		c.JSON(500, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return nil, nil, "", "", false
	}
	id := strings.TrimSpace(c.Param("id"))
	if !annotation.ValidSourceID(id) {
		c.JSON(404, handler.ErrorResponse{Error: "source not found"})
		return nil, nil, "", "", false
	}
	entry, ok := dual.byID[id]
	if !ok || entry.Type != "source" {
		c.JSON(404, handler.ErrorResponse{Error: "source not found"})
		return nil, nil, "", "", false
	}
	_, data, err := s.GetPage(c.Request.Context(), entry.Slug, "sources")
	if err != nil {
		c.JSON(409, handler.ErrorResponse{Error: "source mapping is unavailable"})
		return nil, nil, "", "", false
	}
	fm, _ := parseFrontmatter(string(data))
	raw, _ := fm["source_file"].(string)
	raw = strings.TrimSpace(raw)
	if !store.SafeRawPath(raw) {
		c.JSON(409, handler.ErrorResponse{Error: "source raw mapping is unsafe"})
		return nil, nil, "", "", false
	}
	if _, err := s.ReadFile(c.Request.Context(), raw); err != nil {
		c.JSON(409, handler.ErrorResponse{Error: "source raw mapping is unavailable"})
		return nil, nil, "", "", false
	}
	return s, w, id, raw, true
}

// GetAnnotation handles GET /api/v1/sources/:id/annotation.
//
// @Summary Get a source annotation
// @Tags sources
// @Produce json
// @Param id path string true "Source ID"
// @Success 200 {object} handler.AnnotationResponse
// @Failure 404,409,500 {object} handler.ErrorResponse
// @Security DevUserAuth
// @Security ProjectHeader
// @Router /api/v1/sources/{id}/annotation [get]
func (h *Handler) GetAnnotation(c *gin.Context) {
	s, w, id, raw, ok := h.annotationTarget(c)
	if !ok {
		return
	}
	data, gen, err := w.ReadFileWithGeneration(c.Request.Context(), annotation.Path(id))
	if errors.Is(err, storage.ErrObjectNotExist) {
		h.writeAnnotationResponse(c, s, id, raw, annotation.Object{SHA256: annotation.Digest("")}, 0)
		return
	}
	if err != nil {
		c.JSON(500, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	var object annotation.Object
	if !utf8.Valid(data) || json.Unmarshal(data, &object) != nil || object.Validate(id, raw) != nil {
		c.JSON(500, handler.ErrorResponse{Error: "invalid annotation"})
		return
	}
	h.writeAnnotationResponse(c, s, id, raw, object, gen)
}

// PutAnnotation handles PUT /api/v1/sources/:id/annotation.
//
// @Summary Save or clear a source annotation
// @Tags sources
// @Accept json
// @Produce json
// @Param id path string true "Source ID"
// @Param annotation body handler.AnnotationRequest true "Annotation"
// @Success 200 {object} handler.AnnotationResponse
// @Failure 400,404,409,412,413,500 {object} handler.ErrorResponse
// @Security DevUserAuth
// @Security ProjectHeader
// @Router /api/v1/sources/{id}/annotation [put]
func (h *Handler) PutAnnotation(c *gin.Context) {
	s, w, id, raw, ok := h.annotationTarget(c)
	if !ok {
		return
	}
	var req handler.AnnotationRequest
	if err := decodeStrictAnnotationRequest(c, &req); err != nil {
		c.JSON(400, handler.ErrorResponse{Error: "invalid annotation request"})
		return
	}
	expected, err := strconv.ParseInt(req.ExpectedGeneration, 10, 64)
	if err != nil || expected < 0 {
		c.JSON(400, handler.ErrorResponse{Error: "expected_generation is required"})
		return
	}
	body := annotation.Normalize(req.Body)
	if len([]byte(body)) > maxAnnotationBytes {
		c.JSON(413, handler.ErrorResponse{Error: "annotation body exceeds 16 KiB"})
		return
	}
	if current, gen, err := w.ReadFileWithGeneration(c.Request.Context(), annotation.Path(id)); err == nil {
		if gen != expected {
			c.JSON(412, handler.ErrorResponse{Error: "annotation generation mismatch"})
			return
		}
		var prior annotation.Object
		if utf8.Valid(current) && json.Unmarshal(current, &prior) == nil && prior.Validate(id, raw) == nil && prior.Body == body {
			h.writeAnnotationResponse(c, s, id, raw, prior, gen)
			return
		}
	} else if !errors.Is(err, storage.ErrObjectNotExist) {
		c.JSON(500, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	object := annotation.Object{Version: 1, SourceID: id, RawPath: raw, Body: body, SHA256: annotation.Digest(body), UpdatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedBy: c.GetString("userID")}
	data, _ := json.Marshal(object)
	gen, err := w.WriteFileIfGeneration(c.Request.Context(), data, annotation.Path(id), expected)
	if err != nil {
		if errors.Is(err, store.ErrGenerationMismatch) {
			c.JSON(412, handler.ErrorResponse{Error: "annotation generation mismatch"})
		} else {
			c.JSON(500, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		}
		return
	}
	h.writeAnnotationResponse(c, s, id, raw, object, gen)
}

func decodeStrictAnnotationRequest(c *gin.Context, req *handler.AnnotationRequest) error {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxAnnotationRequestBytes+1))
	if err != nil || len(raw) > maxAnnotationRequestBytes || !utf8.Valid(raw) || !validJSONSurrogates(raw) {
		return errors.New("invalid annotation request")
	}
	return json.Unmarshal(raw, req)
}

// validJSONSurrogates rejects escapes Go's JSON decoder otherwise replaces
// with U+FFFD. UTF-8 itself is checked separately before unmarshalling.
func validJSONSurrogates(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if !inString {
			if raw[i] == '"' {
				inString = true
			}
			continue
		}
		if raw[i] == '"' {
			inString = false
			continue
		}
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		value, ok := hex16(raw[i+1 : i+5])
		if !ok {
			return false
		}
		i += 4
		if value >= 0xD800 && value <= 0xDBFF {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, ok := hex16(raw[i+3 : i+7])
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		} else if value >= 0xDC00 && value <= 0xDFFF {
			return false
		}
	}
	return !inString
}

func hex16(raw []byte) (rune, bool) {
	var value rune
	for _, b := range raw {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value += rune(b - '0')
		case b >= 'a' && b <= 'f':
			value += rune(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value += rune(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func (h *Handler) writeAnnotationResponse(c *gin.Context, s store.Store, id, raw string, object annotation.Object, generation int64) {
	response, err := h.annotationResponse(c, s, id, raw, object, generation)
	if err != nil {
		c.JSON(500, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
		return
	}
	c.JSON(200, response)
}

func (h *Handler) annotationResponse(c *gin.Context, s store.Store, id, raw string, object annotation.Object, generation int64) (handler.AnnotationResponse, error) {
	updatedAt, _ := time.Parse(time.RFC3339, object.UpdatedAt)
	pages, _, err := sourceLifecycleWithAnnotations(c.Request.Context(), s, []store.WikiPage{{ID: id, RawPath: raw}}, map[string]store.ObjectMeta{id: {SHA256: object.SHA256, HasAnnotation: object.Body != "", Updated: updatedAt}})
	if err != nil {
		return handler.AnnotationResponse{}, err
	}
	page := store.WikiPage{LifecycleStatus: "new"}
	if len(pages) == 1 {
		page = pages[0]
	}
	return handler.AnnotationResponse{SourceID: id, RawPath: raw, Body: object.Body, SHA256: object.SHA256, UpdatedAt: object.UpdatedAt, UpdatedBy: object.UpdatedBy, HasAnnotation: object.Body != "", Generation: strconv.FormatInt(generation, 10), AnnotationDirty: page.AnnotationDirty, RawDirty: page.RawDirty, Dirty: page.Dirty, LifecycleStatus: page.LifecycleStatus}, nil
}
