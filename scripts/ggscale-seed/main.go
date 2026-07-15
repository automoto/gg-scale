// Command ggscale-seed populates the database with realistic demo data.
//
// Usage:
//
//	go run ./cmd/ggscale-seed
//
// The script reads DATABASE_URL from the environment (default matches the
// docker-compose dev stack). It is idempotent-ish: if tenants already exist
// it warns and exits unless -force is passed.
package main

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	bcryptCost        = 12
	dashboardPassword = "Password123!"
	playerPassword    = "PlayerPass123!"
)

// ---------------------------------------------------------------------------
// Data pools
// ---------------------------------------------------------------------------

var (
	studioNames = []string{
		"Nebula Interactive", "Ironclad Studios", "PixelForge Games",
		"Voidwalkers LLC", "Quantum Forge", "Stormbreak Entertainment",
	}

	gameNames = [][]string{
		{"Starfall Arena", "Nebula Racer", "Cosmic Defenders"},
		{"Tank Battalion", "Siege Commander", "Iron Frontier"},
		{"Pixel Dungeons", "Craftopia", "8-Bit Heroes"},
		{"Void Hunter", "Abyssal Depths", "Star Drifter"},
		{"Quantum Rift", "Particle Wars", "Entangled"},
		{"Tempest Rising", "Stormbreak: Legends", "Cyclone Racing"},
	}

	fleetNames = []string{
		"eu-west-docker", "us-east-docker", "asia-docker",
		"agones-main", "agones-staging", "docker-dev",
	}

	regions     = []string{"eu-west", "us-east", "us-west", "asia-east", "asia-southeast"}
	gameModes   = []string{"ranked", "casual", "deathmatch", "capture_the_flag", "battle_royale"}
	statuses    = []string{"pending", "allocating", "ready", "allocated", "draining", "shutdown", "failed"}
	ticketStati = []string{"queued", "queued", "matched", "matched", "cancelled", "failed"}

	playerNames = []string{
		"ShadowHunter", "PixelNinja", "NovaStar", "CyberWolf", "IronMantis",
		"NeonPhoenix", "VoidRider", "StormBreaker", "QuantumLeap", "BlazeFox",
		"FrostByte", "ThunderClap", "StarDust", "DarkMatter", "SolarFlare",
		"LunarEclipse", "CosmicRay", "HyperDrive", "NebulaWraith", "GalaxyDrifter",
		"AstroNaut", "MeteorStrike", "CometTail", "OrbitRanger", "ZeroGravity",
		"PhotonBlade", "IonStorm", "PlasmaPulse", "WarpSpeed", "Singularity",
		"EventHorizon", "PulsarBeam", "QuasarBurst", "SuperNova", "BlackHole",
		"WormHole", "DarkEnergy", "LightYear", "RedShift", "BlueGiant",
		"WhiteDwarf", "StarCluster", "MilkyWay", "Andromeda", "Triangulum",
		"Centauri", "Sirius", "Rigel", "Betelgeuse", "Vega",
		"Altair", "Deneb", "Antares", "Arcturus", "Pollux",
		"Castor", "Regulus", "Spica", "Aldebaran", "Procyon",
	}

	firstNames = []string{
		"Alex", "Jordan", "Taylor", "Morgan", "Casey", "Riley", "Quinn",
		"Avery", "Cameron", "Drew", "Blake", "Parker", "Reese", "Hayden",
		"Sage", "Kai", "Ellis", "Phoenix", "Remy", "Shawn",
	}

	lastNames = []string{
		"Chen", "Patel", "Rodriguez", "Kim", "Singh", "O'Brien",
		"Nakamura", "Ivanov", "Silva", "Müller", "Andersson", "Popov",
		"Wang", "Gupta", "Kowalski", "Rossi", "Jensen", "Dubois",
		"Santos", "Olsen",
	}

	emailDomains = []string{
		"gmail.com", "outlook.com", "proton.me", "fastmail.com", "icloud.com",
	}

	leaderboardNames = []string{
		"All-Time Score", "Weekly Kills", "Speedrun Times",
		"Win Streak", "Headshots", "Capture Points",
	}

	auditActions = []string{
		"dashboard.login", "dashboard.tenant.created", "api_key.created",
		"fleet.created", "player.invited", "leaderboard.score_submitted",
	}
)

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	force := flag.Bool("force", false, "truncate existing demo data and re-seed")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://ggscale:ggscale@localhost:5432/ggscale?sslmode=disable"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}

	if !*force {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&n); err != nil {
			log.Fatalf("check tenants: %v", err)
		}
		if n > 0 {
			log.Printf("found %d existing tenant(s); use -force to truncate and re-seed", n)
			os.Exit(0)
		}
	}

	if *force {
		log.Println("truncating existing data...")
		if err := truncateAll(ctx, pool); err != nil {
			log.Fatalf("truncate: %v", err)
		}
	}

	log.Println("seeding demo data...")
	if err := seed(ctx, pool); err != nil {
		log.Fatalf("seed: %v", err)
	}
	log.Println("done")
}

