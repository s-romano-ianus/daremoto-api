package service

// DareMoto: subscription plans, limits and Stripe billing.
//
// Configuration (environment of the api container):
//   DAREMOTO_STRIPE_SECRET_KEY      Stripe secret key (sk_test_... / sk_live_...). Billing is off without it.
//   DAREMOTO_STRIPE_WEBHOOK_SECRET  signing secret of the webhook endpoint (whsec_...). Optional when
//                                   data/billing.json, written by "apimain stripe-setup", has one.
//   DAREMOTO_PUBLIC_URL             public base URL of this server (default: rustdesk.api-server)
//   DAREMOTO_STRIPE_AUTOMATIC_TAX   1 to let Stripe Tax compute VAT (needs Stripe Tax set up)
//
// Prices are looked up by lookup_key, so the Stripe dashboard stays the single source of
// truth for amounts. Plan changes go through ChangePlan (the Stripe customer portal cannot
// change subscriptions with several items); the portal handles payment methods, invoices
// and cancellation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"github.com/stripe/stripe-go/v85"
	"github.com/stripe/stripe-go/v85/webhook"
)

type PlanDef struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Machines  int    `json:"machines"`
	Sessions  int    `json:"sessions"`
	LookupKey string `json:"-"`
}

const (
	LookupKeyTeam         = "daremoto_team_monthly"
	LookupKeyBusiness     = "daremoto_business_monthly"
	LookupKeyExtraSession = "daremoto_extra_session_monthly"
	MachinesWindow        = 30 * 24 * time.Hour
	MaxExtraSessions      = 50
	billingFile           = "data/billing.json"
)

var Plans = map[string]PlanDef{
	model.PlanFree:      {Key: model.PlanFree, Name: "Gratuito", Machines: 1, Sessions: 1},
	model.PlanTeam:      {Key: model.PlanTeam, Name: "Team", Machines: 30, Sessions: 2, LookupKey: LookupKeyTeam},
	model.PlanBusiness:  {Key: model.PlanBusiness, Name: "Business", Machines: 100, Sessions: 5, LookupKey: LookupKeyBusiness},
	model.PlanDedicated: {Key: model.PlanDedicated, Name: "Server dedicato", Machines: 1000, Sessions: 20},
}

// BillingStored is written by "apimain stripe-setup" into data/billing.json.
type BillingStored struct {
	WebhookSecret       string `json:"webhook_secret"`
	WebhookEndpointId   string `json:"webhook_endpoint_id"`
	PortalConfiguration string `json:"portal_configuration"`
	Livemode            bool   `json:"livemode"`
}

type BillingService struct {
	once      sync.Once
	client    *stripe.Client
	secretKey string
	webhook   string
	publicURL string
	autoTax   bool
	stored    BillingStored

	priceMu  sync.Mutex
	prices   map[string]*stripe.Price
	pricesAt time.Time
	changeMu sync.Mutex
}

var Billing = &BillingService{}

var ErrBillingDisabled = errors.New("i pagamenti non sono ancora attivi")

func (s *BillingService) init() {
	s.once.Do(func() {
		s.secretKey = strings.TrimSpace(os.Getenv("DAREMOTO_STRIPE_SECRET_KEY"))
		s.publicURL = strings.TrimRight(strings.TrimSpace(os.Getenv("DAREMOTO_PUBLIC_URL")), "/")
		if s.publicURL == "" && Config != nil {
			s.publicURL = strings.TrimRight(Config.Rustdesk.ApiServer, "/")
		}
		s.autoTax = os.Getenv("DAREMOTO_STRIPE_AUTOMATIC_TAX") == "1"
		if b, err := os.ReadFile(billingFile); err == nil {
			_ = json.Unmarshal(b, &s.stored)
		}
		s.webhook = strings.TrimSpace(os.Getenv("DAREMOTO_STRIPE_WEBHOOK_SECRET"))
		if s.webhook == "" {
			s.webhook = s.stored.WebhookSecret
		}
		if s.secretKey != "" {
			s.client = stripe.NewClient(s.secretKey)
		}
	})
}

