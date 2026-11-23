package service

// DareMoto: plan enforcement for the rendezvous server (hbbs).
//
// hbbs calls Authorize for every connection request (PunchHoleRequest / RequestRelay) with
// the controlling user's access token and the target device id. A connection opens a lease:
//   - pending until the controlled device reports it (audit with the controller id), max 90 s
//   - then bound to that connection id, alive while the controlled device lists it in the
//     "conns" of its heartbeat (sent every few seconds while connections are open)
// Concurrent sessions = distinct target devices with a live lease. Leases live in memory:
// after an API restart running sessions are recounted as soon as they reconnect.

import (
	"fmt"
	"sync"
	"time"

	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm/clause"
)

const (
	leasePendingTTL   = 90 * time.Second
	heartbeatFreshTTL = 30 * time.Second
	leaseGCAge        = time.Hour
	accountURL        = "api.daremoto.net/account"
)

type sessionLease struct {
	userId   uint
	fromId   string // controller device id (from the token)
	targetId string
	created  time.Time
	connId   int64
	boundAt  time.Time
	closed   bool
}

type peerConns struct {
	conns map[int64]bool
	at    time.Time
}

type SessionService struct {
	mu     sync.Mutex
	leases []*sessionLease
	hb     map[string]*peerConns
}

var Sessions = &SessionService{hb: map[string]*peerConns{}}

type AuthorizeResult struct {
	Allow   bool   `json:"allow"`
	Message string `json:"message,omitempty"`
	UserId  uint   `json:"user_id,omitempty"`
	Plan    string `json:"plan,omitempty"`
}

func deny(msg string) AuthorizeResult { return AuthorizeResult{Allow: false, Message: msg} }

func (s *SessionService) live(l *sessionLease, now time.Time) bool {
	if l.closed {
		return false
	}
	if l.connId == 0 {
		return now.Sub(l.created) < leasePendingTTL
	}
	hb := s.hb[l.targetId]
	if hb == nil || !hb.at.After(l.boundAt) {
		// no heartbeat since the connection was reported yet
		return now.Sub(l.boundAt) < heartbeatFreshTTL
	}
	return now.Sub(hb.at) < heartbeatFreshTTL && hb.conns[l.connId]
}

func (s *SessionService) gc(now time.Time) {
	kept := s.leases[:0]
	for _, l := range s.leases {
		if s.live(l, now) || now.Sub(l.created) < leaseGCAge {
			kept = append(kept, l)
		}
	}
	s.leases = kept
	for id, hb := range s.hb {
		if now.Sub(hb.at) > leaseGCAge {
			delete(s.hb, id)
		}
	}
}

// activeTargets returns the target devices with a live lease for the user.
func (s *SessionService) activeTargets(userId uint, now time.Time) map[string]bool {
	out := map[string]bool{}
	for _, l := range s.leases {
		if l.userId == userId && s.live(l, now) {
			out[l.targetId] = true
		}
	}
	return out
}

// ActiveSessions is the number of devices the user is connected to right now.
func (s *SessionService) ActiveSessions(userId uint) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.activeTargets(userId, time.Now()))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

// Authorize decides whether the owner of token may connect to targetId now.
func (s *SessionService) Authorize(token, targetId string) AuthorizeResult {
	if token == "" {
		return deny("Per collegarti a un altro dispositivo accedi al tuo account DareMoto (Impostazioni → Account). Il piano Gratuito non costa nulla.")
	}
	if targetId == "" {
		return deny("ID del dispositivo mancante.")
	}
	user, ut := AllService.UserService.InfoByAccessToken(token)
	if user.Id == 0 {
		return deny("L'accesso al tuo account DareMoto è scaduto: esci e accedi di nuovo da Impostazioni → Account.")
	}
	if !AllService.UserService.CheckUserEnable(user) {
		return deny("Il tuo account DareMoto è disattivato. Per informazioni scrivi a info@ianusproject.it.")
	}
	limits := Billing.LimitsFor(user.Id)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.gc(now)

	// machines: distinct devices in the last 30 days
	known := &model.AccountMachine{}
	DB.Where("user_id = ? AND peer_id = ?", user.Id, targetId).First(known)
	recent := known.Id != 0 && known.LastSeen >= now.Add(-MachinesWindow).Unix()
	if !recent {
		if used := Billing.MachinesUsed(user.Id); used >= int64(limits.Machines) {
			return deny(fmt.Sprintf("Hai raggiunto il limite del piano %s: %s negli ultimi 30 giorni. Per collegarti ad altri PC passa a un piano superiore su %s",
				limits.PlanName, plural(limits.Machines, "1 PC", "%d PC"), accountURL))
		}
	}

	// concurrent sessions: a new device needs a free slot, the same device does not
	active := s.activeTargets(user.Id, now)
	if !active[targetId] && len(active) >= limits.Sessions {
		return deny(fmt.Sprintf("Hai già %s, il massimo del piano %s. Chiudi una sessione oppure aggiungi sessioni su %s",
			plural(len(active), "una sessione aperta", "%d sessioni aperte"), limits.PlanName, accountURL))
	}

	DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "peer_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{"last_seen": now.Unix()}),
	}).Create(&model.AccountMachine{UserId: user.Id, PeerId: targetId, LastSeen: now.Unix()})

	s.leases = append(s.leases, &sessionLease{userId: user.Id, fromId: ut.DeviceId, targetId: targetId, created: now})
	return AuthorizeResult{Allow: true, UserId: user.Id, Plan: limits.Plan}
}

// OnAuditPeer: the controlled device (targetId) reports that controller fromId is on
// connection connId. Binds the most recent pending lease for that pair.
func (s *SessionService) OnAuditPeer(targetId string, connId int64, fromId string) {
	if targetId == "" || connId == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, l := range s.leases {
		if l.targetId == targetId && l.connId == connId && !l.closed {
			return // already bound (repeated audit)
		}
	}
	var best *sessionLease
	for _, l := range s.leases {
		if l.targetId != targetId || l.connId != 0 || l.closed || now.Sub(l.created) >= leasePendingTTL {
			continue
		}
		if fromId != "" && l.fromId != "" && l.fromId != fromId {
			continue
		}
		if best == nil || l.created.After(best.created) {
			best = l
		}
	}
	if best != nil {
		best.connId = connId
		best.boundAt = now
	}
}

// OnHeartbeat records the connections the controlled device has open right now.
func (s *SessionService) OnHeartbeat(targetId string, conns []int64) {
	if targetId == "" {
		return
	}
	m := make(map[int64]bool, len(conns))
	for _, c := range conns {
		m[c] = true
	}
	s.mu.Lock()
	s.hb[targetId] = &peerConns{conns: m, at: time.Now()}
	s.mu.Unlock()
}
