package rbac

const defaultPolicyCSV = `
p, role:platform_owner, *, *, *
p, role:platform_admin, *, tenant, read
p, role:platform_admin, *, dashboard_user, read
p, role:platform_admin, *, dashboard_user, disable
p, role:platform_admin, *, platform:plugins, read
p, role:platform_support, *, tenant, read
p, role:platform_support, *, dashboard_user, read

p, role:tenant_owner, *, tenant, manage
p, role:tenant_owner, *, project, manage
p, role:tenant_owner, *, api_key, manage
p, role:tenant_owner, *, team, manage
p, role:tenant_owner, *, audit, read

p, role:tenant_admin, *, project, manage
p, role:tenant_admin, *, project:*:players, manage
p, role:tenant_admin, *, api_key:publishable, manage
p, role:tenant_admin, *, audit, read

p, role:security_admin, *, api_key:secret, manage
p, role:security_admin, *, custom_token, manage
p, role:security_admin, *, audit, read
p, role:security_admin, *, feature_request, create

p, role:developer, *, project, read
p, role:developer, *, project:*:config, update
p, role:developer, *, project:*:players, read

p, role:support, *, project:*:players, read
p, role:support, *, project:*:players, invite
p, role:support, *, project:*:players, disable

p, role:analyst, *, project, read
p, role:analyst, *, project:*:players, read
p, role:analyst, *, project:*:allocation, read
p, role:analyst, *, project:*:matchmaker, read

p, role:fleet_operator, *, project:*:fleet, manage
p, role:fleet_operator, *, project:*:allocation, read
p, role:fleet_operator, *, project:*:allocation, allocate
p, role:fleet_operator, *, project:*:allocation, deallocate
p, role:fleet_operator, *, project:*:matchmaker, read

p, role:player_standard, *, profile, read
p, role:player_standard, *, profile, update
p, role:player_standard, *, storage, manage
p, role:player_standard, *, friends, manage
p, role:player_standard, *, leaderboard, read
p, role:player_standard, *, realtime, connect

p, role:player_verified, *, profile, read
p, role:player_verified, *, profile, update
p, role:player_verified, *, storage, manage
p, role:player_verified, *, friends, manage
p, role:player_verified, *, leaderboard, read
p, role:player_verified, *, realtime, connect

p, role:player_high_access, *, project:*:relay, issue_credentials
p, role:player_high_access, *, project:*:matchmaking:dedicated, create_ticket

p, role:api_client, *, auth, create
p, role:api_client, *, profile, read

p, role:api_server, *, end_user, verify
p, role:api_server, *, leaderboard, submit

p, role:api_fleet_runtime, *, end_user, verify
p, role:api_fleet_runtime, *, project:*:allocation, read
p, role:api_fleet_runtime, *, project:*:allocation, update
`