// Enabled reports whether Stripe is configured.
func (s *BillingService) Enabled() bool { s.init(); return s.client != nil }

func (s *BillingService) Client() *stripe.Client { s.init(); return s.client }

func (s *BillingService) PublicURL() string { s.init(); return s.publicURL }

// Migrate creates the billing tables. Called at startup, independent of upstream versions.
func (s *BillingService) Migrate() error {
	return DB.AutoMigrate(&model.Subscription{}, &model.AccountMachine{})
}

// ---------------------------------------------------------------- plans and limits

func (s *BillingService) Get(userId uint) *model.Subscription {
	sub := &model.Subscription{}
	DB.Where("user_id = ?", userId).First(sub)
	if sub.Id == 0 {
		sub.UserId = userId
		sub.Plan = model.PlanFree
	}
	return sub
}

func (s *BillingService) getOrCreate(userId uint) *model.Subscription {
	sub := s.Get(userId)
	if sub.Id == 0 {
		DB.Create(sub)
	}
	return sub
}

// IsPaidStatus: statuses that keep a paid plan. past_due keeps it while Stripe retries the
// payment; what happens after the last retry (cancel or unpaid) is set in the dashboard.
func IsPaidStatus(status string) bool {
	switch status {
	case "active", "trialing", "past_due", model.SubscriptionStatusManual:
		return true
	}
	return false
}

type Limits struct {
	Plan     string `json:"plan"`
	PlanName string `json:"plan_name"`
	Machines int    `json:"machines"`
	Sessions int    `json:"sessions"`
}

// Effective returns the plan and limits that apply now.
// ManualExpired: a manual plan whose expiry date has passed (it then counts as free).
func ManualExpired(sub *model.Subscription) bool {
	return sub != nil && sub.Status == model.SubscriptionStatusManual &&
		sub.ManualUntil > 0 && time.Now().Unix() > sub.ManualUntil
}

func (s *BillingService) Effective(sub *model.Subscription) Limits {
	key := model.PlanFree
	if sub != nil && sub.Plan != model.PlanFree && IsPaidStatus(sub.Status) && !ManualExpired(sub) {
		if _, ok := Plans[sub.Plan]; ok {
			key = sub.Plan
		}
	}
	p := Plans[key]
	l := Limits{Plan: p.Key, PlanName: p.Name, Machines: p.Machines, Sessions: p.Sessions}
	if key != model.PlanFree {
		if sub.MaxMachines > 0 {
			l.Machines = sub.MaxMachines
		}
		if sub.MaxSessions > 0 {
			l.Sessions = sub.MaxSessions
		}
		l.Sessions += sub.ExtraSessions
	}
	return l
}

func (s *BillingService) LimitsFor(userId uint) Limits { return s.Effective(s.Get(userId)) }

// MachinesUsed counts the distinct devices the account connected to in the last 30 days.
func (s *BillingService) MachinesUsed(userId uint) int64 {
	var n int64
	DB.Model(&model.AccountMachine{}).
		Where("user_id = ? AND last_seen >= ?", userId, time.Now().Add(-MachinesWindow).Unix()).
		Count(&n)
	return n
}

// SetManual assigns a plan by hand (admin): dedicated servers, agreements outside Stripe.
// plan "free" removes a manual plan. A live Stripe subscription is left untouched.
// until is the expiry (unix time, 0 = never).
func (s *BillingService) SetManual(userId uint, plan string, extra, maxMachines, maxSessions int, note string, until int64) (*model.Subscription, error) {
	if _, ok := Plans[plan]; !ok {
		return nil, fmt.Errorf("piano sconosciuto: %s", plan)
	}
	sub := s.getOrCreate(userId)
	if sub.StripeSubscriptionId != "" && IsPaidStatus(sub.Status) && sub.Status != model.SubscriptionStatusManual {
		return nil, errors.New("l'utente ha un abbonamento Stripe attivo: modificalo da Stripe")
	}
	status := model.SubscriptionStatusManual
	if plan == model.PlanFree {
		status = ""
		until = 0
	}
	err := DB.Model(sub).Updates(map[string]interface{}{
		"plan": plan, "status": status, "extra_sessions": extra,
		"max_machines": maxMachines, "max_sessions": maxSessions, "note": note,
		"manual_until": until,
	}).Error
	return sub, err
}

