package service

// DareMoto: plan enforcement (Authorize, audit binding, heartbeat liveness).
//   go test ./service -run TestSession -v

import (
	"strings"
	"testing"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/config"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSessionTest(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.UserToken{}, &model.Subscription{}, &model.AccountMachine{}); err != nil {
		t.Fatal(err)
	}
	DB = db
	AllService = &Service{UserService: &UserService{}}
	Sessions = &SessionService{hb: map[string]*peerConns{}}
}

func addUser(t *testing.T, name string, status model.StatusCode) string {
	t.Helper()
	u := &model.User{Username: name, Status: status}
	if err := DB.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	tok := "tok-" + name
	DB.Create(&model.UserToken{UserId: u.Id, Token: tok, DeviceId: "dev-" + name, ExpiredAt: time.Now().Add(time.Hour).Unix()})
	return tok
}

func mustAllow(t *testing.T, tok, target string) {
	t.Helper()
	if r := Sessions.Authorize(tok, target); !r.Allow {
		t.Fatalf("expected allow for %s -> %s, got: %s", tok, target, r.Message)
	}
}

func mustDeny(t *testing.T, tok, target, contains string) {
	t.Helper()
	r := Sessions.Authorize(tok, target)
	if r.Allow {
		t.Fatalf("expected deny for %s -> %s", tok, target)
	}
	if !strings.Contains(r.Message, contains) {
		t.Fatalf("deny message %q does not contain %q", r.Message, contains)
	}
}

func TestSessionAnonymousAndInvalid(t *testing.T) {
	setupSessionTest(t)
	mustDeny(t, "", "111", "accedi")
	mustDeny(t, "nope", "111", "scaduto")
	tok := addUser(t, "off", model.COMMON_STATUS_DISABLED)
	mustDeny(t, tok, "111", "disattivato")
}

func TestSessionFreePlanLimits(t *testing.T) {
	setupSessionTest(t)
	tok := addUser(t, "free", model.COMMON_STATUS_ENABLE)

	mustAllow(t, tok, "111")
	// same device again (retry, file transfer): no extra machine or session
	mustAllow(t, tok, "111")
	// a second device: free plan has 1 machine
	mustDeny(t, tok, "222", "1 PC")
}

func TestSessionConcurrency(t *testing.T) {
	setupSessionTest(t)
	tok := addUser(t, "team", model.COMMON_STATUS_ENABLE)
	DB.Create(&model.Subscription{UserId: 1, Plan: model.PlanTeam, Status: "active"}) // 30 PC, 2 sessions

	mustAllow(t, tok, "111")
	mustAllow(t, tok, "222")
	mustDeny(t, tok, "333", "2 sessioni aperte")

	// device 111 reports the connection, then its heartbeat drops it: the slot is free
	Sessions.OnAuditPeer("111", 7, "dev-team")
	Sessions.OnHeartbeat("111", []int64{7})
	if n := Sessions.ActiveSessions(1); n != 2 {
		t.Fatalf("active sessions = %d, want 2", n)
	}
	time.Sleep(10 * time.Millisecond)
	Sessions.OnHeartbeat("111", nil)
	// 222 is still pending (not reported yet): 1 active
	if n := Sessions.ActiveSessions(1); n != 1 {
		t.Fatalf("active sessions after close = %d, want 1", n)
	}
	mustAllow(t, tok, "333")

	// extra sessions raise the limit
	DB.Model(&model.Subscription{}).Where("user_id = 1").Update("extra_sessions", 1)
	mustAllow(t, tok, "444")
	mustDeny(t, tok, "555", "3 sessioni aperte")
}

