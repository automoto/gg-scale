package party

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Code is returned once. Only its SHA-256 hash is stored.
type Code struct {
	ID           int64     `json:"id"`
	Code         string    `json:"code"`
	ExpiresAt    time.Time `json:"expires_at"`
	PartyVersion int64     `json:"party_version"`
}

// Invite is a pending invitation from a leader to a project friend.
type Invite struct {
	ID           int64     `json:"id"`
	PartyID      int64     `json:"party_id"`
	PartyVersion int64     `json:"party_version"`
	TargetID     int64     `json:"target_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// CreateCode revokes previous codes before minting a five-minute code.
func (s *Store) CreateCode(ctx context.Context, project, id, player, version int64, uses int) (*Code, error) {
	if uses < 1 || uses > 7 {
		return nil, ErrInvite
	}
	code, err := newCode()
	if err != nil {
		return nil, err
	}
	out := &Code{Code: code}
	_, err = s.mutate(ctx, project, id, player, version, true, func(tx pgx.Tx, p *Party) error {
		if p.State != "idle" {
			return ErrBusy
		}
		if _, err := tx.Exec(ctx, `UPDATE party_invite_codes SET revoked_at=now() WHERE party_id=$1 AND revoked_at IS NULL`, id); err != nil {
			return err
		}
		p.Version++
		out.PartyVersion = p.Version
		return tx.QueryRow(ctx, `INSERT INTO party_invite_codes(tenant_id,project_id,party_id,code_hash,max_uses) VALUES(current_setting('app.tenant_id')::bigint,$1,$2,$3,$4) RETURNING id,expires_at`, project, id, codeHash(code), uses).Scan(&out.ID, &out.ExpiresAt)
	})
	return out, err
}

// RevokeCode invalidates a code under the party lock.
func (s *Store) RevokeCode(ctx context.Context, project, id, player, version, codeID int64) (*Party, error) {
	return s.mutate(ctx, project, id, player, version, true, func(tx pgx.Tx, p *Party) error {
		result, err := tx.Exec(ctx, `UPDATE party_invite_codes SET revoked_at=now() WHERE party_id=$1 AND id=$2`, id, codeID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrInvite
		}
		p.Version++
		return nil
	})
}

func join(ctx context.Context, tx pgx.Tx, p *Party, player int64) error {
	if p.State != "idle" {
		return ErrBusy
	}
	if len(p.Members) >= p.MaxMembers {
		return ErrFull
	}
	if err := checkPlayer(ctx, tx, p.ProjectID, player); err != nil {
		return err
	}
	now := time.Now().UTC()
	p.Members = append(p.Members, Member{PlayerID: player, JoinedAt: now, LastSeenAt: now, DisconnectDeadline: now.Add(30 * time.Second)})
	p.rosterChanged()
	return save(ctx, tx, p)
}

// JoinCode checks durable player and IP failure windows before redemption.
func (s *Store) JoinCode(ctx context.Context, project, player, version int64, code, ip string) (*Party, error) {
	var out *Party
	var verdict error
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT party_code_ip_limit($1,false)`, ip).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			verdict = ErrCooldown
			return nil
		}
		subjects := []string{fmt.Sprintf("player:%d", player)}
		limits := []int{10}
		for _, subject := range subjects {
			if _, err := tx.Exec(ctx, `INSERT INTO party_code_attempts(tenant_id,project_id,subject) VALUES(current_setting('app.tenant_id')::bigint,$1,$2) ON CONFLICT DO NOTHING`, project, subject); err != nil {
				return err
			}
			var blocked bool
			err := tx.QueryRow(ctx, `SELECT COALESCE(blocked_until>now(),false) FROM party_code_attempts WHERE project_id=$1 AND subject=$2 FOR UPDATE`, project, subject).Scan(&blocked)
			if err != nil {
				return err
			}
			if blocked {
				verdict = ErrCooldown
				return nil
			}
		}
		var id, codeID int64
		err := tx.QueryRow(ctx, `SELECT party_id,id FROM party_invite_codes WHERE project_id=$1 AND code_hash=$2 AND revoked_at IS NULL AND expires_at>now() AND uses<max_uses`, project, codeHash(code)).Scan(&id, &codeID)
		if errors.Is(err, pgx.ErrNoRows) {
			if err = tx.QueryRow(ctx, `SELECT party_code_ip_limit($1,true)`, ip).Scan(&blocked); err != nil {
				return err
			}
			for i, subject := range subjects {
				_, err = tx.Exec(ctx, `UPDATE party_code_attempts SET failures=CASE WHEN window_start<=now()-interval '15 minutes' THEN 1 ELSE failures+1 END,window_start=CASE WHEN window_start<=now()-interval '15 minutes' THEN now() ELSE window_start END,blocked_until=CASE WHEN window_start>now()-interval '15 minutes' AND failures+1 >= $3 THEN now()+interval '15 minutes' ELSE NULL END WHERE project_id=$1 AND subject=$2`, project, subject, limits[i])
				if err != nil {
					return err
				}
			}
			verdict = ErrInvite
			return nil
		}
		if err != nil {
			return err
		}
		out, err = load(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if out.Version != version {
			return ErrStale
		}
		result, err := tx.Exec(ctx, `UPDATE party_invite_codes SET uses=uses+1 WHERE id=$1 AND revoked_at IS NULL AND expires_at>now() AND uses<max_uses`, codeID)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrInvite
		}
		return join(ctx, tx, out, player)
	})
	if err != nil {
		return nil, err
	}
	return out, verdict
}