// AdminUser is one row of the admin search: the user and their plan.
type AdminUser struct {
	Id           uint                `json:"id"`
	Username     string              `json:"username"`
	Email        string              `json:"email"`
	Nickname     string              `json:"nickname"`
	Subscription *model.Subscription `json:"subscription"`
	Limits       Limits              `json:"limits"`
	MachinesUsed int64               `json:"machines_used"`
	Expired      bool                `json:"expired"`
}

// SearchUsers finds users by username, email or nickname (empty query: the latest ones;
// gifted: only the users with a manual plan).
func (s *BillingService) SearchUsers(q string, gifted bool) []AdminUser {
	var users []model.User
	tx := DB.Model(&model.User{}).Order("id desc").Limit(30)
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + q + "%"
		tx = tx.Where("username LIKE ? OR email LIKE ? OR nickname LIKE ?", like, like, like)
	}
	if gifted {
		tx = tx.Where("id IN (?)", DB.Model(&model.Subscription{}).Select("user_id").
			Where("status = ? AND plan <> ?", model.SubscriptionStatusManual, model.PlanFree))
	}
	tx.Find(&users)
	out := make([]AdminUser, 0, len(users))
	for _, u := range users {
		sub := s.Get(u.Id)
		out = append(out, AdminUser{
			Id: u.Id, Username: u.Username, Email: u.Email, Nickname: u.Nickname,
			Subscription: sub, Limits: s.Effective(sub), MachinesUsed: s.MachinesUsed(u.Id),
			Expired: ManualExpired(sub),
		})
	}
	return out
}

// ---------------------------------------------------------------- Stripe prices

type PriceInfo struct {
	Id       string `json:"-"`
	Amount   int64  `json:"amount"` // cents
	Currency string `json:"currency"`
	Interval string `json:"interval"`
}

func (s *BillingService) Prices(ctx context.Context) (map[string]*stripe.Price, error) {
	if !s.Enabled() {
		return nil, ErrBillingDisabled
	}
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	if s.prices != nil && time.Since(s.pricesAt) < 10*time.Minute {
		return s.prices, nil
	}
	params := &stripe.PriceListParams{
		Active:     stripe.Bool(true),
		LookupKeys: stripe.StringSlice([]string{LookupKeyTeam, LookupKeyBusiness, LookupKeyExtraSession}),
	}
	m := map[string]*stripe.Price{}
	for p, err := range s.client.V1Prices.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, err
		}
		m[p.LookupKey] = p
	}
	for _, k := range []string{LookupKeyTeam, LookupKeyBusiness, LookupKeyExtraSession} {
		if m[k] == nil {
			return nil, fmt.Errorf("prezzo Stripe mancante: %s (eseguire apimain stripe-setup)", k)
		}
	}
	s.prices, s.pricesAt = m, time.Now()
	return m, nil
}

func (s *BillingService) PriceInfos(ctx context.Context) map[string]PriceInfo {
	out := map[string]PriceInfo{}
	prices, err := s.Prices(ctx)
	if err != nil {
		return out
	}
	names := map[string]string{LookupKeyTeam: model.PlanTeam, LookupKeyBusiness: model.PlanBusiness, LookupKeyExtraSession: "extra_session"}
	for k, p := range prices {
		pi := PriceInfo{Id: p.ID, Amount: p.UnitAmount, Currency: string(p.Currency)}
		if p.Recurring != nil {
			pi.Interval = string(p.Recurring.Interval)
		}
		out[names[k]] = pi
	}
	return out
}