// ---------------------------------------------------------------------------
// Truncate
// ---------------------------------------------------------------------------

func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	tables := []string{
		"usage_samples", "fleet_allocation_events", "game_server_allocations",
		"matchmaker_matches", "matchmaking_tickets", "fleets",
		"leaderboard_entries", "leaderboards", "friend_edges",
		"storage_objects", "sessions", "player_account_sessions",
		"player_account_totp_backup_codes", "player_account_totp",
		"player_account_trusted_devices", "tenant_player_bans",
		"project_players", "player_accounts", "audit_log", "api_keys",
		"projects", "tenants", "control_panel_invitations",
		"control_panel_sessions", "control_panel_memberships",
		"control_panel_user_totp_backup_codes", "control_panel_user_totp",
		"control_panel_trusted_devices", "control_panel_users",
		"platform_audit_log", "feature_grants", "rate_limit_overrides",
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	for _, t := range tables {
		if _, err := tx.Exec(ctx, fmt.Sprintf("TRUNCATE TABLE %s CASCADE", t)); err != nil {
			return fmt.Errorf("truncate %s: %w", t, err)
		}
	}
	// casbin_rule holds both static 'p' policy (seeded by migrations) and 'g'
	// grouping rows (subject→role). Truncating it would wipe the policy, so clear
	// only the grouping rows that reference the now-truncated subjects.
	if _, err := tx.Exec(ctx, `DELETE FROM casbin_rule WHERE ptype = 'g'`); err != nil {
		return fmt.Errorf("clear casbin grouping rows: %w", err)
	}
	committed = true
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Seed orchestration
// ---------------------------------------------------------------------------

type tenantSeed struct {
	id       int64
	name     string
	tier     int
	projects []projectSeed
}

type projectSeed struct {
	id      int64
	name    string
	players []playerSeed
}

type playerSeed struct {
	id         int64
	externalID string
	email      *string
	accountID  *string
}

func seed(ctx context.Context, pool *pgxpool.Pool) error {
	s := &seeder{
		pool: pool,
		rng:  rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(time.Now().UnixNano()+1))),
	}

	// 1. Platform admin
	adminID, err := s.createDashboardUser(ctx, "admin@demo.ggscale", dashboardPassword, true)
	if err != nil {
		return fmt.Errorf("create admin: %w", err)
	}
	log.Printf("created platform admin id=%d email=admin@demo.ggscale password=%s", adminID, dashboardPassword)
	if err := s.grantGroupingRow(ctx, cpUserSubject(adminID), roleGroupPlatformAdmin, "*"); err != nil {
		return fmt.Errorf("grant platform admin role: %w", err)
	}

	// 2. Tenants + first project via dashboard_create_tenant
	var tenants []tenantSeed
	for i, name := range studioNames[:4] {
		tier := []int{0, 1, 2, 1}[i]
		projectName := gameNames[i][0]
		keyHash := randomHash()

		var tid, pid, bootstrapKeyID int64
		err := pool.QueryRow(ctx,
			`SELECT tenant_id, project_id, api_key_id FROM control_panel_create_tenant($1, $2, $3, $4, $5)`,
			adminID, name, projectName, keyHash, "bootstrap-key",
		).Scan(&tid, &pid, &bootstrapKeyID)
		if err != nil {
			return fmt.Errorf("create tenant %q: %w", name, err)
		}

		// The function makes the actor an owner and mints a secret bootstrap key;
		// mirror the Casbin grouping the control panel would have written.
		if err := s.grantGroupingRow(ctx, cpUserSubject(adminID), roleGroupTenantOwner, tenantDomain(tid)); err != nil {
			return fmt.Errorf("grant tenant owner role: %w", err)
		}
		if err := s.grantGroupingRow(ctx, apiKeySubject(bootstrapKeyID), roleGroupAPIServer, tenantDomain(tid)); err != nil {
			return fmt.Errorf("grant bootstrap key role: %w", err)
		}

		if _, err := pool.Exec(ctx, `UPDATE tenants SET tier = $1 WHERE id = $2`, tier, tid); err != nil {
			return err
		}

		t := tenantSeed{id: tid, name: name, tier: tier, projects: []projectSeed{{id: pid, name: projectName}}}
		tenants = append(tenants, t)
		log.Printf("created tenant id=%d name=%q tier=%d project=%q", tid, name, tier, projectName)
	}

	// 3. Extra projects per tenant
	for i := range tenants {
		for j := 1; j < len(gameNames[i]) && j < 3; j++ {
			var pid int64
			err := s.withTenant(ctx, tenants[i].id, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`INSERT INTO projects (tenant_id, name) VALUES ($1, $2) RETURNING id`,
					tenants[i].id, gameNames[i][j],
				).Scan(&pid)
			})
			if err != nil {
				return fmt.Errorf("create project: %w", err)
			}
			tenants[i].projects = append(tenants[i].projects, projectSeed{id: pid, name: gameNames[i][j]})
			log.Printf("  extra project id=%d name=%q", pid, gameNames[i][j])
		}
	}

	// 4. Dashboard users (one owner per tenant)
	for i, t := range tenants {
		email := fmt.Sprintf("owner-%d@demo.ggscale", i+1)
		uid, err := s.createDashboardUser(ctx, email, dashboardPassword, false)
		if err != nil {
			return err
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role) VALUES ($1, $2, 'owner')`,
			uid, t.id,
		)
		if err != nil {
			return err
		}
		if err := s.grantGroupingRow(ctx, cpUserSubject(uid), roleGroupTenantOwner, tenantDomain(t.id)); err != nil {
			return fmt.Errorf("grant tenant owner role: %w", err)
		}
		log.Printf("created tenant owner id=%d email=%s tenant=%q", uid, email, t.name)
	}

	// 5. API keys (extra publishable keys)
	for _, t := range tenants {
		for _, p := range t.projects {
			keyHash := randomHash()
			var keyID int64
			err := pool.QueryRow(ctx,
				`INSERT INTO api_keys (tenant_id, project_id, key_hash, label, scopes, key_type)
				 VALUES ($1, $2, $3, $4, $5, 'publishable') RETURNING id`,
				t.id, p.id, keyHash, fmt.Sprintf("sdk-%s", p.name), []string{"read"},
			).Scan(&keyID)
			if err != nil {
				return err
			}
			if err := s.grantGroupingRow(ctx, apiKeySubject(keyID), roleGroupAPIClient, tenantDomain(t.id)); err != nil {
				return fmt.Errorf("grant api key role: %w", err)
			}
		}
	}

	// 6. Players, leaderboards, entries, friends, storage, fleets, allocations, tickets, usage
	for _, t := range tenants {
		for _, p := range t.projects {
			if err := s.seedProject(ctx, t.id, p); err != nil {
				return fmt.Errorf("seed project %d: %w", p.id, err)
			}
		}
		if err := s.seedAuditLogs(ctx, t.id); err != nil {
			return fmt.Errorf("seed audit: %w", err)
		}
	}

	// 7. Platform audit log
	_, err = pool.Exec(ctx,
		`INSERT INTO platform_audit_log (actor_user_id, action, target, payload)
		 VALUES ($1, 'seed.completed', 'database', '{}')`, adminID)
	if err != nil {
		return err
	}

	printSummary(ctx, pool)
	return nil
}

// ---------------------------------------------------------------------------
// Seeder helpers
// ---------------------------------------------------------------------------

type seeder struct {
	pool *pgxpool.Pool
	rng  *rand.Rand
}

// Casbin grouping-row helpers. The control panel writes these subject→role rows
// in-transaction when it provisions tenants, memberships, and API keys. The seed
// inserts those records directly, so it must write the matching grouping rows or
// authorization checks return 403. Formats mirror internal/rbac.
const (
	roleGroupPlatformAdmin = "role:platform_admin"
	roleGroupTenantOwner   = "role:tenant_owner"
	roleGroupAPIServer     = "role:api_server"
	roleGroupAPIClient     = "role:api_client"
)

func cpUserSubject(id int64) string { return "control_panel:user:" + strconv.FormatInt(id, 10) }
func apiKeySubject(id int64) string { return "api_key:" + strconv.FormatInt(id, 10) }
func tenantDomain(id int64) string  { return "tenant:" + strconv.FormatInt(id, 10) }

// grantGroupingRow writes a Casbin grouping row assigning subject the given role
// within domain (use "*" for the global platform-admin domain).
func (s *seeder) grantGroupingRow(ctx context.Context, subject, role, domain string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ('g', $1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		subject, role, domain,
	)
	return err
}

func (s *seeder) withTenant(ctx context.Context, tenantID int64, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, strconv.FormatInt(tenantID, 10)); err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

func (s *seeder) createDashboardUser(ctx context.Context, email, password string, isAdmin bool) (int64, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO control_panel_users (email, password_hash, is_platform_admin)
		 VALUES ($1, $2, $3) RETURNING id`,
		email, hash, isAdmin,
	).Scan(&id)
	return id, err
}

