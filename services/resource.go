package services

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// PUT /resource/{id} — queue storing of a resource (id = infohash)
// putResource godoc
// @Summary      Queue storing of a resource
// @Description  Creates the resource if missing or marks it queued for processing
// @Tags         resource
// @Param        id   path      string  true  "Resource ID"
// @Success      202  {object}  Resource
// @Failure      500  {object}  ErrorResponse
// @Router       /resource/{id} [put]
func (s *Web) putResource(c *gin.Context) {
	id := c.Param("id")
	db := s.pg.Get()
	if db == nil {
		_ = c.Error(errors.New("DB not configured"))
		return
	}
	res, err := ResourceQueueForStoring(c.Request.Context(), db, id)
	if err != nil {
		_ = c.Error(err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"resource": res})
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
	c.JSON(http.StatusOK, gin.H{"resource": res})
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
	res, err := ResourceQueueForDeletion(context.Background(), db, id)
	if err != nil {
		_ = c.Error(err)
		return
	}
	if res == nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"resource": res})
}
