package model

// DareMoto: subscription plans and usage tracking.

const (
	PlanFree      = "free"
	PlanTeam      = "team"
	PlanBusiness  = "business"
	PlanDedicated = "dedicated"

	// SubscriptionStatusManual marks a plan assigned by an admin (dedicated server,
	// agreements outside Stripe). Stripe statuses (active, past_due, ...) are stored as-is.
	SubscriptionStatusManual = "manual"
)

// Subscription is the plan of one account. Users without a row are on the free plan.
type Subscription struct {
	IdModel
	UserId        uint   `json:"user_id" gorm:"not null;uniqueIndex"`
	Plan          string `json:"plan" gorm:"default:'free';not null"`
	ExtraSessions int    `json:"extra_sessions" gorm:"default:0;not null"`
	// Overrides of the plan limits (0 = plan default). Used for the dedicated plan.
	MaxMachines          int    `json:"max_machines" gorm:"default:0;not null"`
	MaxSessions          int    `json:"max_sessions" gorm:"default:0;not null"`
	StripeCustomerId     string `json:"stripe_customer_id" gorm:"default:'';not null;index"`
	StripeSubscriptionId string `json:"stripe_subscription_id" gorm:"default:'';not null;index"`
	Status               string `json:"status" gorm:"default:'';not null"`
	CurrentPeriodEnd     int64  `json:"current_period_end" gorm:"default:0;not null"`
	CancelAtPeriodEnd    bool   `json:"cancel_at_period_end" gorm:"default:0;not null"`
	Note                 string `json:"note" gorm:"default:'';not null"`
	// Expiry of a manual plan (unix time, 0 = never). Once past, the account is on the free
	// plan again, without anyone having to act.
	ManualUntil int64 `json:"manual_until" gorm:"default:0;not null"`
	TimeModel
}

// AccountMachine is a device an account connected to. A plan's machine limit counts the
// distinct devices seen in the last 30 days (filled by the rendezvous server check).
type AccountMachine struct {
	IdModel
	UserId   uint   `json:"user_id" gorm:"not null;uniqueIndex:idx_account_machine"`
	PeerId   string `json:"peer_id" gorm:"not null;uniqueIndex:idx_account_machine"`
	LastSeen int64  `json:"last_seen" gorm:"default:0;not null;index"`
	TimeModel
}
