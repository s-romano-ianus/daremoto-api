package admin

// DareMoto: admin view and manual assignment of plans (dedicated server, agreements
// outside Stripe). Requires an admin token in the "api-token" header.

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/http/response"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

type Billing struct{}

type billingSetForm struct {
	UserId        uint   `json:"user_id"`
	Plan          string `json:"plan"`
	ExtraSessions int    `json:"extra_sessions"`
	MaxMachines   int    `json:"max_machines"`
	MaxSessions   int    `json:"max_sessions"`
	Note          string `json:"note"`
}

// Get [GET] /api/admin/billing/subscription?user_id=1
func (b *Billing) Get(c *gin.Context) {
	id, _ := strconv.Atoi(c.Query("user_id"))
	if id <= 0 {
		response.Fail(c, 101, "user_id mancante")
		return
	}
	sub := service.Billing.Get(uint(id))
	response.Success(c, gin.H{
		"subscription":  sub,
		"limits":        service.Billing.Effective(sub),
		"machines_used": service.Billing.MachinesUsed(uint(id)),
	})
}

// Set [POST] /api/admin/billing/subscription
func (b *Billing) Set(c *gin.Context) {
	f := &billingSetForm{}
	if err := c.ShouldBindJSON(f); err != nil || f.UserId == 0 {
		response.Fail(c, 101, "Richiesta non valida")
		return
	}
	if service.AllService.UserService.InfoById(f.UserId).Id == 0 {
		response.Fail(c, 101, "Utente non trovato")
		return
	}
	sub, err := service.Billing.SetManual(f.UserId, f.Plan, f.ExtraSessions, f.MaxMachines, f.MaxSessions, f.Note)
	if err != nil {
		response.Fail(c, 101, err.Error())
		return
	}
	response.Success(c, service.Billing.Effective(service.Billing.Get(sub.UserId)))
}
