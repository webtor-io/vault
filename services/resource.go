package services

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// UIDsRequest carries the pledgers' uids (sha1 hex) in the order the worker
// tries them.
type UIDsRequest struct {
	UIDs []string `json:"uids"`
}

var uidRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// parseUIDs reads a UIDsRequest. An empty body is not present; {}, null and
// [] are present and empty.
func parseUIDs(body io.Reader) (uids []string, present bool, err error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, false, errors.Wrap(err, "failed to read body")
	}
	if len(b) == 0 {
		return nil, false, nil
	}
	var req UIDsRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, false, errors.Wrap(ErrBadRequest, err.Error())
	}
	for i, u := range req.UIDs {
		if !uidRe.MatchString(u) {
			// The index, not the value: uids stay out of the logs.
			return nil, false, errors.Wrapf(ErrBadRequest, "uids[%d] is not a lowercase sha1 hex", i)
		}
	}
	return req.UIDs, true, nil
}

// PUT /resource/{id} — queue storing of a resource (id = infohash)
// putResource godoc
// @Summary      Queue storing of a resource
// @Description  Creates the resource if missing or marks it queued for processing. A non-empty uids replaces the pledgers' uids; no body or an empty list leaves them
// @Tags         resource
// @Accept       json
// @Param        id    path      string       true   "Resource ID"
// @Param        body  body      UIDsRequest  false  "Pledgers' uids"
// @Success      202  {object}  Resource
// @Failure      400  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Router       /resource/{id} [put]
func (s *Web) putResource(c *gin.Context) {
	id := c.Param("id")
	db := s.pg.Get()
	if db == nil {
		_ = c.Error(errors.New("DB not configured"))
		return
	}
	uids, _, err := parseUIDs(c.Request.Body)
	if err != nil {
		if aerr := APILogWrite(c.Request.Context(), db, id, OperationStore, http.StatusBadRequest); aerr != nil {
			log.WithError(aerr).WithField("resource_id", id).Warn("failed to write api log")
		}
		_ = c.Error(err)
		return
	}
	res, err := ResourceQueueForStoring(c.Request.Context(), db, id, uids)
	if err != nil {
		if aerr := APILogWrite(c.Request.Context(), db, id, OperationStore, http.StatusInternalServerError); aerr != nil {
			log.WithError(aerr).WithField("resource_id", id).Warn("failed to write api log")
		}
		_ = c.Error(err)
		return
	}
	if aerr := APILogWrite(c.Request.Context(), db, id, OperationStore, http.StatusAccepted); aerr != nil {
		log.WithError(aerr).WithField("resource_id", id).Warn("failed to write api log")
	}
	c.JSON(http.StatusAccepted, res)
}

// PUT /resource/{id}/uids — replace the pledgers' uids, status unchanged
// putResourceUIDs godoc
// @Summary      Replace the pledgers' uids of a resource
// @Description  Replaces the uids the worker signs with, keeping their order. The status is not changed and a missing resource is not created. {}, null and [] clear the list
// @Tags         resource
// @Accept       json
// @Param        id    path      string       true  "Resource ID"
// @Param        body  body      UIDsRequest  true  "Pledgers' uids"
// @Success      200  {object}  Resource
// @Failure      400  {object}  ErrorResponse
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Router       /resource/{id}/uids [put]
func (s *Web) putResourceUIDs(c *gin.Context) {
	db := s.pg.Get()
	if db == nil {
		_ = c.Error(errors.New("DB not configured"))
		return
	}
	uids, present, err := parseUIDs(c.Request.Body)
	if err == nil && !present {
		err = errors.Wrap(ErrBadRequest, "body with uids required")
	}
	if err != nil {
		_ = c.Error(err)
		return
	}
	res, err := ResourceSetUIDs(c.Request.Context(), db, c.Param("id"), uids)
	if err != nil {
		_ = c.Error(err)
		return
	}
	if res == nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.JSON(http.StatusOK, res)
}

// GET /resource/{id}
// getResource godoc
// @Summary      Get resource
// @Tags         resource
// @Param        id   path      string  true  "Resource ID"
// @Success      200  {object}  Resource
// @Failure      404  {object}  ErrorResponse
// @Failure      500  {object}  ErrorResponse
// @Router       /resource/{id} [get]
func (s *Web) getResource(c *gin.Context) {
	db := s.pg.Get()
	if db == nil {
		_ = c.Error(errors.New("DB not configured"))
		return
	}
	id := c.Param("id")
	res, err := ResourceGetByID(c.Request.Context(), db, id)
	if err != nil {
		_ = c.Error(err)
		return
	}
	if res == nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.JSON(http.StatusOK, res)
}

// DELETE /resource/{id} — queue deletion
// deleteResource godoc
// @Summary      Queue deletion of a resource
// @Tags         resource
// @Param        id   path      string  true  "Resource ID"
// @Success      202  {object}  Resource
// @Failure      500  {object}  ErrorResponse
// @Router       /resource/{id} [delete]
func (s *Web) deleteResource(c *gin.Context) {
	db := s.pg.Get()
	if db == nil {
		_ = c.Error(errors.New("DB not configured"))
		return
	}
	id := c.Param("id")
	res, err := ResourceQueueForDeletion(c.Request.Context(), db, id)
	if err != nil {
		if aerr := APILogWrite(c.Request.Context(), db, id, OperationDelete, http.StatusInternalServerError); aerr != nil {
			log.WithError(aerr).WithField("resource_id", id).Warn("failed to write api log")
		}
		_ = c.Error(err)
		return
	}
	if res == nil {
		if aerr := APILogWrite(c.Request.Context(), db, id, OperationDelete, http.StatusNotFound); aerr != nil {
			log.WithError(aerr).WithField("resource_id", id).Warn("failed to write api log")
		}
		c.Status(http.StatusNotFound)
		return
	}
	if aerr := APILogWrite(c.Request.Context(), db, id, OperationDelete, http.StatusAccepted); aerr != nil {
		log.WithError(aerr).WithField("resource_id", id).Warn("failed to write api log")
	}
	c.JSON(http.StatusAccepted, res)
}
