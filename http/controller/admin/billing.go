package admin

// DareMoto: admin view and manual assignment of plans (dedicated server, agreements
// outside Stripe). Requires an admin token in the "api-token" header.

import (
	"strconv"
	"time"
	_ "time/tzdata" // Europe/Rome also without tzdata in the image

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
	// last day of a gifted plan, "2026-03-31" (Italian time, end of day); empty = never
	Until string `json:"until"`
}

// Users [GET] /api/admin/billing/users?q=mario&gifted=1
func (b *Billing) Users(c *gin.Context) {
	response.Success(c, service.Billing.SearchUsers(c.Query("q"), c.Query("gifted") == "1"))
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
	var until int64
	if f.Until != "" {
		loc, err := time.LoadLocation("Europe/Rome")
		if err != nil {
			loc = time.Local
		}
		day, err := time.ParseInLocation("2006-01-02", f.Until, loc)
		if err != nil {
			response.Fail(c, 101, "Data di scadenza non valida")
			return
		}
		until = day.AddDate(0, 0, 1).Unix() - 1
		if until < time.Now().Unix() {
			response.Fail(c, 101, "La data di scadenza è già passata")
			return
		}
	}
	if f.MaxMachines < 0 || f.MaxSessions < 0 || f.ExtraSessions < 0 {
		response.Fail(c, 101, "I limiti non possono essere negativi")
		return
	}
	sub, err := service.Billing.SetManual(f.UserId, f.Plan, f.ExtraSessions, f.MaxMachines, f.MaxSessions, f.Note, until)
	if err != nil {
		response.Fail(c, 101, err.Error())
		return
	}
	response.Success(c, service.Billing.Effective(service.Billing.Get(sub.UserId)))
}