func planFromLookup(lookup string) string {
	for k, p := range Plans {
		if p.LookupKey != "" && p.LookupKey == lookup {
			return k
		}
	}
	return ""
}

func validPurchase(plan string, extra int) error {
	if plan != model.PlanTeam && plan != model.PlanBusiness {
		return errors.New("piano non acquistabile online")
	}
	if extra < 0 || extra > MaxExtraSessions {
		return fmt.Errorf("sessioni extra: da 0 a %d", MaxExtraSessions)
	}
	return nil
}

// ---------------------------------------------------------------- checkout / change / portal

func (s *BillingService) ensureCustomer(ctx context.Context, u *model.User, sub *model.Subscription) (string, error) {
	if sub.StripeCustomerId != "" {
		return sub.StripeCustomerId, nil
	}
	params := &stripe.CustomerCreateParams{
		Metadata: map[string]string{"user_id": strconv.Itoa(int(u.Id)), "username": u.Username},
	}
	if u.Email != "" {
		params.Email = stripe.String(u.Email)
	}
	if u.Nickname != "" {
		params.Name = stripe.String(u.Nickname)
	}
	params.SetIdempotencyKey(fmt.Sprintf("daremoto-customer-%d", u.Id))
	c, err := s.client.V1Customers.Create(ctx, params)
	if err != nil {
		return "", err
	}
	DB.Model(sub).Update("stripe_customer_id", c.ID)
	sub.StripeCustomerId = c.ID
	return c.ID, nil
}

func (s *BillingService) hasLiveSubscription(sub *model.Subscription) bool {
	return sub.StripeSubscriptionId != "" && sub.Status != "canceled" && sub.Status != "incomplete_expired" && sub.Status != ""
}

// Checkout starts a Stripe Checkout session for a new subscription and returns its URL.
func (s *BillingService) Checkout(ctx context.Context, u *model.User, plan string, extra int) (string, error) {
	if !s.Enabled() {
		return "", ErrBillingDisabled
	}
	if err := validPurchase(plan, extra); err != nil {
		return "", err
	}
	sub := s.getOrCreate(u.Id)
	if s.hasLiveSubscription(sub) {
		return "", errors.New("hai già un abbonamento: cambia piano da questa pagina")
	}
	if sub.Status == model.SubscriptionStatusManual && sub.Plan != model.PlanFree && !ManualExpired(sub) {
		return "", errors.New("il tuo piano è gestito da Ianus Project: contattaci per modificarlo")
	}
	prices, err := s.Prices(ctx)
	if err != nil {
		return "", err
	}
	customer, err := s.ensureCustomer(ctx, u, sub)
	if err != nil {
		return "", err
	}
	items := []*stripe.CheckoutSessionCreateLineItemParams{
		{Price: stripe.String(prices[Plans[plan].LookupKey].ID), Quantity: stripe.Int64(1)},
	}
	if extra > 0 {
		items = append(items, &stripe.CheckoutSessionCreateLineItemParams{
			Price: stripe.String(prices[LookupKeyExtraSession].ID), Quantity: stripe.Int64(int64(extra)),
		})
	}
	uid := strconv.Itoa(int(u.Id))
	params := &stripe.CheckoutSessionCreateParams{
		Mode:                     stripe.String("subscription"),
		Customer:                 stripe.String(customer),
		ClientReferenceID:        stripe.String(uid),
		LineItems:                items,
		Locale:                   stripe.String("it"),
		AllowPromotionCodes:      stripe.Bool(true),
		BillingAddressCollection: stripe.String("required"),
		CustomerUpdate:           &stripe.CheckoutSessionCreateCustomerUpdateParams{Address: stripe.String("auto"), Name: stripe.String("auto")},
		TaxIDCollection:          &stripe.CheckoutSessionCreateTaxIDCollectionParams{Enabled: stripe.Bool(true)},
		SubscriptionData:         &stripe.CheckoutSessionCreateSubscriptionDataParams{Metadata: map[string]string{"user_id": uid}},
		Metadata:                 map[string]string{"user_id": uid},
		// Ianus Project is the seller: Managed Payments (Stripe as merchant of record)
		// would limit Checkout to cards, without PayPal and SEPA Direct Debit.
		ManagedPayments: &stripe.CheckoutSessionCreateManagedPaymentsParams{Enabled: stripe.Bool(false)},
		SuccessURL:      stripe.String(s.publicURL + "/account/?checkout=success"),
		CancelURL:       stripe.String(s.publicURL + "/account/?checkout=cancel"),
	}
	if s.autoTax {
		params.AutomaticTax = &stripe.CheckoutSessionCreateAutomaticTaxParams{Enabled: stripe.Bool(true)}
	}
	cs, err := s.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return cs.URL, nil
}