func (s *seeder) seedProject(ctx context.Context, tenantID int64, p projectSeed) error {
	// Players
	playerCount := 20 + s.rng.IntN(35) // 20-54 players
	players := make([]playerSeed, 0, playerCount)
	for i := 0; i < playerCount; i++ {
		var id int64
		var email *string
		var accountID *string
		externalID := fmt.Sprintf("player-%s-%d", randomHex(4), i)
		if s.rng.IntN(3) != 0 { // 2/3 have emails
			addr := fmt.Sprintf("%s.%s.%d@%s",
				strings.ToLower(firstNames[s.rng.IntN(len(firstNames))]),
				strings.ToLower(lastNames[s.rng.IntN(len(lastNames))]),
				s.rng.IntN(100000),
				emailDomains[s.rng.IntN(len(emailDomains))],
			)
			email = &addr
		}

		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			if email != nil {
				passHash, _ := bcrypt.GenerateFromPassword([]byte(playerPassword), bcryptCost)
				var acc string
				if err := tx.QueryRow(ctx,
					`INSERT INTO player_accounts (email, password_hash, display_name, email_verified_at)
					 VALUES ($1, $2, $3, now()) RETURNING id::text`,
					*email, passHash, playerNames[s.rng.IntN(len(playerNames))],
				).Scan(&acc); err != nil {
					return err
				}
				accountID = &acc
				return tx.QueryRow(ctx,
					`INSERT INTO project_players (tenant_id, project_id, external_id, email, password_hash, email_verified_at, player_account_id)
					 VALUES ($1, $2, $3, $4, $5, now(), $6) RETURNING id`,
					tenantID, p.id, externalID, *email, passHash, acc,
				).Scan(&id)
			}
			return tx.QueryRow(ctx,
				`INSERT INTO project_players (tenant_id, project_id, external_id)
				 VALUES ($1, $2, $3) RETURNING id`,
				tenantID, p.id, externalID,
			).Scan(&id)
		})
		if err != nil {
			return fmt.Errorf("player %d: %w", i, err)
		}
		players = append(players, playerSeed{id: id, externalID: externalID, email: email, accountID: accountID})
	}

	// Leaderboards
	lbCount := 2 + s.rng.IntN(2) // 2-3
	for i := 0; i < lbCount; i++ {
		name := leaderboardNames[(i+int(p.id))%len(leaderboardNames)]
		sortOrder := "desc"
		if strings.Contains(name, "Speedrun") || strings.Contains(name, "Time") {
			sortOrder = "asc"
		}
		var lbID int64
		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`INSERT INTO leaderboards (tenant_id, project_id, name, sort_order)
				 VALUES ($1, $2, $3, $4) RETURNING id`,
				tenantID, p.id, name, sortOrder,
			).Scan(&lbID)
		})
		if err != nil {
			return fmt.Errorf("leaderboard: %w", err)
		}

		// Scores
		scoreCount := 10 + s.rng.IntN(30)
		for j := 0; j < scoreCount && j < len(players); j++ {
			player := players[s.rng.IntN(len(players))]
			score := int64(s.rng.IntN(100000))
			if sortOrder == "asc" {
				score = int64(s.rng.IntN(3600)) // seconds
			}
			recorded := time.Now().Add(-time.Duration(s.rng.IntN(30*24)) * time.Hour)
			err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, score, recorded_at)
					 VALUES ($1, $2, $3, $4, $5)`,
					tenantID, lbID, player.id, score, recorded,
				)
				return err
			})
			if err != nil {
				return fmt.Errorf("score: %w", err)
			}
		}
	}

	// Friend edges
	accountPlayers := make([]playerSeed, 0, len(players))
	for _, player := range players {
		if player.accountID != nil {
			accountPlayers = append(accountPlayers, player)
		}
	}
	friendCount := 5 + s.rng.IntN(10)
	for i := 0; i < friendCount && len(accountPlayers) > 3; i++ {
		a := accountPlayers[s.rng.IntN(len(accountPlayers))]
		b := accountPlayers[s.rng.IntN(len(accountPlayers))]
		if a.id == b.id {
			continue
		}
		status := []string{"pending", "accepted", "accepted", "accepted", "blocked"}[s.rng.IntN(5)]
		created := time.Now().Add(-time.Duration(s.rng.IntN(30*24)) * time.Hour)
		updated := created.Add(time.Duration(s.rng.IntN(24)) * time.Hour)
		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO friend_edges (from_account_id, to_account_id, status, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5)
				 ON CONFLICT (from_account_id, to_account_id) DO NOTHING`,
				*a.accountID, *b.accountID, status, created, updated,
			)
			return err
		})
		if err != nil {
			return fmt.Errorf("friend: %w", err)
		}
		// Symmetric accepted
		if status == "accepted" {
			_ = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO friend_edges (from_account_id, to_account_id, status, created_at, updated_at)
					 VALUES ($1, $2, $3, $4, $5)
					 ON CONFLICT (from_account_id, to_account_id) DO NOTHING`,
					*b.accountID, *a.accountID, status, created, updated,
				)
				return err
			})
		}
	}

	// Storage objects
	for i := 0; i < 5+s.rng.IntN(5) && i < len(players); i++ {
		player := players[s.rng.IntN(len(players))]
		key := []string{"settings", "profile", "inventory", "achievements", "stats"}[s.rng.IntN(5)]
		value := fmt.Sprintf(`{"theme":"%s","volume":%d,"notifications":%t}`,
			[]string{"dark", "light", "auto"}[s.rng.IntN(3)],
			s.rng.IntN(100),
			s.rng.IntN(2) == 0,
		)
		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO storage_objects (tenant_id, project_id, owner_user_id, key, value)
				 VALUES ($1, $2, $3, $4, $5)
				 ON CONFLICT (tenant_id, project_id, owner_user_id, key) WHERE deleted_at IS NULL DO NOTHING`,
				tenantID, p.id, player.id, key, value,
			)
			return err
		})
		if err != nil {
			return fmt.Errorf("storage: %w", err)
		}
	}

	// Fleets
	fleetCount := 1 + s.rng.IntN(2)
	fleetIDs := make([]int64, 0, fleetCount)
	for i := 0; i < fleetCount; i++ {
		name := fleetNames[(int(p.id)+i)%len(fleetNames)]
		backend := "docker"
		if s.rng.IntN(3) == 0 {
			backend = "agones"
		}
		config := `{"image":"traefik/whoami:latest","port":"8080","memory":"536870912"}`
		if backend == "agones" {
			config = `{"fleet_name":"simple-game-server","namespace":"default"}`
		}
		var fid int64
		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
				 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				tenantID, p.id, name, backend, config,
			).Scan(&fid)
		})
		if err != nil {
			return fmt.Errorf("fleet: %w", err)
		}
		fleetIDs = append(fleetIDs, fid)
	}

	// Allocations
	for i := 0; i < 5+s.rng.IntN(8); i++ {
		status := statuses[s.rng.IntN(len(statuses))]
		region := regions[s.rng.IntN(len(regions))]
		address := ""
		if status == "ready" || status == "allocated" || status == "draining" {
			address = fmt.Sprintf("127.0.0.1:%d", 30000+s.rng.IntN(10000))
		}
		fleetID := fleetIDs[s.rng.IntN(len(fleetIDs))]
		metadata := `{}`
		requested := time.Now().Add(-time.Duration(s.rng.IntN(72)) * time.Hour)
		readyAt := requested.Add(time.Duration(s.rng.IntN(60)) * time.Second)
		releasedAt := readyAt.Add(time.Duration(s.rng.IntN(48)) * time.Hour)
		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO game_server_allocations
				 (tenant_id, project_id, backend, backend_ref, region, address, status, metadata, requested_at, ready_at, released_at, fleet_id)
				 VALUES ($1, $2, 'docker', $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				tenantID, p.id,
				randomHex(8),
				region, address, status, metadata,
				requested, readyAt, releasedAt, fleetID,
			)
			return err
		})
		if err != nil {
			return fmt.Errorf("allocation: %w", err)
		}
	}

	// Matchmaking tickets
	for i := 0; i < 10+s.rng.IntN(15); i++ {
		status := ticketStati[s.rng.IntN(len(ticketStati))]
		player := players[s.rng.IntN(len(players))]
		region := regions[s.rng.IntN(len(regions))]
		mode := gameModes[s.rng.IntN(len(gameModes))]
		fleetID := fleetIDs[s.rng.IntN(len(fleetIDs))]
		attributes := `{"rank":1500,"latency_ms":24}`
		matchAddress := ""
		if status == "matched" {
			matchAddress = fmt.Sprintf("127.0.0.1:%d", 30000+s.rng.IntN(10000))
		}
		created := time.Now().Add(-time.Duration(s.rng.IntN(24)) * time.Hour)
		matched := created.Add(time.Duration(s.rng.IntN(60)) * time.Second)
		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO matchmaking_tickets
				 (tenant_id, project_id, fleet_id, player_id, region, game_mode, attributes, status, match_address, created_at, matched_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				tenantID, p.id, fleetID, player.id, region, mode, attributes, status, matchAddress, created, matched,
			)
			return err
		})
		if err != nil {
			return fmt.Errorf("ticket: %w", err)
		}
	}

	// Usage samples (current month only, every 3 hours)
	// The table is range-partitioned monthly; avoid inserting outside existing partitions.
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	hours := int(now.Sub(base).Hours())
	for h := 0; h <= hours; h += 3 {
		ts := base.Add(time.Duration(h) * time.Hour)
		ccu := int32(10 + s.rng.IntN(500))
		requests := int64(ccu * int32(10+s.rng.IntN(100)))
		egress := requests * int64(100+s.rng.IntN(9000))
		_, err := s.pool.Exec(ctx,
			`INSERT INTO usage_samples (tenant_id, project_id, ts, ccu, requests, bytes_egress)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, p.id, ts, ccu, requests, egress,
		)
		if err != nil {
			return fmt.Errorf("usage: %w", err)
		}
	}

	return nil
}

func (s *seeder) seedAuditLogs(ctx context.Context, tenantID int64) error {
	for i := 0; i < 5+s.rng.IntN(10); i++ {
		action := auditActions[s.rng.IntN(len(auditActions))]
		target := fmt.Sprintf("tenant:%d", tenantID)
		payload := fmt.Sprintf(`{"detail":"demo action %d"}`, i)
		err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO audit_log (tenant_id, action, target, payload)
				 VALUES ($1, $2, $3, $4)`,
				tenantID, action, target, payload,
			)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

func printSummary(ctx context.Context, pool *pgxpool.Pool) {
	var counts struct {
		Tenants  int64
		Projects int64
		Players  int64
		Scores   int64
		Friends  int64
		Fleets   int64
		Allocs   int64
		Tickets  int64
		Usage    int64
		Storage  int64
		Audit    int64
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&counts.Tenants)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM projects`).Scan(&counts.Projects)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM project_players`).Scan(&counts.Players)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM leaderboard_entries`).Scan(&counts.Scores)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM friend_edges`).Scan(&counts.Friends)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM fleets`).Scan(&counts.Fleets)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM game_server_allocations`).Scan(&counts.Allocs)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM matchmaking_tickets`).Scan(&counts.Tickets)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM usage_samples`).Scan(&counts.Usage)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM storage_objects`).Scan(&counts.Storage)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&counts.Audit)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                   DEMO SEED COMPLETE                         ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Tenants:    %-47d ║\n", counts.Tenants)
	fmt.Printf("║  Projects:   %-47d ║\n", counts.Projects)
	fmt.Printf("║  Players:    %-47d ║\n", counts.Players)
	fmt.Printf("║  Scores:     %-47d ║\n", counts.Scores)
	fmt.Printf("║  Friends:    %-47d ║\n", counts.Friends)
	fmt.Printf("║  Fleets:     %-47d ║\n", counts.Fleets)
	fmt.Printf("║  Allocs:     %-47d ║\n", counts.Allocs)
	fmt.Printf("║  Tickets:    %-47d ║\n", counts.Tickets)
	fmt.Printf("║  Usage:      %-47d ║\n", counts.Usage)
	fmt.Printf("║  Storage:    %-47d ║\n", counts.Storage)
	fmt.Printf("║  Audit:      %-47d ║\n", counts.Audit)
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Admin:      admin@demo.ggscale / %s            ║\n", dashboardPassword)
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randomHash() []byte {
	h := sha256.Sum256([]byte(randomHex(32)))
	return h[:]
}