// InviteFriend sends an invitation only to an accepted, unblocked friend in this project.
func (s *Store) InviteFriend(ctx context.Context, project, id, player, version, target int64) (*Invite, error) {
	out := &Invite{PartyID: id, TargetID: target}
	_, err := s.mutate(ctx, project, id, player, version, true, func(tx pgx.Tx, p *Party) error {
		if p.State != "idle" {
			return ErrBusy
		}
		var friends bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_players a JOIN project_players b ON a.project_id=b.project_id JOIN friend_edges f ON ((f.from_account_id=a.player_account_id AND f.to_account_id=b.player_account_id) OR (f.to_account_id=a.player_account_id AND f.from_account_id=b.player_account_id)) WHERE a.id=$1 AND b.id=$2 AND a.project_id=$3 AND f.status='accepted' AND NOT EXISTS(SELECT 1 FROM friend_edges x WHERE x.status='blocked' AND ((x.from_account_id=a.player_account_id AND x.to_account_id=b.player_account_id) OR (x.to_account_id=a.player_account_id AND x.from_account_id=b.player_account_id))))`, player, target, project).Scan(&friends)
		if err != nil {
			return err
		}
		if !friends {
			return ErrInvite
		}
		if _, err = tx.Exec(ctx, `UPDATE party_invites SET status='expired' WHERE party_id=$1 AND target_id=$2 AND status='pending' AND expires_at<=now()`, id, target); err != nil {
			return err
		}
		p.Version++
		out.PartyVersion = p.Version
		return tx.QueryRow(ctx, `INSERT INTO party_invites(tenant_id,project_id,party_id,target_id) VALUES(current_setting('app.tenant_id')::bigint,$1,$2,$3) ON CONFLICT(party_id,target_id) WHERE status='pending' DO UPDATE SET expires_at=party_invites.expires_at RETURNING id,expires_at`, project, id, target).Scan(&out.ID, &out.ExpiresAt)
	})
	return out, err
}

// Invites lists only the authenticated player's live invitations.
func (s *Store) Invites(ctx context.Context, project, player int64) ([]Invite, error) {
	out := []Invite{}
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT i.id,i.party_id,p.version,i.target_id,i.expires_at FROM party_invites i JOIN parties p ON p.id=i.party_id WHERE i.project_id=$1 AND i.target_id=$2 AND i.status='pending' AND i.expires_at>now() AND p.state='idle' ORDER BY i.id LIMIT 100`, project, player)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var i Invite
			if err := rows.Scan(&i.ID, &i.PartyID, &i.PartyVersion, &i.TargetID, &i.ExpiresAt); err != nil {
				return err
			}
			out = append(out, i)
		}
		return rows.Err()
	})
	return out, err
}

// ResolveInvite accepts, declines, or revokes an invitation atomically.
func (s *Store) ResolveInvite(ctx context.Context, project, id, player, version int64, accept bool) (*Party, error) {
	var out *Party
	err := s.pool.Q(ctx, func(tx pgx.Tx) error {
		var partyID, target int64
		err := tx.QueryRow(ctx, `SELECT party_id,target_id FROM party_invites WHERE project_id=$1 AND id=$2 AND status='pending' AND expires_at>now()`, project, id).Scan(&partyID, &target)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvite
		}
		if err != nil {
			return err
		}
		out, err = load(ctx, tx, project, partyID)
		if err != nil {
			return err
		}
		if target != player && (accept || out.LeaderID != player) {
			return ErrNotLeader
		}
		if out.Version != version {
			return ErrStale
		}
		status := "declined"
		if target != player {
			status = "revoked"
		}
		if accept {
			status = "accepted"
		}
		result, err := tx.Exec(ctx, `UPDATE party_invites SET status=$2 WHERE id=$1 AND status='pending' AND expires_at>now()`, id, status)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrInvite
		}
		if accept {
			return join(ctx, tx, out, player)
		}
		out.Version++
		return save(ctx, tx, out)
	})
	return out, err
}