// ChangePlan switches plan and/or extra sessions of the live subscription. The difference
// is invoiced right away (an upgrade is paid now, a downgrade leaves a credit).
func (s *BillingService) ChangePlan(ctx context.Context, u *model.User, plan string, extra int) error {
	if !s.Enabled() {
		return ErrBillingDisabled
	}
	if err := validPurchase(plan, extra); err != nil {
		return err
	}
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	sub := s.Get(u.Id)
	if !s.hasLiveSubscription(sub) {
		return errors.New("nessun abbonamento attivo da modificare")
	}
	prices, err := s.Prices(ctx)
	if err != nil {
		return err
	}
	cur, err := s.client.V1Subscriptions.Retrieve(ctx, sub.StripeSubscriptionId, nil)
	if err != nil {
		return err
	}
	var items []*stripe.SubscriptionUpdateItemParams
	var planItem, extraItem *stripe.SubscriptionItem
	for _, it := range cur.Items.Data {
		if it.Price == nil {
			continue
		}
		if it.Price.LookupKey == LookupKeyExtraSession {
			extraItem = it
		} else if planFromLookup(it.Price.LookupKey) != "" {
			planItem = it
		}
	}
	if planItem == nil {
		return errors.New("abbonamento senza piano riconosciuto: contatta l'assistenza")
	}
	if planItem.Price.ID != prices[Plans[plan].LookupKey].ID {
		items = append(items, &stripe.SubscriptionUpdateItemParams{ID: stripe.String(planItem.ID), Price: stripe.String(prices[Plans[plan].LookupKey].ID)})
	}
	switch {
	case extraItem != nil && extra == 0:
		items = append(items, &stripe.SubscriptionUpdateItemParams{ID: stripe.String(extraItem.ID), Deleted: stripe.Bool(true)})
	case extraItem != nil && extraItem.Quantity != int64(extra):
		items = append(items, &stripe.SubscriptionUpdateItemParams{ID: stripe.String(extraItem.ID), Quantity: stripe.Int64(int64(extra))})
	case extraItem == nil && extra > 0:
		items = append(items, &stripe.SubscriptionUpdateItemParams{Price: stripe.String(prices[LookupKeyExtraSession].ID), Quantity: stripe.Int64(int64(extra))})
	}
	if len(items) == 0 {
		return nil
	}
	updated, err := s.client.V1Subscriptions.Update(ctx, cur.ID, &stripe.SubscriptionUpdateParams{
		Items:             items,
		ProrationBehavior: stripe.String("always_invoice"),
		PaymentBehavior:   stripe.String("pending_if_incomplete"),
	})
	if err != nil {
		return err
	}
	return s.syncFrom(updated)
}

// Portal returns a Stripe customer portal URL (payment methods, invoices, cancellation).
func (s *BillingService) Portal(ctx context.Context, u *model.User) (string, error) {
	if !s.Enabled() {
		return "", ErrBillingDisabled
	}
	sub := s.Get(u.Id)
	if sub.StripeCustomerId == "" {
		return "", errors.New("nessun abbonamento a pagamento")
	}
	params := &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(sub.StripeCustomerId),
		ReturnURL: stripe.String(s.publicURL + "/account/"),
		Locale:    stripe.String("it"),
	}
	if s.stored.PortalConfiguration != "" {
		params.Configuration = stripe.String(s.stored.PortalConfiguration)
	}
	ps, err := s.client.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return ps.URL, nil
}