func TestSessionPendingExpires(t *testing.T) {
	setupSessionTest(t)
	tok := addUser(t, "free", model.COMMON_STATUS_ENABLE)
	DB.Create(&model.Subscription{UserId: 1, Plan: model.PlanTeam, Status: "active", MaxSessions: 1})
	mustAllow(t, tok, "111")
	mustDeny(t, tok, "222", "una sessione aperta")
	// a connection that never got reported stops counting after the pending TTL
	Sessions.mu.Lock()
	Sessions.leases[0].created = time.Now().Add(-leasePendingTTL - time.Second)
	Sessions.mu.Unlock()
	mustAllow(t, tok, "222")
}

func TestSessionCanceledPlanFallsBackToFree(t *testing.T) {
	setupSessionTest(t)
	tok := addUser(t, "gone", model.COMMON_STATUS_ENABLE)
	DB.Create(&model.Subscription{UserId: 1, Plan: model.PlanBusiness, Status: "canceled"})
	mustAllow(t, tok, "111")
	mustDeny(t, tok, "222", "Gratuito")
}

func TestManualPlanExpiry(t *testing.T) {
	setupSessionTest(t)
	tok := addUser(t, "tester", model.COMMON_STATUS_ENABLE)
	var u model.User
	DB.Where("username = ?", "tester").First(&u)

	// gifted dedicated plan with custom limits, still valid
	if _, err := Billing.SetManual(u.Id, model.PlanDedicated, 0, 5000, 50, "tester", time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if l := Billing.LimitsFor(u.Id); l.Plan != model.PlanDedicated || l.Machines != 5000 || l.Sessions != 50 {
		t.Fatalf("unexpected limits while valid: %+v", l)
	}
	mustAllow(t, tok, "pc-1")
	mustAllow(t, tok, "pc-2")

	// expired: back to free (1 machine, already used by pc-1 and pc-2)
	DB.Model(&model.Subscription{}).Where("user_id = ?", u.Id).Update("manual_until", time.Now().Add(-time.Minute).Unix())
	if l := Billing.LimitsFor(u.Id); l.Plan != model.PlanFree {
		t.Fatalf("expected free after expiry, got %+v", l)
	}
	mustDeny(t, tok, "pc-3", "Gratuito")

	// revoked: free, expiry cleared
	sub, err := Billing.SetManual(u.Id, model.PlanFree, 0, 0, 0, "", 123)
	if err != nil || Billing.Get(sub.UserId).ManualUntil != 0 {
		t.Fatalf("revoke: %v", err)
	}

	// search finds the user
	if r := Billing.SearchUsers("test", false); len(r) != 1 || r[0].Username != "tester" {
		t.Fatalf("search: %+v", r)
	}
}

func TestLoginWithUsernameOrEmail(t *testing.T) {
	setupSessionTest(t)
	Config = &config.Config{}
	u := &model.User{Username: "mario", Email: "Mario.Rossi@example.com", Status: model.COMMON_STATUS_ENABLE}
	DB.Create(u)
	if err := AllService.UserService.UpdatePassword(u, "nuova-password"); err != nil {
		t.Fatal(err)
	}
	us := AllService.UserService
	if us.InfoByUsernamePassword("mario", "nuova-password").Id != u.Id {
		t.Fatal("username login failed")
	}
	if us.InfoByUsernamePassword(" mario.rossi@example.com", "nuova-password").Id != u.Id {
		t.Fatal("email login failed")
	}
	if us.InfoByUsernamePassword("mario.rossi@example.com", "sbagliata").Id != 0 {
		t.Fatal("wrong password accepted")
	}
	// SSO account without a password, and an email shared by two accounts
	DB.Create(&model.User{Username: "sso@example.com", Email: "sso@example.com"})
	if us.InfoByUsernamePassword("sso@example.com", "").Id != 0 {
		t.Fatal("empty password accepted")
	}
	DB.Create(&model.User{Username: "mario2", Email: "mario.rossi@example.com"})
	if us.InfoByUsernamePassword("mario.rossi@example.com", "nuova-password").Id != 0 {
		t.Fatal("ambiguous email accepted")
	}
}
