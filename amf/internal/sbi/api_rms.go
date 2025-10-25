package sbi

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/free5gc/amf/internal/rms"
	"github.com/free5gc/util/idgenerator"
)

func (s *Server) getRMSRoutes() []Route {
	return []Route{
		{
			Name:    "root",
			Method:  http.MethodGet,
			Pattern: "/",
			APIFunc: func(c *gin.Context) {
				c.String(http.StatusOK, "Hello World!")
			},
		},
		{
			Name:    "GetSubscriptions",
			Method:  http.MethodGet,
			Pattern: "/subscriptions",
			APIFunc: s.getSubscriptions,
		},
		{
			Name:    "CreateSubscription",
			Method:  http.MethodPost,
			Pattern: "/subscriptions/",
			APIFunc: s.createSubscription,
		},
		{
			Name:    "UpdateSubscription",
			Method:  http.MethodPut,
			Pattern: "/subscriptions/:subscriptionID",
			APIFunc: s.updateSubscription,
		},
		{
			Name:    "DeleteSubscription",
			Method:  http.MethodDelete,
			Pattern: "/subscriptions/:subscriptionID",
			APIFunc: s.deleteSubscription,
		},
	}
}

func (s *Server) getSubscriptions(c *gin.Context) {
	store := rms.GetSubscriptionStore()
	subscriptions := store.GetAll()

	response := struct {
		Subscriptions []*rms.Subscription `json:"subscriptions"`
	}{
		Subscriptions: subscriptions,
	}

	c.JSON(http.StatusOK, response)
}

func (s *Server) createSubscription(c *gin.Context) {
	var req rms.Subscription
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON payload"})
		return
	}

	if req.UeId == "" || req.NotifyUri == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing required fields: ueId and notifyUri"})
		return
	}

	subId := generateSubscriptionId()
	req.SubId = subId

	store := rms.GetSubscriptionStore()
	store.Create(&req)

	c.JSON(http.StatusCreated, req)
}

func (s *Server) updateSubscription(c *gin.Context) {
	subscriptionID := c.Param("subscriptionID")
	if subscriptionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing subscriptionID"})
		return
	}

	var req rms.Subscription
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON payload"})
		return
	}

	if req.UeId == "" || req.NotifyUri == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing required fields: ueId and notifyUri"})
		return
	}

	store := rms.GetSubscriptionStore()
	if store.Update(subscriptionID, &req) {
		c.JSON(http.StatusOK, req)
	} else {
		req.SubId = subscriptionID
		store.Create(&req)
		c.JSON(http.StatusCreated, req)
	}
}

func (s *Server) deleteSubscription(c *gin.Context) {
	subscriptionID := c.Param("subscriptionID")
	if subscriptionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing subscriptionID"})
		return
	}

	store := rms.GetSubscriptionStore()
	if store.Delete(subscriptionID) {
		c.Status(http.StatusNoContent)
	} else {
		c.JSON(http.StatusNotFound, gin.H{"error": "Subscription not found"})
	}
}

var (
	subIdGenerator     *idgenerator.IDGenerator
	subIdGeneratorOnce sync.Once
)

func getSubIdGenerator() *idgenerator.IDGenerator {
	subIdGeneratorOnce.Do(func() {
		subIdGenerator = idgenerator.NewGenerator(1, 9999999)
	})
	return subIdGenerator
}

func generateSubscriptionId() string {
	id, err := getSubIdGenerator().Allocate()
	if err != nil {
		id = 1
	}
	return fmt.Sprintf("sub-%s", strconv.FormatInt(id, 10))
}