// ---------------------------------------------------------------- webhook and sync

// HandleWebhook verifies the signature and syncs the subscription the event refers to.
// It always re-reads the subscription from Stripe, so event order and retries don't matter.
func (s *BillingService) HandleWebhook(ctx context.Context, payload []byte, signature string) error {
	if !s.Enabled() || s.webhook == "" {
		return ErrBillingDisabled
	}
	ev, err := webhook.ConstructEventWithOptions(payload, signature, s.webhook, webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		return fmt.Errorf("firma webhook non valida: %w", err)
	}
	var obj map[string]interface{}
	_ = json.Unmarshal(ev.Data.Raw, &obj)
	subId := ""
	switch {
	case ev.Type == "checkout.session.completed" || ev.Type == "checkout.session.async_payment_succeeded":
		subId = strField(obj, "subscription")
	case strings.HasPrefix(string(ev.Type), "customer.subscription."):
		subId = strField(obj, "id")
	case strings.HasPrefix(string(ev.Type), "invoice."):
		subId = strField(obj, "subscription")
		if subId == "" {
			if parent, ok := obj["parent"].(map[string]interface{}); ok {
				if det, ok := parent["subscription_details"].(map[string]interface{}); ok {
					subId = strField(det, "subscription")
				}
			}
		}
	}
	if subId == "" {
		return nil
	}
	return s.Sync(ctx, subId)
}

func strField(m map[string]interface{}, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case map[string]interface{}:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}

func (s *BillingService) Sync(ctx context.Context, subscriptionId string) error {
	st, err := s.client.V1Subscriptions.Retrieve(ctx, subscriptionId, nil)
	if err != nil {
		return err
	}
	return s.syncFrom(st)
}

func (s *BillingService) syncFrom(st *stripe.Subscription) error {
	customerId := ""
	if st.Customer != nil {
		customerId = st.Customer.ID
	}
	var userId uint
	if v, err := strconv.Atoi(st.Metadata["user_id"]); err == nil && v > 0 {
		userId = uint(v)
	} else if customerId != "" {
		byCustomer := &model.Subscription{}
		DB.Where("stripe_customer_id = ?", customerId).First(byCustomer)
		userId = byCustomer.UserId
	}
	if userId == 0 {
		Logger.Warnf("stripe: subscription %s without a known user", st.ID)
		return nil
	}
	sub := s.getOrCreate(userId)
	status := string(st.Status)
	// An old, ended subscription must not overwrite a newer one.
	if sub.StripeSubscriptionId != "" && sub.StripeSubscriptionId != st.ID && !IsPaidStatus(status) {
		return nil
	}
	plan, extra := model.PlanFree, 0
	var periodEnd int64
	for _, it := range st.Items.Data {
		if it.Price == nil {
			continue
		}
		if it.Price.LookupKey == LookupKeyExtraSession {
			extra = int(it.Quantity)
		} else if p := planFromLookup(it.Price.LookupKey); p != "" {
			plan = p
		}
		if it.CurrentPeriodEnd > periodEnd {
			periodEnd = it.CurrentPeriodEnd
		}
	}
	upd := map[string]interface{}{
		"plan": plan, "extra_sessions": extra, "status": status,
		"stripe_subscription_id": st.ID, "current_period_end": periodEnd,
		"cancel_at_period_end": st.CancelAtPeriodEnd || st.CancelAt > 0,
		"max_machines":         0, "max_sessions": 0,
	}
	if customerId != "" {
		upd["stripe_customer_id"] = customerId
	}
	if err := DB.Model(sub).Updates(upd).Error; err != nil {
		return err
	}
	Logger.Infof("stripe: user %d -> plan %s (+%d sessions), status %s", userId, plan, extra, status)
	return nil
}
