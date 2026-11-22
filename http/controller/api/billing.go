package api

// DareMoto: subscription status, Stripe checkout, plan changes, customer portal, webhook.

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/global"
	"github.com/lejianwen/rustdesk-api/v2/http/response"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

type Billing struct{}

type billingPurchaseForm struct {
	Plan          string `json:"plan"`
	ExtraSessions int    `json:"extra_sessions"`
}

// Status: plan, limits, usage and prices of the current user.
// [GET] /api/billing/status
func (b *Billing) Status(c *gin.Context) {
	u := service.AllService.UserService.CurUser(c)
	bs := service.Billing
	sub := bs.Get(u.Id)
	limits := bs.Effective(sub)
	response.Success(c, gin.H{
		"user":            gin.H{"username": u.Username, "email": u.Email, "nickname": u.Nickname},
		"billing_enabled": bs.Enabled(),
		"plan":            limits.Plan,
		"plan_name":       limits.PlanName,
		"limits":          gin.H{"machines": limits.Machines, "sessions": limits.Sessions},
		"usage":           gin.H{"machines": bs.MachinesUsed(u.Id)},
		"subscription": gin.H{
			"plan":                 sub.Plan,
			"status":               sub.Status,
			"extra_sessions":       sub.ExtraSessions,
			"current_period_end":   sub.CurrentPeriodEnd,
			"cancel_at_period_end": sub.CancelAtPeriodEnd,
			"stripe":               sub.StripeSubscriptionId != "" && sub.Status != "canceled" && sub.Status != "incomplete_expired",
			"has_customer":         sub.StripeCustomerId != "",
		},
		"plans":  service.Plans,
		"prices": bs.PriceInfos(c.Request.Context()),
	})
}

// Checkout starts a Stripe Checkout session. [POST] /api/billing/checkout
func (b *Billing) Checkout(c *gin.Context) {
	f := &billingPurchaseForm{}
	if err := c.ShouldBindJSON(f); err != nil {
		response.Error(c, "Richiesta non valida")
		return
	}
	u := service.AllService.UserService.CurUser(c)
	url, err := service.Billing.Checkout(c.Request.Context(), u, f.Plan, f.ExtraSessions)
	if err != nil {
		global.Logger.Warnf("billing checkout user %d: %v", u.Id, err)
		response.Error(c, err.Error())
		return
	}
	response.Success(c, gin.H{"url": url})
}

// Change switches plan / extra sessions of the live subscription. [POST] /api/billing/change
func (b *Billing) Change(c *gin.Context) {
	f := &billingPurchaseForm{}
	if err := c.ShouldBindJSON(f); err != nil {
		response.Error(c, "Richiesta non valida")
		return
	}
	u := service.AllService.UserService.CurUser(c)
	if err := service.Billing.ChangePlan(c.Request.Context(), u, f.Plan, f.ExtraSessions); err != nil {
		global.Logger.Warnf("billing change user %d: %v", u.Id, err)
		response.Error(c, err.Error())
		return
	}
	response.Success(c, nil)
}

// Portal returns the Stripe customer portal URL. [POST] /api/billing/portal
func (b *Billing) Portal(c *gin.Context) {
	u := service.AllService.UserService.CurUser(c)
	url, err := service.Billing.Portal(c.Request.Context(), u)
	if err != nil {
		global.Logger.Warnf("billing portal user %d: %v", u.Id, err)
		response.Error(c, err.Error())
		return
	}
	response.Success(c, gin.H{"url": url})
}

// Webhook receives Stripe events. [POST] /api/billing/webhook
func (b *Billing) Webhook(c *gin.Context) {
	payload, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}
	if err := service.Billing.HandleWebhook(c.Request.Context(), payload, c.GetHeader("Stripe-Signature")); err != nil {
		global.Logger.Warnf("stripe webhook: %v", err)
		// 400 makes Stripe retry: right for signature or transient API errors.
		c.String(http.StatusBadRequest, "error")
		return
	}
	c.String(http.StatusOK, "ok")
}
