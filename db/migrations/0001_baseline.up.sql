-- ggscale schema baseline.
--
-- Squashed from the original incremental migrations (pre-1.0, one-time reset).
-- This single file is the authoritative schema: tables, indexes, constraints,
-- functions, triggers, row-level-security policies, grants, partitions, the
-- River job-queue schema, and the static Casbin policy seed. It is generated
-- to be faithful to the fully-migrated schema and verified by a schema diff.
--
-- The ggscale_app role is cluster-level, so its creation is guarded; the app
-- connects as a login user that SET ROLEs to ggscale_app, and every object
-- grant below targets ggscale_app. Created first so the grants that follow
-- resolve.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ggscale_app') THEN
        CREATE ROLE ggscale_app NOLOGIN;
    END IF;
END$$;

--
-- PostgreSQL database dump
--


-- Dumped from database version 17.10 (Debian 17.10-1.pgdg13+1)
-- Dumped by pg_dump version 17.10 (Debian 17.10-1.pgdg13+1)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: citext; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;


--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;


--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;


--
-- Name: allocation_status; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.allocation_status AS ENUM (
    'pending',
    'allocating',
    'ready',
    'allocated',
    'draining',
    'shutdown',
    'failed'
);


--
-- Name: river_job_state; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.river_job_state AS ENUM (
    'available',
    'cancelled',
    'completed',
    'discarded',
    'pending',
    'retryable',
    'running',
    'scheduled'
);


--
-- Name: ticket_status; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.ticket_status AS ENUM (
    'queued',
    'matched',
    'cancelled',
    'failed'
);


--
-- Name: control_panel_create_tenant(bigint, text, text, bytea, text); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.control_panel_create_tenant(p_actor_user_id bigint, p_tenant_name text, p_project_name text, p_key_hash bytea, p_key_label text) RETURNS TABLE(tenant_id bigint, project_id bigint, api_key_id bigint, membership_id bigint)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
DECLARE
    v_label TEXT := nullif(trim(p_key_label), '');
BEGIN
    IF p_actor_user_id IS NULL OR p_actor_user_id <= 0 THEN
        RAISE EXCEPTION 'control panel actor user id is required' USING ERRCODE = '22023';
    END IF;
    IF nullif(trim(p_tenant_name), '') IS NULL THEN
        RAISE EXCEPTION 'tenant name is required' USING ERRCODE = '22023';
    END IF;
    IF nullif(trim(p_project_name), '') IS NULL THEN
        RAISE EXCEPTION 'project name is required' USING ERRCODE = '22023';
    END IF;
    IF p_key_hash IS NULL OR length(p_key_hash) = 0 THEN
        RAISE EXCEPTION 'api key hash is required' USING ERRCODE = '22023';
    END IF;

    PERFORM set_config('app.allow_tenant_bootstrap', '1', true);

    INSERT INTO tenants (name)
    VALUES (trim(p_tenant_name))
    RETURNING id INTO tenant_id;

    PERFORM set_config('app.tenant_id', tenant_id::TEXT, true);

    INSERT INTO projects (tenant_id, name)
    VALUES (tenant_id, trim(p_project_name))
    RETURNING id INTO project_id;

    INSERT INTO api_keys (tenant_id, project_id, key_hash, label, scopes)
    VALUES (tenant_id, project_id, p_key_hash, v_label, '{}'::TEXT[])
    RETURNING id INTO api_key_id;

    INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role)
    VALUES (p_actor_user_id, tenant_id, 'owner')
    RETURNING id INTO membership_id;

    INSERT INTO audit_log (tenant_id, action, target, payload)
    VALUES (
        tenant_id,
        'control_panel.tenant.created',
        'tenant:' || tenant_id::TEXT,
        jsonb_build_object(
            'control_panel_user_id', p_actor_user_id,
            'project_id', project_id,
            'api_key_id', api_key_id,
            'membership_id', membership_id
        )
    );

    RETURN NEXT;
END;
$$;


--
-- Name: fleet_allocation_events_trim(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.fleet_allocation_events_trim() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    DELETE FROM fleet_allocation_events
    WHERE allocation_id = NEW.allocation_id
      AND id IN (
          SELECT id FROM fleet_allocation_events
          WHERE allocation_id = NEW.allocation_id
          ORDER BY id DESC
          OFFSET 50
      );
    RETURN NULL;
END;
$$;


--
-- Name: notify_matchmaker_ticket(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.notify_matchmaker_ticket() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('matchmaker_ticket', json_build_object(
        'tenant_id',  NEW.tenant_id,
        'project_id', NEW.project_id,
        'mode',       NEW.mode,
        'fleet_id',   NEW.fleet_id,
        'region',     NEW.region,
        'game_mode',  NEW.game_mode
    )::text);
    RETURN NEW;
END;
$$;


--
-- Name: player_account_linked_projects(uuid); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.player_account_linked_projects(p_account_id uuid) RETURNS TABLE(player_id bigint, tenant_id bigint, project_id bigint, project_name text, external_id text, linked_at timestamp with time zone)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT e.id, e.tenant_id, e.project_id, p.name::text, e.external_id, e.created_at
    FROM project_players e
    JOIN projects p ON p.id = e.project_id
    WHERE e.player_account_id = p_account_id
      AND e.deleted_at IS NULL
    ORDER BY e.created_at;
$$;


--
-- Name: player_invite_lookup(bytea); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.player_invite_lookup(p_code_hash bytea) RETURNS TABLE(id bigint, tenant_id bigint, project_id bigint, email public.citext, expires_at timestamp with time zone, project_name text)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT
        i.id,
        i.tenant_id,
        i.project_id,
        i.email,
        i.expires_at,
        p.name
    FROM player_invitations i
    JOIN projects p ON p.id = i.project_id
    WHERE i.code_hash = p_code_hash
      AND i.accepted_at IS NULL
      AND i.revoked_at IS NULL;
$$;


--
-- Name: project_join_context(bigint); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.project_join_context(p_project_id bigint) RETURNS TABLE(tenant_id bigint, effective_enabled boolean, project_name text)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT p.tenant_id,
           (t.public_joining_enabled AND p.public_joining_enabled),
           p.name::text
    FROM projects p
    JOIN tenants t ON t.id = p.tenant_id
    WHERE p.id = p_project_id
      AND p.deleted_at IS NULL
      AND t.deleted_at IS NULL;
$$;


--
-- Name: project_player_tenant(bigint); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.project_player_tenant(p_player_id bigint) RETURNS TABLE(tenant_id bigint)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT tenant_id
    FROM project_players
    WHERE id = p_player_id
      AND deleted_at IS NULL;
$$;


--
-- Name: river_job_state_in_bitmask(bit, public.river_job_state); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.river_job_state_in_bitmask(bitmask bit, state public.river_job_state) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT CASE state
        WHEN 'available' THEN get_bit(bitmask, 7)
        WHEN 'cancelled' THEN get_bit(bitmask, 6)
        WHEN 'completed' THEN get_bit(bitmask, 5)
        WHEN 'discarded' THEN get_bit(bitmask, 4)
        WHEN 'pending'   THEN get_bit(bitmask, 3)
        WHEN 'retryable' THEN get_bit(bitmask, 2)
        WHEN 'running'   THEN get_bit(bitmask, 1)
        WHEN 'scheduled' THEN get_bit(bitmask, 0)
        ELSE 0
    END = 1;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_keys (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint,
    key_hash bytea NOT NULL,
    label text,
    scopes text[] DEFAULT '{}'::text[] NOT NULL,
    key_type text DEFAULT 'secret'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT api_keys_key_type_check CHECK ((key_type = ANY (ARRAY['publishable'::text, 'secret'::text])))
);

ALTER TABLE ONLY public.api_keys FORCE ROW LEVEL SECURITY;


--
-- Name: api_keys_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.api_keys_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: api_keys_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.api_keys_id_seq OWNED BY public.api_keys.id;


--
-- Name: audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_log (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    actor_user_id bigint,
    action text NOT NULL,
    target text,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.audit_log FORCE ROW LEVEL SECURITY;


--
-- Name: audit_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.audit_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: audit_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.audit_log_id_seq OWNED BY public.audit_log.id;


--
-- Name: casbin_rule; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.casbin_rule (
    id bigint NOT NULL,
    ptype text NOT NULL,
    v0 text,
    v1 text,
    v2 text,
    v3 text,
    v4 text,
    v5 text
);


--
-- Name: casbin_rule_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.casbin_rule_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: casbin_rule_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.casbin_rule_id_seq OWNED BY public.casbin_rule.id;


--
-- Name: control_panel_invitations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.control_panel_invitations (
    id bigint NOT NULL,
    email public.citext NOT NULL,
    tenant_id bigint,
    role text NOT NULL,
    code_hash bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    revoked_at timestamp with time zone,
    invited_by_user_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_panel_invitations_role_check CHECK ((role = ANY (ARRAY['platform_admin'::text, 'tenant_admin'::text, 'tenant_member'::text])))
);


--
-- Name: control_panel_invitations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.control_panel_invitations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: control_panel_invitations_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.control_panel_invitations_id_seq OWNED BY public.control_panel_invitations.id;


--
-- Name: control_panel_memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.control_panel_memberships (
    id bigint NOT NULL,
    control_panel_user_id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    role text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT control_panel_memberships_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'admin'::text, 'member'::text])))
);


--
-- Name: control_panel_memberships_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.control_panel_memberships_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: control_panel_memberships_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.control_panel_memberships_id_seq OWNED BY public.control_panel_memberships.id;


--
-- Name: control_panel_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.control_panel_sessions (
    id bigint NOT NULL,
    control_panel_user_id bigint NOT NULL,
    refresh_hash bytea NOT NULL,
    csrf_secret bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    ip text,
    user_agent text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: control_panel_sessions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.control_panel_sessions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: control_panel_sessions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.control_panel_sessions_id_seq OWNED BY public.control_panel_sessions.id;


--
-- Name: control_panel_trusted_devices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.control_panel_trusted_devices (
    id bigint NOT NULL,
    control_panel_user_id bigint NOT NULL,
    token_hash bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    ip text,
    user_agent text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: control_panel_trusted_devices_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.control_panel_trusted_devices_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: control_panel_trusted_devices_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.control_panel_trusted_devices_id_seq OWNED BY public.control_panel_trusted_devices.id;


--
-- Name: control_panel_user_totp; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.control_panel_user_totp (
    control_panel_user_id bigint NOT NULL,
    secret_enc bytea NOT NULL,
    confirmed_at timestamp with time zone,
    last_used_step bigint DEFAULT 0 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: control_panel_user_totp_backup_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.control_panel_user_totp_backup_codes (
    id bigint NOT NULL,
    control_panel_user_id bigint NOT NULL,
    code_hash bytea NOT NULL,
    used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: control_panel_user_totp_backup_codes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.control_panel_user_totp_backup_codes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: control_panel_user_totp_backup_codes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.control_panel_user_totp_backup_codes_id_seq OWNED BY public.control_panel_user_totp_backup_codes.id;


--
-- Name: control_panel_users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.control_panel_users (
    id bigint NOT NULL,
    email public.citext NOT NULL,
    password_hash bytea NOT NULL,
    is_platform_admin boolean DEFAULT false NOT NULL,
    login_failures integer DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    last_login_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    email_verified_at timestamp with time zone,
    email_verification_code_hash bytea,
    email_verification_salt bytea,
    email_verification_expires_at timestamp with time zone,
    email_verification_attempts integer DEFAULT 0 NOT NULL,
    email_verification_last_sent_at timestamp with time zone,
    disabled_at timestamp with time zone,
    email_verification_lifetime_attempts integer DEFAULT 0 NOT NULL,
    email_verification_locked_until timestamp with time zone
);


--
-- Name: control_panel_users_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.control_panel_users_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: control_panel_users_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.control_panel_users_id_seq OWNED BY public.control_panel_users.id;


--
-- Name: feature_grants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.feature_grants (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint,
    feature text NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    approved_by_control_panel_user_id bigint,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT feature_grants_feature_check CHECK ((feature = ANY (ARRAY['p2p_relay'::text, 'dedicated_servers'::text, 'fleet_docker_backend'::text, 'fleet_agones_backend'::text, 'fleet_plugin_backend'::text])))
);

ALTER TABLE ONLY public.feature_grants FORCE ROW LEVEL SECURITY;


--
-- Name: feature_grants_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.feature_grants_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: feature_grants_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.feature_grants_id_seq OWNED BY public.feature_grants.id;


--
-- Name: fleet_allocation_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.fleet_allocation_events (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    allocation_id bigint NOT NULL,
    status public.allocation_status NOT NULL,
    address text DEFAULT ''::text NOT NULL,
    err_message text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.fleet_allocation_events FORCE ROW LEVEL SECURITY;


--
-- Name: fleet_allocation_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.fleet_allocation_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: fleet_allocation_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.fleet_allocation_events_id_seq OWNED BY public.fleet_allocation_events.id;


--
-- Name: fleets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.fleets (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    name text NOT NULL,
    backend text NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);

ALTER TABLE ONLY public.fleets FORCE ROW LEVEL SECURITY;


--
-- Name: fleets_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.fleets_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: fleets_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.fleets_id_seq OWNED BY public.fleets.id;


--
-- Name: friend_edges; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.friend_edges (
    id bigint NOT NULL,
    from_account_id uuid NOT NULL,
    to_account_id uuid NOT NULL,
    status text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT friend_edges_no_self_loop CHECK ((from_account_id <> to_account_id)),
    CONSTRAINT friend_edges_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'accepted'::text, 'rejected'::text, 'blocked'::text])))
);


--
-- Name: friend_edges_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.friend_edges_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: friend_edges_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.friend_edges_id_seq OWNED BY public.friend_edges.id;


--
-- Name: game_invite; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.game_invite (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    from_player_id bigint NOT NULL,
    to_player_id bigint NOT NULL,
    session_id text NOT NULL,
    join_code text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL
);

ALTER TABLE ONLY public.game_invite FORCE ROW LEVEL SECURITY;


--
-- Name: game_invite_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.game_invite_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: game_invite_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.game_invite_id_seq OWNED BY public.game_invite.id;


--
-- Name: game_server_allocations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.game_server_allocations (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    backend text NOT NULL,
    backend_ref text DEFAULT ''::text NOT NULL,
    region text DEFAULT ''::text NOT NULL,
    address text DEFAULT ''::text NOT NULL,
    status public.allocation_status DEFAULT 'pending'::public.allocation_status NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    ready_at timestamp with time zone,
    released_at timestamp with time zone,
    fleet_id bigint
);

ALTER TABLE ONLY public.game_server_allocations FORCE ROW LEVEL SECURITY;


--
-- Name: game_server_allocations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.game_server_allocations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: game_server_allocations_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.game_server_allocations_id_seq OWNED BY public.game_server_allocations.id;


--
-- Name: game_session; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.game_session (
    id text NOT NULL,
    join_code text NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    title_id text DEFAULT ''::text NOT NULL,
    host_player_id bigint NOT NULL,
    state text DEFAULT 'open'::text NOT NULL,
    props jsonb DEFAULT '{}'::jsonb NOT NULL,
    max_players integer DEFAULT 2 NOT NULL,
    private boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT game_session_state_check CHECK ((state = ANY (ARRAY['open'::text, 'in_progress'::text, 'ended'::text])))
);

ALTER TABLE ONLY public.game_session FORCE ROW LEVEL SECURITY;


--
-- Name: game_session_peer; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.game_session_peer (
    tenant_id bigint NOT NULL,
    session_id text NOT NULL,
    player_id bigint NOT NULL,
    ip text,
    port integer,
    qos jsonb DEFAULT '{}'::jsonb NOT NULL,
    last_seen timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.game_session_peer FORCE ROW LEVEL SECURITY;


--
-- Name: leaderboard_entries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.leaderboard_entries (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    leaderboard_id bigint NOT NULL,
    player_id bigint NOT NULL,
    score bigint NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.leaderboard_entries FORCE ROW LEVEL SECURITY;


--
-- Name: leaderboard_entries_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.leaderboard_entries_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: leaderboard_entries_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.leaderboard_entries_id_seq OWNED BY public.leaderboard_entries.id;


--
-- Name: leaderboards; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.leaderboards (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    name text NOT NULL,
    sort_order text DEFAULT 'desc'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT leaderboards_sort_order_check CHECK ((sort_order = ANY (ARRAY['asc'::text, 'desc'::text])))
);

ALTER TABLE ONLY public.leaderboards FORCE ROW LEVEL SECURITY;


--
-- Name: leaderboards_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.leaderboards_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: leaderboards_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.leaderboards_id_seq OWNED BY public.leaderboards.id;


--
-- Name: matchmaker_matches; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.matchmaker_matches (
    id text NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    mode text NOT NULL,
    fleet_id bigint,
    address text DEFAULT ''::text NOT NULL,
    protocol text DEFAULT ''::text NOT NULL,
    session_id text DEFAULT ''::text NOT NULL,
    join_code text DEFAULT ''::text NOT NULL,
    roster jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL
);

ALTER TABLE ONLY public.matchmaker_matches FORCE ROW LEVEL SECURITY;


--
-- Name: matchmaking_tickets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.matchmaking_tickets (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    player_id bigint NOT NULL,
    region text DEFAULT ''::text NOT NULL,
    game_mode text DEFAULT ''::text NOT NULL,
    attributes jsonb DEFAULT '{}'::jsonb NOT NULL,
    status public.ticket_status DEFAULT 'queued'::public.ticket_status NOT NULL,
    match_address text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    matched_at timestamp with time zone,
    fleet_id bigint,
    claim_id uuid,
    claimed_at timestamp with time zone,
    claim_expires_at timestamp with time zone,
    allocation_attempts integer DEFAULT 0 NOT NULL,
    match_protocol text DEFAULT ''::text NOT NULL,
    mode text DEFAULT 'fleet_allocation'::text NOT NULL,
    match_id text DEFAULT ''::text NOT NULL,
    min_count integer DEFAULT 1 NOT NULL,
    max_count integer DEFAULT 1 NOT NULL,
    count_multiple integer DEFAULT 1 NOT NULL,
    allow_cross_region boolean DEFAULT true NOT NULL,
    query text DEFAULT '*'::text NOT NULL,
    string_properties jsonb DEFAULT '{}'::jsonb NOT NULL,
    numeric_properties jsonb DEFAULT '{}'::jsonb NOT NULL,
    expires_at timestamp with time zone,
    CONSTRAINT matchmaking_tickets_count_multiple_check CHECK ((count_multiple >= 1)),
    CONSTRAINT matchmaking_tickets_count_range CHECK ((max_count >= min_count)),
    CONSTRAINT matchmaking_tickets_fleet_mode CHECK (((mode <> 'fleet_allocation'::text) OR (fleet_id IS NOT NULL))),
    CONSTRAINT matchmaking_tickets_min_count_check CHECK ((min_count >= 1)),
    CONSTRAINT matchmaking_tickets_mode_check CHECK ((mode = ANY (ARRAY['match_only'::text, 'game_session'::text, 'fleet_allocation'::text])))
);

ALTER TABLE ONLY public.matchmaking_tickets FORCE ROW LEVEL SECURITY;


--
-- Name: matchmaking_tickets_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.matchmaking_tickets_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: matchmaking_tickets_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.matchmaking_tickets_id_seq OWNED BY public.matchmaking_tickets.id;


--
-- Name: platform_audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.platform_audit_log (
    id bigint NOT NULL,
    actor_user_id bigint,
    action text NOT NULL,
    target text,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: platform_audit_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.platform_audit_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: platform_audit_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.platform_audit_log_id_seq OWNED BY public.platform_audit_log.id;


--
-- Name: player_account_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.player_account_sessions (
    id bigint NOT NULL,
    player_account_id uuid NOT NULL,
    refresh_hash bytea NOT NULL,
    session_epoch integer DEFAULT 0 NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: player_account_sessions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.player_account_sessions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: player_account_sessions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.player_account_sessions_id_seq OWNED BY public.player_account_sessions.id;


--
-- Name: player_account_totp; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.player_account_totp (
    player_account_id uuid NOT NULL,
    secret_enc bytea NOT NULL,
    confirmed_at timestamp with time zone,
    last_used_step bigint DEFAULT 0 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: player_account_totp_backup_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.player_account_totp_backup_codes (
    id bigint NOT NULL,
    player_account_id uuid NOT NULL,
    code_hash bytea NOT NULL,
    used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: player_account_totp_backup_codes_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.player_account_totp_backup_codes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: player_account_totp_backup_codes_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.player_account_totp_backup_codes_id_seq OWNED BY public.player_account_totp_backup_codes.id;


--
-- Name: player_account_trusted_devices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.player_account_trusted_devices (
    id bigint NOT NULL,
    player_account_id uuid NOT NULL,
    token_hash bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: player_account_trusted_devices_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.player_account_trusted_devices_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: player_account_trusted_devices_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.player_account_trusted_devices_id_seq OWNED BY public.player_account_trusted_devices.id;


--
-- Name: player_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.player_accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    email public.citext NOT NULL,
    password_hash bytea NOT NULL,
    display_name text,
    email_verified_at timestamp with time zone,
    disabled_at timestamp with time zone,
    session_epoch integer DEFAULT 0 NOT NULL,
    email_verification_code_hash bytea,
    email_verification_salt bytea,
    email_verification_expires_at timestamp with time zone,
    email_verification_attempts integer DEFAULT 0 NOT NULL,
    email_verification_lifetime_attempts integer DEFAULT 0 NOT NULL,
    email_verification_locked_until timestamp with time zone,
    email_verification_last_sent_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    remote_addr_ip_lan text,
    remote_addr_ip_public text,
    remote_addr_dns text,
    remote_addr_iroh text
);


--
-- Name: player_invitations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.player_invitations (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    email public.citext NOT NULL,
    code_hash bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    revoked_at timestamp with time zone,
    invited_by_user_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.player_invitations FORCE ROW LEVEL SECURITY;


--
-- Name: player_invitations_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.player_invitations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: player_invitations_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.player_invitations_id_seq OWNED BY public.player_invitations.id;


--
-- Name: presence; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.presence (
    tenant_id bigint NOT NULL,
    player_id bigint NOT NULL,
    status text DEFAULT 'online'::text NOT NULL,
    session_id text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT presence_status_check CHECK (((char_length(status) >= 1) AND (char_length(status) <= 32)))
);

ALTER TABLE ONLY public.presence FORCE ROW LEVEL SECURITY;


--
-- Name: project_players; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.project_players (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    external_id text NOT NULL,
    email public.citext,
    email_verified_at timestamp with time zone,
    password_hash bytea,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    email_verification_code_hash bytea,
    email_verification_expires_at timestamp with time zone,
    email_verification_salt bytea,
    email_verification_attempts integer DEFAULT 0 NOT NULL,
    email_verification_last_sent_at timestamp with time zone,
    disabled_at timestamp with time zone,
    email_verification_lifetime_attempts integer DEFAULT 0 NOT NULL,
    email_verification_locked_until timestamp with time zone,
    xuid text,
    player_account_id uuid,
    session_epoch integer DEFAULT 0 NOT NULL
);

ALTER TABLE ONLY public.project_players FORCE ROW LEVEL SECURITY;


--
-- Name: project_players_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.project_players_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: project_players_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.project_players_id_seq OWNED BY public.project_players.id;


--
-- Name: projects; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.projects (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    public_joining_enabled boolean DEFAULT true NOT NULL
);

ALTER TABLE ONLY public.projects FORCE ROW LEVEL SECURITY;


--
-- Name: projects_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.projects_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: projects_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.projects_id_seq OWNED BY public.projects.id;


--
-- Name: rate_limit_overrides; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.rate_limit_overrides (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint,
    kind text NOT NULL,
    rate double precision NOT NULL,
    burst double precision NOT NULL,
    updated_by bigint,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT rate_limit_overrides_burst_check CHECK ((burst >= (0)::double precision)),
    CONSTRAINT rate_limit_overrides_kind_check CHECK ((kind = ANY (ARRAY['api'::text, 'invite_inviter'::text, 'invite_domain'::text, 'invite_recipient'::text]))),
    CONSTRAINT rate_limit_overrides_rate_check CHECK ((rate >= (0)::double precision))
);


--
-- Name: rate_limit_overrides_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.rate_limit_overrides_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: rate_limit_overrides_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.rate_limit_overrides_id_seq OWNED BY public.rate_limit_overrides.id;


--
-- Name: river_client; Type: TABLE; Schema: public; Owner: -
--

CREATE UNLOGGED TABLE public.river_client (
    id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    paused_at timestamp with time zone,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT name_length CHECK (((char_length(id) > 0) AND (char_length(id) < 128)))
);


--
-- Name: river_client_queue; Type: TABLE; Schema: public; Owner: -
--

CREATE UNLOGGED TABLE public.river_client_queue (
    river_client_id text NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    max_workers bigint DEFAULT 0 NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    num_jobs_completed bigint DEFAULT 0 NOT NULL,
    num_jobs_running bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT name_length CHECK (((char_length(name) > 0) AND (char_length(name) < 128))),
    CONSTRAINT num_jobs_completed_zero_or_positive CHECK ((num_jobs_completed >= 0)),
    CONSTRAINT num_jobs_running_zero_or_positive CHECK ((num_jobs_running >= 0))
);


--
-- Name: river_job; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.river_job (
    id bigint NOT NULL,
    state public.river_job_state DEFAULT 'available'::public.river_job_state NOT NULL,
    attempt smallint DEFAULT 0 NOT NULL,
    max_attempts smallint NOT NULL,
    attempted_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    finalized_at timestamp with time zone,
    scheduled_at timestamp with time zone DEFAULT now() NOT NULL,
    priority smallint DEFAULT 1 NOT NULL,
    args jsonb NOT NULL,
    attempted_by text[],
    errors jsonb[],
    kind text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    queue text DEFAULT 'default'::text NOT NULL,
    tags character varying(255)[] DEFAULT '{}'::character varying[] NOT NULL,
    unique_key bytea,
    unique_states bit(8),
    CONSTRAINT finalized_or_finalized_at_null CHECK ((((finalized_at IS NULL) AND (state <> ALL (ARRAY['cancelled'::public.river_job_state, 'completed'::public.river_job_state, 'discarded'::public.river_job_state]))) OR ((finalized_at IS NOT NULL) AND (state = ANY (ARRAY['cancelled'::public.river_job_state, 'completed'::public.river_job_state, 'discarded'::public.river_job_state]))))),
    CONSTRAINT kind_length CHECK (((char_length(kind) > 0) AND (char_length(kind) < 128))),
    CONSTRAINT max_attempts_is_positive CHECK ((max_attempts > 0)),
    CONSTRAINT priority_in_range CHECK (((priority >= 1) AND (priority <= 4))),
    CONSTRAINT queue_length CHECK (((char_length(queue) > 0) AND (char_length(queue) < 128)))
);


--
-- Name: river_job_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.river_job_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: river_job_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.river_job_id_seq OWNED BY public.river_job.id;


--
-- Name: river_leader; Type: TABLE; Schema: public; Owner: -
--

CREATE UNLOGGED TABLE public.river_leader (
    elected_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    leader_id text NOT NULL,
    name text DEFAULT 'default'::text NOT NULL,
    CONSTRAINT leader_id_length CHECK (((char_length(leader_id) > 0) AND (char_length(leader_id) < 128))),
    CONSTRAINT name_length CHECK ((name = 'default'::text))
);


--
-- Name: river_queue; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.river_queue (
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    paused_at timestamp with time zone,
    updated_at timestamp with time zone NOT NULL
);


--
-- Name: server_secrets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.server_secrets (
    name text NOT NULL,
    value bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    player_id bigint NOT NULL,
    refresh_hash bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    project_id bigint NOT NULL
);

ALTER TABLE ONLY public.sessions FORCE ROW LEVEL SECURITY;


--
-- Name: sessions_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.sessions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: sessions_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.sessions_id_seq OWNED BY public.sessions.id;


--
-- Name: storage_objects; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_objects (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    owner_user_id bigint NOT NULL,
    key text NOT NULL,
    value jsonb NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone
);

ALTER TABLE ONLY public.storage_objects FORCE ROW LEVEL SECURITY;


--
-- Name: storage_objects_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.storage_objects_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: storage_objects_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.storage_objects_id_seq OWNED BY public.storage_objects.id;


--
-- Name: tenant_player_bans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tenant_player_bans (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    player_account_id uuid NOT NULL,
    reason text,
    created_by bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: tenant_player_bans_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.tenant_player_bans_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: tenant_player_bans_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.tenant_player_bans_id_seq OWNED BY public.tenant_player_bans.id;


--
-- Name: tenants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tenants (
    id bigint NOT NULL,
    name text NOT NULL,
    tier text DEFAULT 'free'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    custom_token_secret bytea,
    public_joining_enabled boolean DEFAULT true NOT NULL,
    CONSTRAINT tenants_tier_check CHECK ((tier = ANY (ARRAY['free'::text, 'payg'::text, 'premium'::text])))
);

ALTER TABLE ONLY public.tenants FORCE ROW LEVEL SECURITY;


--
-- Name: tenants_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.tenants_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: tenants_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.tenants_id_seq OWNED BY public.tenants.id;


--
-- Name: usage_samples; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples (
    id bigint NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
)
PARTITION BY RANGE (ts);

ALTER TABLE ONLY public.usage_samples FORCE ROW LEVEL SECURITY;


--
-- Name: usage_samples_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.usage_samples_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: usage_samples_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.usage_samples_id_seq OWNED BY public.usage_samples.id;


--
-- Name: usage_samples_2026_07; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2026_07 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2026_08; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2026_08 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2026_09; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2026_09 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2026_10; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2026_10 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2026_11; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2026_11 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2026_12; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2026_12 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2027_01; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2027_01 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2027_02; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2027_02 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2027_03; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2027_03 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2027_04; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2027_04 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2027_05; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2027_05 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2027_06; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_2027_06 (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.usage_samples_default (
    id bigint DEFAULT nextval('public.usage_samples_id_seq'::regclass) NOT NULL,
    tenant_id bigint NOT NULL,
    project_id bigint NOT NULL,
    ts timestamp with time zone NOT NULL,
    ccu integer DEFAULT 0 NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    bytes_egress bigint DEFAULT 0 NOT NULL
);


--
-- Name: usage_samples_2026_07; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2026_07 FOR VALUES FROM ('2026-07-01 00:00:00+00') TO ('2026-08-01 00:00:00+00');


--
-- Name: usage_samples_2026_08; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2026_08 FOR VALUES FROM ('2026-08-01 00:00:00+00') TO ('2026-09-01 00:00:00+00');


--
-- Name: usage_samples_2026_09; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2026_09 FOR VALUES FROM ('2026-09-01 00:00:00+00') TO ('2026-10-01 00:00:00+00');


--
-- Name: usage_samples_2026_10; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2026_10 FOR VALUES FROM ('2026-10-01 00:00:00+00') TO ('2026-11-01 00:00:00+00');


--
-- Name: usage_samples_2026_11; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2026_11 FOR VALUES FROM ('2026-11-01 00:00:00+00') TO ('2026-12-01 00:00:00+00');


--
-- Name: usage_samples_2026_12; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2026_12 FOR VALUES FROM ('2026-12-01 00:00:00+00') TO ('2027-01-01 00:00:00+00');


--
-- Name: usage_samples_2027_01; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2027_01 FOR VALUES FROM ('2027-01-01 00:00:00+00') TO ('2027-02-01 00:00:00+00');


--
-- Name: usage_samples_2027_02; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2027_02 FOR VALUES FROM ('2027-02-01 00:00:00+00') TO ('2027-03-01 00:00:00+00');


--
-- Name: usage_samples_2027_03; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2027_03 FOR VALUES FROM ('2027-03-01 00:00:00+00') TO ('2027-04-01 00:00:00+00');


--
-- Name: usage_samples_2027_04; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2027_04 FOR VALUES FROM ('2027-04-01 00:00:00+00') TO ('2027-05-01 00:00:00+00');


--
-- Name: usage_samples_2027_05; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2027_05 FOR VALUES FROM ('2027-05-01 00:00:00+00') TO ('2027-06-01 00:00:00+00');


--
-- Name: usage_samples_2027_06; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_2027_06 FOR VALUES FROM ('2027-06-01 00:00:00+00') TO ('2027-07-01 00:00:00+00');


--
-- Name: usage_samples_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ATTACH PARTITION public.usage_samples_default DEFAULT;


--
-- Name: api_keys id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys ALTER COLUMN id SET DEFAULT nextval('public.api_keys_id_seq'::regclass);


--
-- Name: audit_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_log ALTER COLUMN id SET DEFAULT nextval('public.audit_log_id_seq'::regclass);


--
-- Name: casbin_rule id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.casbin_rule ALTER COLUMN id SET DEFAULT nextval('public.casbin_rule_id_seq'::regclass);


--
-- Name: control_panel_invitations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_invitations ALTER COLUMN id SET DEFAULT nextval('public.control_panel_invitations_id_seq'::regclass);


--
-- Name: control_panel_memberships id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_memberships ALTER COLUMN id SET DEFAULT nextval('public.control_panel_memberships_id_seq'::regclass);


--
-- Name: control_panel_sessions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_sessions ALTER COLUMN id SET DEFAULT nextval('public.control_panel_sessions_id_seq'::regclass);


--
-- Name: control_panel_trusted_devices id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_trusted_devices ALTER COLUMN id SET DEFAULT nextval('public.control_panel_trusted_devices_id_seq'::regclass);


--
-- Name: control_panel_user_totp_backup_codes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_user_totp_backup_codes ALTER COLUMN id SET DEFAULT nextval('public.control_panel_user_totp_backup_codes_id_seq'::regclass);


--
-- Name: control_panel_users id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_users ALTER COLUMN id SET DEFAULT nextval('public.control_panel_users_id_seq'::regclass);


--
-- Name: feature_grants id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feature_grants ALTER COLUMN id SET DEFAULT nextval('public.feature_grants_id_seq'::regclass);


--
-- Name: fleet_allocation_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleet_allocation_events ALTER COLUMN id SET DEFAULT nextval('public.fleet_allocation_events_id_seq'::regclass);


--
-- Name: fleets id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleets ALTER COLUMN id SET DEFAULT nextval('public.fleets_id_seq'::regclass);


--
-- Name: friend_edges id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.friend_edges ALTER COLUMN id SET DEFAULT nextval('public.friend_edges_id_seq'::regclass);


--
-- Name: game_invite id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_invite ALTER COLUMN id SET DEFAULT nextval('public.game_invite_id_seq'::regclass);


--
-- Name: game_server_allocations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_server_allocations ALTER COLUMN id SET DEFAULT nextval('public.game_server_allocations_id_seq'::regclass);


--
-- Name: leaderboard_entries id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboard_entries ALTER COLUMN id SET DEFAULT nextval('public.leaderboard_entries_id_seq'::regclass);


--
-- Name: leaderboards id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboards ALTER COLUMN id SET DEFAULT nextval('public.leaderboards_id_seq'::regclass);


--
-- Name: matchmaking_tickets id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaking_tickets ALTER COLUMN id SET DEFAULT nextval('public.matchmaking_tickets_id_seq'::regclass);


--
-- Name: platform_audit_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.platform_audit_log ALTER COLUMN id SET DEFAULT nextval('public.platform_audit_log_id_seq'::regclass);


--
-- Name: player_account_sessions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_sessions ALTER COLUMN id SET DEFAULT nextval('public.player_account_sessions_id_seq'::regclass);


--
-- Name: player_account_totp_backup_codes id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_totp_backup_codes ALTER COLUMN id SET DEFAULT nextval('public.player_account_totp_backup_codes_id_seq'::regclass);


--
-- Name: player_account_trusted_devices id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_trusted_devices ALTER COLUMN id SET DEFAULT nextval('public.player_account_trusted_devices_id_seq'::regclass);


--
-- Name: player_invitations id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_invitations ALTER COLUMN id SET DEFAULT nextval('public.player_invitations_id_seq'::regclass);


--
-- Name: project_players id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_players ALTER COLUMN id SET DEFAULT nextval('public.project_players_id_seq'::regclass);


--
-- Name: projects id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects ALTER COLUMN id SET DEFAULT nextval('public.projects_id_seq'::regclass);


--
-- Name: rate_limit_overrides id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rate_limit_overrides ALTER COLUMN id SET DEFAULT nextval('public.rate_limit_overrides_id_seq'::regclass);


--
-- Name: river_job id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_job ALTER COLUMN id SET DEFAULT nextval('public.river_job_id_seq'::regclass);


--
-- Name: sessions id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions ALTER COLUMN id SET DEFAULT nextval('public.sessions_id_seq'::regclass);


--
-- Name: storage_objects id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_objects ALTER COLUMN id SET DEFAULT nextval('public.storage_objects_id_seq'::regclass);


--
-- Name: tenant_player_bans id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_player_bans ALTER COLUMN id SET DEFAULT nextval('public.tenant_player_bans_id_seq'::regclass);


--
-- Name: tenants id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenants ALTER COLUMN id SET DEFAULT nextval('public.tenants_id_seq'::regclass);


--
-- Name: usage_samples id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples ALTER COLUMN id SET DEFAULT nextval('public.usage_samples_id_seq'::regclass);


--
-- Name: api_keys api_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash);


--
-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);


--
-- Name: audit_log audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id);


--
-- Name: casbin_rule casbin_rule_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.casbin_rule
    ADD CONSTRAINT casbin_rule_pkey PRIMARY KEY (id);


--
-- Name: control_panel_invitations control_panel_invitations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_invitations
    ADD CONSTRAINT control_panel_invitations_pkey PRIMARY KEY (id);


--
-- Name: control_panel_memberships control_panel_memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_memberships
    ADD CONSTRAINT control_panel_memberships_pkey PRIMARY KEY (id);


--
-- Name: control_panel_memberships control_panel_memberships_user_id_tenant_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_memberships
    ADD CONSTRAINT control_panel_memberships_user_id_tenant_id_key UNIQUE (control_panel_user_id, tenant_id);


--
-- Name: control_panel_sessions control_panel_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_sessions
    ADD CONSTRAINT control_panel_sessions_pkey PRIMARY KEY (id);


--
-- Name: control_panel_sessions control_panel_sessions_refresh_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_sessions
    ADD CONSTRAINT control_panel_sessions_refresh_hash_key UNIQUE (refresh_hash);


--
-- Name: control_panel_trusted_devices control_panel_trusted_devices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_trusted_devices
    ADD CONSTRAINT control_panel_trusted_devices_pkey PRIMARY KEY (id);


--
-- Name: control_panel_trusted_devices control_panel_trusted_devices_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_trusted_devices
    ADD CONSTRAINT control_panel_trusted_devices_token_hash_key UNIQUE (token_hash);


--
-- Name: control_panel_user_totp_backup_codes control_panel_user_totp_backup_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_user_totp_backup_codes
    ADD CONSTRAINT control_panel_user_totp_backup_codes_pkey PRIMARY KEY (id);


--
-- Name: control_panel_user_totp_backup_codes control_panel_user_totp_backup_codes_user_id_code_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_user_totp_backup_codes
    ADD CONSTRAINT control_panel_user_totp_backup_codes_user_id_code_hash_key UNIQUE (control_panel_user_id, code_hash);


--
-- Name: control_panel_user_totp control_panel_user_totp_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_user_totp
    ADD CONSTRAINT control_panel_user_totp_pkey PRIMARY KEY (control_panel_user_id);


--
-- Name: control_panel_users control_panel_users_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_users
    ADD CONSTRAINT control_panel_users_email_key UNIQUE (email);


--
-- Name: control_panel_users control_panel_users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_users
    ADD CONSTRAINT control_panel_users_pkey PRIMARY KEY (id);


--
-- Name: feature_grants feature_grants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feature_grants
    ADD CONSTRAINT feature_grants_pkey PRIMARY KEY (id);


--
-- Name: fleet_allocation_events fleet_allocation_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleet_allocation_events
    ADD CONSTRAINT fleet_allocation_events_pkey PRIMARY KEY (id);


--
-- Name: fleets fleets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleets
    ADD CONSTRAINT fleets_pkey PRIMARY KEY (id);


--
-- Name: friend_edges friend_edges_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.friend_edges
    ADD CONSTRAINT friend_edges_pkey PRIMARY KEY (id);


--
-- Name: game_invite game_invite_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_invite
    ADD CONSTRAINT game_invite_pkey PRIMARY KEY (id);


--
-- Name: game_server_allocations game_server_allocations_fleet_id_required; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.game_server_allocations
    ADD CONSTRAINT game_server_allocations_fleet_id_required CHECK ((fleet_id IS NOT NULL)) NOT VALID;


--
-- Name: game_server_allocations game_server_allocations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_server_allocations
    ADD CONSTRAINT game_server_allocations_pkey PRIMARY KEY (id);


--
-- Name: game_session game_session_join_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session
    ADD CONSTRAINT game_session_join_code_key UNIQUE (join_code);


--
-- Name: game_session_peer game_session_peer_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session_peer
    ADD CONSTRAINT game_session_peer_pkey PRIMARY KEY (session_id, player_id);


--
-- Name: game_session game_session_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session
    ADD CONSTRAINT game_session_pkey PRIMARY KEY (id);


--
-- Name: leaderboard_entries leaderboard_entries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboard_entries
    ADD CONSTRAINT leaderboard_entries_pkey PRIMARY KEY (id);


--
-- Name: leaderboards leaderboards_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboards
    ADD CONSTRAINT leaderboards_pkey PRIMARY KEY (id);


--
-- Name: matchmaker_matches matchmaker_matches_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaker_matches
    ADD CONSTRAINT matchmaker_matches_pkey PRIMARY KEY (id);


--
-- Name: matchmaking_tickets matchmaking_tickets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_pkey PRIMARY KEY (id);


--
-- Name: platform_audit_log platform_audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.platform_audit_log
    ADD CONSTRAINT platform_audit_log_pkey PRIMARY KEY (id);


--
-- Name: player_account_sessions player_account_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_sessions
    ADD CONSTRAINT player_account_sessions_pkey PRIMARY KEY (id);


--
-- Name: player_account_sessions player_account_sessions_refresh_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_sessions
    ADD CONSTRAINT player_account_sessions_refresh_hash_key UNIQUE (refresh_hash);


--
-- Name: player_account_totp_backup_codes player_account_totp_backup_code_player_account_id_code_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_totp_backup_codes
    ADD CONSTRAINT player_account_totp_backup_code_player_account_id_code_hash_key UNIQUE (player_account_id, code_hash);


--
-- Name: player_account_totp_backup_codes player_account_totp_backup_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_totp_backup_codes
    ADD CONSTRAINT player_account_totp_backup_codes_pkey PRIMARY KEY (id);


--
-- Name: player_account_totp player_account_totp_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_totp
    ADD CONSTRAINT player_account_totp_pkey PRIMARY KEY (player_account_id);


--
-- Name: player_account_trusted_devices player_account_trusted_devices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_trusted_devices
    ADD CONSTRAINT player_account_trusted_devices_pkey PRIMARY KEY (id);


--
-- Name: player_account_trusted_devices player_account_trusted_devices_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_trusted_devices
    ADD CONSTRAINT player_account_trusted_devices_token_hash_key UNIQUE (token_hash);


--
-- Name: player_accounts player_accounts_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_accounts
    ADD CONSTRAINT player_accounts_email_key UNIQUE (email);


--
-- Name: player_accounts player_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_accounts
    ADD CONSTRAINT player_accounts_pkey PRIMARY KEY (id);


--
-- Name: player_invitations player_invitations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_invitations
    ADD CONSTRAINT player_invitations_pkey PRIMARY KEY (id);


--
-- Name: presence presence_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.presence
    ADD CONSTRAINT presence_pkey PRIMARY KEY (tenant_id, player_id);


--
-- Name: project_players project_players_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_players
    ADD CONSTRAINT project_players_pkey PRIMARY KEY (id);


--
-- Name: projects projects_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_pkey PRIMARY KEY (id);


--
-- Name: rate_limit_overrides rate_limit_overrides_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rate_limit_overrides
    ADD CONSTRAINT rate_limit_overrides_pkey PRIMARY KEY (id);


--
-- Name: river_client river_client_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_client
    ADD CONSTRAINT river_client_pkey PRIMARY KEY (id);


--
-- Name: river_client_queue river_client_queue_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_client_queue
    ADD CONSTRAINT river_client_queue_pkey PRIMARY KEY (river_client_id, name);


--
-- Name: river_job river_job_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_job
    ADD CONSTRAINT river_job_pkey PRIMARY KEY (id);


--
-- Name: river_leader river_leader_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_leader
    ADD CONSTRAINT river_leader_pkey PRIMARY KEY (name);


--
-- Name: river_queue river_queue_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_queue
    ADD CONSTRAINT river_queue_pkey PRIMARY KEY (name);


--
-- Name: server_secrets server_secrets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.server_secrets
    ADD CONSTRAINT server_secrets_pkey PRIMARY KEY (name);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_refresh_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_refresh_hash_key UNIQUE (refresh_hash);


--
-- Name: storage_objects storage_objects_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_objects
    ADD CONSTRAINT storage_objects_pkey PRIMARY KEY (id);


--
-- Name: tenant_player_bans tenant_player_bans_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_player_bans
    ADD CONSTRAINT tenant_player_bans_pkey PRIMARY KEY (id);


--
-- Name: tenant_player_bans tenant_player_bans_tenant_id_player_account_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_player_bans
    ADD CONSTRAINT tenant_player_bans_tenant_id_player_account_id_key UNIQUE (tenant_id, player_account_id);


--
-- Name: tenants tenants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenants
    ADD CONSTRAINT tenants_pkey PRIMARY KEY (id);


--
-- Name: usage_samples usage_samples_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples
    ADD CONSTRAINT usage_samples_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2026_07 usage_samples_2026_07_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2026_07
    ADD CONSTRAINT usage_samples_2026_07_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2026_08 usage_samples_2026_08_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2026_08
    ADD CONSTRAINT usage_samples_2026_08_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2026_09 usage_samples_2026_09_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2026_09
    ADD CONSTRAINT usage_samples_2026_09_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2026_10 usage_samples_2026_10_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2026_10
    ADD CONSTRAINT usage_samples_2026_10_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2026_11 usage_samples_2026_11_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2026_11
    ADD CONSTRAINT usage_samples_2026_11_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2026_12 usage_samples_2026_12_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2026_12
    ADD CONSTRAINT usage_samples_2026_12_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2027_01 usage_samples_2027_01_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2027_01
    ADD CONSTRAINT usage_samples_2027_01_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2027_02 usage_samples_2027_02_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2027_02
    ADD CONSTRAINT usage_samples_2027_02_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2027_03 usage_samples_2027_03_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2027_03
    ADD CONSTRAINT usage_samples_2027_03_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2027_04 usage_samples_2027_04_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2027_04
    ADD CONSTRAINT usage_samples_2027_04_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2027_05 usage_samples_2027_05_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2027_05
    ADD CONSTRAINT usage_samples_2027_05_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_2027_06 usage_samples_2027_06_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_2027_06
    ADD CONSTRAINT usage_samples_2027_06_pkey PRIMARY KEY (id, ts);


--
-- Name: usage_samples_default usage_samples_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_samples_default
    ADD CONSTRAINT usage_samples_default_pkey PRIMARY KEY (id, ts);


--
-- Name: api_keys_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_keys_active_idx ON public.api_keys USING btree (tenant_id) WHERE (revoked_at IS NULL);


--
-- Name: api_keys_project_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_keys_project_id_idx ON public.api_keys USING btree (project_id) WHERE (project_id IS NOT NULL);


--
-- Name: api_keys_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_keys_tenant_id_idx ON public.api_keys USING btree (tenant_id);


--
-- Name: api_keys_type_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_keys_type_idx ON public.api_keys USING btree (tenant_id, key_type) WHERE (revoked_at IS NULL);


--
-- Name: audit_log_actor_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_log_actor_idx ON public.audit_log USING btree (tenant_id, actor_user_id, occurred_at DESC) WHERE (actor_user_id IS NOT NULL);


--
-- Name: audit_log_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_log_tenant_id_idx ON public.audit_log USING btree (tenant_id, occurred_at DESC);


--
-- Name: casbin_rule_unique_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX casbin_rule_unique_idx ON public.casbin_rule USING btree (ptype, COALESCE(v0, ''::text), COALESCE(v1, ''::text), COALESCE(v2, ''::text), COALESCE(v3, ''::text), COALESCE(v4, ''::text), COALESCE(v5, ''::text));


--
-- Name: control_panel_invitations_code_lookup_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_invitations_code_lookup_idx ON public.control_panel_invitations USING btree (code_hash) WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: control_panel_invitations_open_uq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX control_panel_invitations_open_uq ON public.control_panel_invitations USING btree (email, COALESCE(tenant_id, (0)::bigint)) WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: control_panel_invitations_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_invitations_tenant_idx ON public.control_panel_invitations USING btree (tenant_id) WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: control_panel_memberships_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_memberships_tenant_idx ON public.control_panel_memberships USING btree (tenant_id);


--
-- Name: control_panel_sessions_user_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_sessions_user_active_idx ON public.control_panel_sessions USING btree (control_panel_user_id) WHERE (revoked_at IS NULL);


--
-- Name: control_panel_trusted_devices_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_trusted_devices_user_idx ON public.control_panel_trusted_devices USING btree (control_panel_user_id);


--
-- Name: control_panel_users_created_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_users_created_id_idx ON public.control_panel_users USING btree (created_at DESC, id DESC);


--
-- Name: control_panel_users_disabled_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_users_disabled_idx ON public.control_panel_users USING btree (disabled_at) WHERE (disabled_at IS NOT NULL);


--
-- Name: control_panel_users_email_trgm_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX control_panel_users_email_trgm_idx ON public.control_panel_users USING gin (((email)::text) public.gin_trgm_ops);


--
-- Name: feature_grants_tenant_feature_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX feature_grants_tenant_feature_idx ON public.feature_grants USING btree (tenant_id, feature);


--
-- Name: feature_grants_unique_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX feature_grants_unique_idx ON public.feature_grants USING btree (tenant_id, COALESCE(project_id, (0)::bigint), feature);


--
-- Name: fleet_allocation_events_alloc_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX fleet_allocation_events_alloc_idx ON public.fleet_allocation_events USING btree (allocation_id, id DESC);


--
-- Name: fleets_project_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX fleets_project_active_idx ON public.fleets USING btree (project_id) WHERE (deleted_at IS NULL);


--
-- Name: fleets_project_name_active_uidx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX fleets_project_name_active_uidx ON public.fleets USING btree (project_id, name) WHERE (deleted_at IS NULL);


--
-- Name: fleets_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX fleets_tenant_id_idx ON public.fleets USING btree (tenant_id);


--
-- Name: friend_edges_pair_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX friend_edges_pair_uniq ON public.friend_edges USING btree (from_account_id, to_account_id);


--
-- Name: friend_edges_to_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX friend_edges_to_account_idx ON public.friend_edges USING btree (to_account_id, status);


--
-- Name: game_invite_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_invite_tenant_idx ON public.game_invite USING btree (tenant_id);


--
-- Name: game_invite_to_player_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_invite_to_player_idx ON public.game_invite USING btree (tenant_id, to_player_id, expires_at);


--
-- Name: game_server_allocations_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_server_allocations_active_idx ON public.game_server_allocations USING btree (tenant_id, project_id, status) WHERE (released_at IS NULL);


--
-- Name: game_server_allocations_fleet_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_server_allocations_fleet_id_idx ON public.game_server_allocations USING btree (fleet_id);


--
-- Name: game_server_allocations_project_id_desc_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_server_allocations_project_id_desc_idx ON public.game_server_allocations USING btree (tenant_id, project_id, id DESC);


--
-- Name: game_server_allocations_project_live_id_desc_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_server_allocations_project_live_id_desc_idx ON public.game_server_allocations USING btree (tenant_id, project_id, id DESC) WHERE (released_at IS NULL);


--
-- Name: game_server_allocations_tenant_backend_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_server_allocations_tenant_backend_idx ON public.game_server_allocations USING btree (tenant_id, backend);


--
-- Name: game_server_allocations_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_server_allocations_tenant_id_idx ON public.game_server_allocations USING btree (tenant_id);


--
-- Name: game_session_join_code_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_session_join_code_idx ON public.game_session USING btree (join_code) WHERE (state = 'open'::text);


--
-- Name: game_session_peer_session_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_session_peer_session_idx ON public.game_session_peer USING btree (session_id, last_seen);


--
-- Name: game_session_project_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_session_project_state_idx ON public.game_session USING btree (project_id, state, expires_at);


--
-- Name: game_session_tenant_state_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX game_session_tenant_state_idx ON public.game_session USING btree (tenant_id, state);


--
-- Name: leaderboard_entries_best_score_asc_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX leaderboard_entries_best_score_asc_idx ON public.leaderboard_entries USING btree (tenant_id, leaderboard_id, player_id, score);


--
-- Name: leaderboard_entries_best_score_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX leaderboard_entries_best_score_idx ON public.leaderboard_entries USING btree (tenant_id, leaderboard_id, player_id, score DESC);


--
-- Name: leaderboard_entries_player_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX leaderboard_entries_player_idx ON public.leaderboard_entries USING btree (tenant_id, leaderboard_id, player_id);


--
-- Name: leaderboard_entries_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX leaderboard_entries_tenant_id_idx ON public.leaderboard_entries USING btree (tenant_id);


--
-- Name: leaderboard_entries_top_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX leaderboard_entries_top_idx ON public.leaderboard_entries USING btree (tenant_id, leaderboard_id, score DESC, recorded_at);


--
-- Name: leaderboards_name_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX leaderboards_name_uniq ON public.leaderboards USING btree (tenant_id, project_id, name) WHERE (deleted_at IS NULL);


--
-- Name: leaderboards_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX leaderboards_tenant_id_idx ON public.leaderboards USING btree (tenant_id);


--
-- Name: matchmaker_matches_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX matchmaker_matches_expires_idx ON public.matchmaker_matches USING btree (expires_at);


--
-- Name: matchmaking_tickets_claim_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX matchmaking_tickets_claim_expiry_idx ON public.matchmaking_tickets USING btree (claim_expires_at) WHERE (claim_id IS NOT NULL);


--
-- Name: matchmaking_tickets_queued_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX matchmaking_tickets_queued_idx ON public.matchmaking_tickets USING btree (tenant_id, project_id, mode, fleet_id, region, game_mode, created_at, id) WHERE ((status = 'queued'::public.ticket_status) AND (claim_id IS NULL));


--
-- Name: matchmaking_tickets_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX matchmaking_tickets_tenant_id_idx ON public.matchmaking_tickets USING btree (tenant_id);


--
-- Name: platform_audit_log_action_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX platform_audit_log_action_idx ON public.platform_audit_log USING btree (action, occurred_at DESC);


--
-- Name: player_account_sessions_account_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX player_account_sessions_account_active_idx ON public.player_account_sessions USING btree (player_account_id) WHERE (revoked_at IS NULL);


--
-- Name: player_account_trusted_devices_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX player_account_trusted_devices_account_idx ON public.player_account_trusted_devices USING btree (player_account_id);


--
-- Name: player_invitations_code_lookup_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX player_invitations_code_lookup_idx ON public.player_invitations USING btree (code_hash) WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: player_invitations_open_uq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX player_invitations_open_uq ON public.player_invitations USING btree (project_id, email) WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL));


--
-- Name: player_invitations_tenant_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX player_invitations_tenant_idx ON public.player_invitations USING btree (tenant_id);


--
-- Name: project_players_disabled_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_players_disabled_idx ON public.project_players USING btree (disabled_at) WHERE (disabled_at IS NOT NULL);


--
-- Name: project_players_email_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX project_players_email_uniq ON public.project_players USING btree (tenant_id, project_id, email) WHERE ((email IS NOT NULL) AND (deleted_at IS NULL));


--
-- Name: project_players_email_verification_code_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_players_email_verification_code_idx ON public.project_players USING btree (email_verification_code_hash) WHERE (email_verification_code_hash IS NOT NULL);


--
-- Name: project_players_external_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX project_players_external_uniq ON public.project_players USING btree (tenant_id, project_id, external_id) WHERE (deleted_at IS NULL);


--
-- Name: project_players_player_account_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_players_player_account_id_idx ON public.project_players USING btree (player_account_id) WHERE (player_account_id IS NOT NULL);


--
-- Name: project_players_project_created_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_players_project_created_id_idx ON public.project_players USING btree (project_id, created_at DESC, id DESC) WHERE (deleted_at IS NULL);


--
-- Name: project_players_project_email_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_players_project_email_idx ON public.project_players USING btree (project_id, email) WHERE (deleted_at IS NULL);


--
-- Name: project_players_project_email_trgm_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_players_project_email_trgm_idx ON public.project_players USING gin (((email)::text) public.gin_trgm_ops) WHERE ((deleted_at IS NULL) AND (email IS NOT NULL));


--
-- Name: project_players_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX project_players_tenant_id_idx ON public.project_players USING btree (tenant_id);


--
-- Name: project_players_xuid_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX project_players_xuid_uniq ON public.project_players USING btree (project_id, xuid) WHERE ((xuid IS NOT NULL) AND (deleted_at IS NULL));


--
-- Name: projects_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX projects_tenant_id_idx ON public.projects USING btree (tenant_id);


--
-- Name: projects_tenant_name_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX projects_tenant_name_uniq ON public.projects USING btree (tenant_id, name) WHERE (deleted_at IS NULL);


--
-- Name: rate_limit_overrides_tenant_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX rate_limit_overrides_tenant_kind_idx ON public.rate_limit_overrides USING btree (tenant_id, kind);


--
-- Name: rate_limit_overrides_unique_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX rate_limit_overrides_unique_idx ON public.rate_limit_overrides USING btree (tenant_id, COALESCE(project_id, (0)::bigint), kind);


--
-- Name: river_job_args_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_args_index ON public.river_job USING gin (args);


--
-- Name: river_job_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_kind ON public.river_job USING btree (kind);


--
-- Name: river_job_metadata_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_metadata_index ON public.river_job USING gin (metadata);


--
-- Name: river_job_prioritized_fetching_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_prioritized_fetching_index ON public.river_job USING btree (state, queue, priority, scheduled_at, id);


--
-- Name: river_job_state_and_finalized_at_index; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX river_job_state_and_finalized_at_index ON public.river_job USING btree (state, finalized_at) WHERE (finalized_at IS NOT NULL);


--
-- Name: river_job_unique_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX river_job_unique_idx ON public.river_job USING btree (unique_key) WHERE ((unique_key IS NOT NULL) AND (unique_states IS NOT NULL) AND public.river_job_state_in_bitmask(unique_states, state));


--
-- Name: sessions_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_active_idx ON public.sessions USING btree (tenant_id, expires_at) WHERE (revoked_at IS NULL);


--
-- Name: sessions_player_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_player_id_idx ON public.sessions USING btree (player_id);


--
-- Name: sessions_project_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_project_active_idx ON public.sessions USING btree (tenant_id, project_id, expires_at) WHERE (revoked_at IS NULL);


--
-- Name: sessions_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_tenant_id_idx ON public.sessions USING btree (tenant_id);


--
-- Name: storage_objects_owner_key_uniq; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX storage_objects_owner_key_uniq ON public.storage_objects USING btree (tenant_id, project_id, owner_user_id, key) WHERE (deleted_at IS NULL);


--
-- Name: storage_objects_tenant_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_objects_tenant_id_idx ON public.storage_objects USING btree (tenant_id);


--
-- Name: tenant_player_bans_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tenant_player_bans_account_idx ON public.tenant_player_bans USING btree (player_account_id);


--
-- Name: tenants_name_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tenants_name_idx ON public.tenants USING btree (name) WHERE (deleted_at IS NULL);


--
-- Name: usage_samples_project_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_project_ts_idx ON ONLY public.usage_samples USING btree (project_id, ts);


--
-- Name: usage_samples_2026_07_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_07_project_id_ts_idx ON public.usage_samples_2026_07 USING btree (project_id, ts);


--
-- Name: usage_samples_tenant_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_tenant_ts_idx ON ONLY public.usage_samples USING btree (tenant_id, ts);


--
-- Name: usage_samples_2026_07_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_07_tenant_id_ts_idx ON public.usage_samples_2026_07 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2026_08_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_08_project_id_ts_idx ON public.usage_samples_2026_08 USING btree (project_id, ts);


--
-- Name: usage_samples_2026_08_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_08_tenant_id_ts_idx ON public.usage_samples_2026_08 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2026_09_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_09_project_id_ts_idx ON public.usage_samples_2026_09 USING btree (project_id, ts);


--
-- Name: usage_samples_2026_09_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_09_tenant_id_ts_idx ON public.usage_samples_2026_09 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2026_10_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_10_project_id_ts_idx ON public.usage_samples_2026_10 USING btree (project_id, ts);


--
-- Name: usage_samples_2026_10_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_10_tenant_id_ts_idx ON public.usage_samples_2026_10 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2026_11_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_11_project_id_ts_idx ON public.usage_samples_2026_11 USING btree (project_id, ts);


--
-- Name: usage_samples_2026_11_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_11_tenant_id_ts_idx ON public.usage_samples_2026_11 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2026_12_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_12_project_id_ts_idx ON public.usage_samples_2026_12 USING btree (project_id, ts);


--
-- Name: usage_samples_2026_12_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2026_12_tenant_id_ts_idx ON public.usage_samples_2026_12 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2027_01_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_01_project_id_ts_idx ON public.usage_samples_2027_01 USING btree (project_id, ts);


--
-- Name: usage_samples_2027_01_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_01_tenant_id_ts_idx ON public.usage_samples_2027_01 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2027_02_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_02_project_id_ts_idx ON public.usage_samples_2027_02 USING btree (project_id, ts);


--
-- Name: usage_samples_2027_02_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_02_tenant_id_ts_idx ON public.usage_samples_2027_02 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2027_03_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_03_project_id_ts_idx ON public.usage_samples_2027_03 USING btree (project_id, ts);


--
-- Name: usage_samples_2027_03_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_03_tenant_id_ts_idx ON public.usage_samples_2027_03 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2027_04_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_04_project_id_ts_idx ON public.usage_samples_2027_04 USING btree (project_id, ts);


--
-- Name: usage_samples_2027_04_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_04_tenant_id_ts_idx ON public.usage_samples_2027_04 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2027_05_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_05_project_id_ts_idx ON public.usage_samples_2027_05 USING btree (project_id, ts);


--
-- Name: usage_samples_2027_05_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_05_tenant_id_ts_idx ON public.usage_samples_2027_05 USING btree (tenant_id, ts);


--
-- Name: usage_samples_2027_06_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_06_project_id_ts_idx ON public.usage_samples_2027_06 USING btree (project_id, ts);


--
-- Name: usage_samples_2027_06_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_2027_06_tenant_id_ts_idx ON public.usage_samples_2027_06 USING btree (tenant_id, ts);


--
-- Name: usage_samples_default_project_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_default_project_id_ts_idx ON public.usage_samples_default USING btree (project_id, ts);


--
-- Name: usage_samples_default_tenant_id_ts_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX usage_samples_default_tenant_id_ts_idx ON public.usage_samples_default USING btree (tenant_id, ts);


--
-- Name: usage_samples_2026_07_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2026_07_pkey;


--
-- Name: usage_samples_2026_07_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2026_07_project_id_ts_idx;


--
-- Name: usage_samples_2026_07_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2026_07_tenant_id_ts_idx;


--
-- Name: usage_samples_2026_08_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2026_08_pkey;


--
-- Name: usage_samples_2026_08_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2026_08_project_id_ts_idx;


--
-- Name: usage_samples_2026_08_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2026_08_tenant_id_ts_idx;


--
-- Name: usage_samples_2026_09_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2026_09_pkey;


--
-- Name: usage_samples_2026_09_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2026_09_project_id_ts_idx;


--
-- Name: usage_samples_2026_09_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2026_09_tenant_id_ts_idx;


--
-- Name: usage_samples_2026_10_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2026_10_pkey;


--
-- Name: usage_samples_2026_10_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2026_10_project_id_ts_idx;


--
-- Name: usage_samples_2026_10_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2026_10_tenant_id_ts_idx;


--
-- Name: usage_samples_2026_11_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2026_11_pkey;


--
-- Name: usage_samples_2026_11_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2026_11_project_id_ts_idx;


--
-- Name: usage_samples_2026_11_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2026_11_tenant_id_ts_idx;


--
-- Name: usage_samples_2026_12_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2026_12_pkey;


--
-- Name: usage_samples_2026_12_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2026_12_project_id_ts_idx;


--
-- Name: usage_samples_2026_12_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2026_12_tenant_id_ts_idx;


--
-- Name: usage_samples_2027_01_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2027_01_pkey;


--
-- Name: usage_samples_2027_01_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2027_01_project_id_ts_idx;


--
-- Name: usage_samples_2027_01_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2027_01_tenant_id_ts_idx;


--
-- Name: usage_samples_2027_02_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2027_02_pkey;


--
-- Name: usage_samples_2027_02_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2027_02_project_id_ts_idx;


--
-- Name: usage_samples_2027_02_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2027_02_tenant_id_ts_idx;


--
-- Name: usage_samples_2027_03_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2027_03_pkey;


--
-- Name: usage_samples_2027_03_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2027_03_project_id_ts_idx;


--
-- Name: usage_samples_2027_03_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2027_03_tenant_id_ts_idx;


--
-- Name: usage_samples_2027_04_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2027_04_pkey;


--
-- Name: usage_samples_2027_04_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2027_04_project_id_ts_idx;


--
-- Name: usage_samples_2027_04_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2027_04_tenant_id_ts_idx;


--
-- Name: usage_samples_2027_05_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2027_05_pkey;


--
-- Name: usage_samples_2027_05_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2027_05_project_id_ts_idx;


--
-- Name: usage_samples_2027_05_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2027_05_tenant_id_ts_idx;


--
-- Name: usage_samples_2027_06_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_2027_06_pkey;


--
-- Name: usage_samples_2027_06_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_2027_06_project_id_ts_idx;


--
-- Name: usage_samples_2027_06_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_2027_06_tenant_id_ts_idx;


--
-- Name: usage_samples_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_pkey ATTACH PARTITION public.usage_samples_default_pkey;


--
-- Name: usage_samples_default_project_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_project_ts_idx ATTACH PARTITION public.usage_samples_default_project_id_ts_idx;


--
-- Name: usage_samples_default_tenant_id_ts_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.usage_samples_tenant_ts_idx ATTACH PARTITION public.usage_samples_default_tenant_id_ts_idx;


--
-- Name: fleet_allocation_events fleet_allocation_events_trim_trg; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER fleet_allocation_events_trim_trg AFTER INSERT ON public.fleet_allocation_events FOR EACH ROW EXECUTE FUNCTION public.fleet_allocation_events_trim();


--
-- Name: matchmaking_tickets matchmaking_tickets_notify; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER matchmaking_tickets_notify AFTER INSERT ON public.matchmaking_tickets FOR EACH ROW WHEN ((new.status = 'queued'::public.ticket_status)) EXECUTE FUNCTION public.notify_matchmaker_ticket();


--
-- Name: api_keys api_keys_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: api_keys api_keys_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: audit_log audit_log_actor_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES public.project_players(id);


--
-- Name: audit_log audit_log_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: control_panel_invitations control_panel_invitations_invited_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_invitations
    ADD CONSTRAINT control_panel_invitations_invited_by_user_id_fkey FOREIGN KEY (invited_by_user_id) REFERENCES public.control_panel_users(id) ON DELETE CASCADE;


--
-- Name: control_panel_invitations control_panel_invitations_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_invitations
    ADD CONSTRAINT control_panel_invitations_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: control_panel_memberships control_panel_memberships_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_memberships
    ADD CONSTRAINT control_panel_memberships_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: control_panel_memberships control_panel_memberships_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_memberships
    ADD CONSTRAINT control_panel_memberships_user_id_fkey FOREIGN KEY (control_panel_user_id) REFERENCES public.control_panel_users(id) ON DELETE CASCADE;


--
-- Name: control_panel_sessions control_panel_sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_sessions
    ADD CONSTRAINT control_panel_sessions_user_id_fkey FOREIGN KEY (control_panel_user_id) REFERENCES public.control_panel_users(id) ON DELETE CASCADE;


--
-- Name: control_panel_trusted_devices control_panel_trusted_devices_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_trusted_devices
    ADD CONSTRAINT control_panel_trusted_devices_user_id_fkey FOREIGN KEY (control_panel_user_id) REFERENCES public.control_panel_users(id) ON DELETE CASCADE;


--
-- Name: control_panel_user_totp_backup_codes control_panel_user_totp_backup_codes_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_user_totp_backup_codes
    ADD CONSTRAINT control_panel_user_totp_backup_codes_user_id_fkey FOREIGN KEY (control_panel_user_id) REFERENCES public.control_panel_users(id) ON DELETE CASCADE;


--
-- Name: control_panel_user_totp control_panel_user_totp_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.control_panel_user_totp
    ADD CONSTRAINT control_panel_user_totp_user_id_fkey FOREIGN KEY (control_panel_user_id) REFERENCES public.control_panel_users(id) ON DELETE CASCADE;


--
-- Name: feature_grants feature_grants_approved_by_control_panel_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feature_grants
    ADD CONSTRAINT feature_grants_approved_by_control_panel_user_id_fkey FOREIGN KEY (approved_by_control_panel_user_id) REFERENCES public.control_panel_users(id) ON DELETE SET NULL;


--
-- Name: feature_grants feature_grants_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feature_grants
    ADD CONSTRAINT feature_grants_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: feature_grants feature_grants_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.feature_grants
    ADD CONSTRAINT feature_grants_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: fleet_allocation_events fleet_allocation_events_allocation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleet_allocation_events
    ADD CONSTRAINT fleet_allocation_events_allocation_id_fkey FOREIGN KEY (allocation_id) REFERENCES public.game_server_allocations(id) ON DELETE CASCADE;


--
-- Name: fleet_allocation_events fleet_allocation_events_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleet_allocation_events
    ADD CONSTRAINT fleet_allocation_events_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: fleets fleets_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleets
    ADD CONSTRAINT fleets_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: fleets fleets_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.fleets
    ADD CONSTRAINT fleets_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: friend_edges friend_edges_from_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.friend_edges
    ADD CONSTRAINT friend_edges_from_account_id_fkey FOREIGN KEY (from_account_id) REFERENCES public.player_accounts(id) ON DELETE CASCADE;


--
-- Name: friend_edges friend_edges_to_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.friend_edges
    ADD CONSTRAINT friend_edges_to_account_id_fkey FOREIGN KEY (to_account_id) REFERENCES public.player_accounts(id) ON DELETE CASCADE;


--
-- Name: game_invite game_invite_from_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_invite
    ADD CONSTRAINT game_invite_from_player_id_fkey FOREIGN KEY (from_player_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: game_invite game_invite_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_invite
    ADD CONSTRAINT game_invite_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: game_invite game_invite_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_invite
    ADD CONSTRAINT game_invite_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: game_invite game_invite_to_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_invite
    ADD CONSTRAINT game_invite_to_player_id_fkey FOREIGN KEY (to_player_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: game_server_allocations game_server_allocations_fleet_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_server_allocations
    ADD CONSTRAINT game_server_allocations_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES public.fleets(id) ON DELETE RESTRICT;


--
-- Name: game_server_allocations game_server_allocations_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_server_allocations
    ADD CONSTRAINT game_server_allocations_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: game_server_allocations game_server_allocations_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_server_allocations
    ADD CONSTRAINT game_server_allocations_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: game_session game_session_host_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session
    ADD CONSTRAINT game_session_host_player_id_fkey FOREIGN KEY (host_player_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: game_session_peer game_session_peer_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session_peer
    ADD CONSTRAINT game_session_peer_player_id_fkey FOREIGN KEY (player_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: game_session_peer game_session_peer_session_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session_peer
    ADD CONSTRAINT game_session_peer_session_id_fkey FOREIGN KEY (session_id) REFERENCES public.game_session(id) ON DELETE CASCADE;


--
-- Name: game_session_peer game_session_peer_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session_peer
    ADD CONSTRAINT game_session_peer_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: game_session game_session_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session
    ADD CONSTRAINT game_session_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: game_session game_session_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.game_session
    ADD CONSTRAINT game_session_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: leaderboard_entries leaderboard_entries_leaderboard_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboard_entries
    ADD CONSTRAINT leaderboard_entries_leaderboard_id_fkey FOREIGN KEY (leaderboard_id) REFERENCES public.leaderboards(id) ON DELETE CASCADE;


--
-- Name: leaderboard_entries leaderboard_entries_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboard_entries
    ADD CONSTRAINT leaderboard_entries_player_id_fkey FOREIGN KEY (player_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: leaderboard_entries leaderboard_entries_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboard_entries
    ADD CONSTRAINT leaderboard_entries_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: leaderboards leaderboards_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboards
    ADD CONSTRAINT leaderboards_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: leaderboards leaderboards_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.leaderboards
    ADD CONSTRAINT leaderboards_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: matchmaker_matches matchmaker_matches_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaker_matches
    ADD CONSTRAINT matchmaker_matches_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: matchmaker_matches matchmaker_matches_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaker_matches
    ADD CONSTRAINT matchmaker_matches_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: matchmaking_tickets matchmaking_tickets_fleet_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES public.fleets(id) ON DELETE RESTRICT;


--
-- Name: matchmaking_tickets matchmaking_tickets_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_player_id_fkey FOREIGN KEY (player_id) REFERENCES public.project_players(id) ON DELETE CASCADE NOT VALID;


--
-- Name: matchmaking_tickets matchmaking_tickets_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: matchmaking_tickets matchmaking_tickets_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: player_account_sessions player_account_sessions_player_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_sessions
    ADD CONSTRAINT player_account_sessions_player_account_id_fkey FOREIGN KEY (player_account_id) REFERENCES public.player_accounts(id) ON DELETE CASCADE;


--
-- Name: player_account_totp_backup_codes player_account_totp_backup_codes_player_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_totp_backup_codes
    ADD CONSTRAINT player_account_totp_backup_codes_player_account_id_fkey FOREIGN KEY (player_account_id) REFERENCES public.player_accounts(id) ON DELETE CASCADE;


--
-- Name: player_account_totp player_account_totp_player_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_totp
    ADD CONSTRAINT player_account_totp_player_account_id_fkey FOREIGN KEY (player_account_id) REFERENCES public.player_accounts(id) ON DELETE CASCADE;


--
-- Name: player_account_trusted_devices player_account_trusted_devices_player_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_account_trusted_devices
    ADD CONSTRAINT player_account_trusted_devices_player_account_id_fkey FOREIGN KEY (player_account_id) REFERENCES public.player_accounts(id) ON DELETE CASCADE;


--
-- Name: player_invitations player_invitations_invited_by_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_invitations
    ADD CONSTRAINT player_invitations_invited_by_user_id_fkey FOREIGN KEY (invited_by_user_id) REFERENCES public.control_panel_users(id) ON DELETE CASCADE;


--
-- Name: player_invitations player_invitations_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_invitations
    ADD CONSTRAINT player_invitations_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: player_invitations player_invitations_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.player_invitations
    ADD CONSTRAINT player_invitations_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: presence presence_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.presence
    ADD CONSTRAINT presence_player_id_fkey FOREIGN KEY (player_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: presence presence_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.presence
    ADD CONSTRAINT presence_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: project_players project_players_player_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_players
    ADD CONSTRAINT project_players_player_account_id_fkey FOREIGN KEY (player_account_id) REFERENCES public.player_accounts(id) ON DELETE SET NULL;


--
-- Name: project_players project_players_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_players
    ADD CONSTRAINT project_players_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: project_players project_players_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.project_players
    ADD CONSTRAINT project_players_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: projects projects_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.projects
    ADD CONSTRAINT projects_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: rate_limit_overrides rate_limit_overrides_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rate_limit_overrides
    ADD CONSTRAINT rate_limit_overrides_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: rate_limit_overrides rate_limit_overrides_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rate_limit_overrides
    ADD CONSTRAINT rate_limit_overrides_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: rate_limit_overrides rate_limit_overrides_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.rate_limit_overrides
    ADD CONSTRAINT rate_limit_overrides_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.control_panel_users(id) ON DELETE SET NULL;


--
-- Name: river_client_queue river_client_queue_river_client_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.river_client_queue
    ADD CONSTRAINT river_client_queue_river_client_id_fkey FOREIGN KEY (river_client_id) REFERENCES public.river_client(id) ON DELETE CASCADE;


--
-- Name: sessions sessions_player_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_player_id_fkey FOREIGN KEY (player_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: sessions sessions_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: sessions sessions_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: storage_objects storage_objects_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_objects
    ADD CONSTRAINT storage_objects_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES public.project_players(id) ON DELETE CASCADE;


--
-- Name: storage_objects storage_objects_project_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_objects
    ADD CONSTRAINT storage_objects_project_id_fkey FOREIGN KEY (project_id) REFERENCES public.projects(id) ON DELETE CASCADE;


--
-- Name: storage_objects storage_objects_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_objects
    ADD CONSTRAINT storage_objects_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: tenant_player_bans tenant_player_bans_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_player_bans
    ADD CONSTRAINT tenant_player_bans_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.control_panel_users(id) ON DELETE SET NULL;


--
-- Name: tenant_player_bans tenant_player_bans_player_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_player_bans
    ADD CONSTRAINT tenant_player_bans_player_account_id_fkey FOREIGN KEY (player_account_id) REFERENCES public.player_accounts(id) ON DELETE CASCADE;


--
-- Name: tenant_player_bans tenant_player_bans_tenant_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tenant_player_bans
    ADD CONSTRAINT tenant_player_bans_tenant_id_fkey FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE;


--
-- Name: api_keys; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.api_keys ENABLE ROW LEVEL SECURITY;

--
-- Name: api_keys api_keys_bootstrap; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY api_keys_bootstrap ON public.api_keys FOR SELECT USING (((current_setting('app.tenant_id'::text, true) IS NULL) OR (current_setting('app.tenant_id'::text, true) = ''::text)));


--
-- Name: api_keys api_keys_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY api_keys_isolation ON public.api_keys USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: audit_log; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.audit_log ENABLE ROW LEVEL SECURITY;

--
-- Name: audit_log audit_log_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY audit_log_isolation ON public.audit_log USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: feature_grants; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.feature_grants ENABLE ROW LEVEL SECURITY;

--
-- Name: feature_grants feature_grants_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY feature_grants_isolation ON public.feature_grants USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: fleet_allocation_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.fleet_allocation_events ENABLE ROW LEVEL SECURITY;

--
-- Name: fleet_allocation_events fleet_allocation_events_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY fleet_allocation_events_isolation ON public.fleet_allocation_events USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: fleets; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.fleets ENABLE ROW LEVEL SECURITY;

--
-- Name: fleets fleets_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY fleets_isolation ON public.fleets USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: game_invite; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.game_invite ENABLE ROW LEVEL SECURITY;

--
-- Name: game_invite game_invite_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY game_invite_isolation ON public.game_invite USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: game_server_allocations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.game_server_allocations ENABLE ROW LEVEL SECURITY;

--
-- Name: game_server_allocations game_server_allocations_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY game_server_allocations_isolation ON public.game_server_allocations USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: game_session; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.game_session ENABLE ROW LEVEL SECURITY;

--
-- Name: game_session game_session_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY game_session_isolation ON public.game_session USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: game_session_peer; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.game_session_peer ENABLE ROW LEVEL SECURITY;

--
-- Name: game_session_peer game_session_peer_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY game_session_peer_isolation ON public.game_session_peer USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: leaderboard_entries; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.leaderboard_entries ENABLE ROW LEVEL SECURITY;

--
-- Name: leaderboard_entries leaderboard_entries_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY leaderboard_entries_isolation ON public.leaderboard_entries USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: leaderboards; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.leaderboards ENABLE ROW LEVEL SECURITY;

--
-- Name: leaderboards leaderboards_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY leaderboards_isolation ON public.leaderboards USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: matchmaker_matches; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.matchmaker_matches ENABLE ROW LEVEL SECURITY;

--
-- Name: matchmaker_matches matchmaker_matches_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY matchmaker_matches_isolation ON public.matchmaker_matches USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: matchmaker_matches matchmaker_matches_worker_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY matchmaker_matches_worker_delete ON public.matchmaker_matches FOR DELETE USING ((NULLIF(current_setting('app.tenant_id'::text, true), ''::text) IS NULL));


--
-- Name: matchmaking_tickets; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.matchmaking_tickets ENABLE ROW LEVEL SECURITY;

--
-- Name: matchmaking_tickets matchmaking_tickets_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY matchmaking_tickets_isolation ON public.matchmaking_tickets USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: matchmaking_tickets matchmaking_tickets_worker_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY matchmaking_tickets_worker_delete ON public.matchmaking_tickets FOR DELETE USING ((NULLIF(current_setting('app.tenant_id'::text, true), ''::text) IS NULL));


--
-- Name: matchmaking_tickets matchmaking_tickets_worker_select; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY matchmaking_tickets_worker_select ON public.matchmaking_tickets FOR SELECT USING ((NULLIF(current_setting('app.tenant_id'::text, true), ''::text) IS NULL));


--
-- Name: matchmaking_tickets matchmaking_tickets_worker_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY matchmaking_tickets_worker_update ON public.matchmaking_tickets FOR UPDATE USING ((NULLIF(current_setting('app.tenant_id'::text, true), ''::text) IS NULL)) WITH CHECK ((NULLIF(current_setting('app.tenant_id'::text, true), ''::text) IS NULL));


--
-- Name: player_invitations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.player_invitations ENABLE ROW LEVEL SECURITY;

--
-- Name: player_invitations player_invitations_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY player_invitations_isolation ON public.player_invitations USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: presence; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.presence ENABLE ROW LEVEL SECURITY;

--
-- Name: presence presence_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY presence_isolation ON public.presence USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: project_players; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.project_players ENABLE ROW LEVEL SECURITY;

--
-- Name: project_players project_players_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY project_players_isolation ON public.project_players USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: projects; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.projects ENABLE ROW LEVEL SECURITY;

--
-- Name: projects projects_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY projects_isolation ON public.projects USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: sessions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: sessions sessions_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY sessions_isolation ON public.sessions USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: storage_objects; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.storage_objects ENABLE ROW LEVEL SECURITY;

--
-- Name: storage_objects storage_objects_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY storage_objects_isolation ON public.storage_objects USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: tenants; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.tenants ENABLE ROW LEVEL SECURITY;

--
-- Name: tenants tenants_bootstrap; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tenants_bootstrap ON public.tenants FOR SELECT USING (((current_setting('app.tenant_id'::text, true) IS NULL) OR (current_setting('app.tenant_id'::text, true) = ''::text)));


--
-- Name: tenants tenants_control_panel_membership; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tenants_control_panel_membership ON public.tenants FOR SELECT USING (((NULLIF(current_setting('app.control_panel_user_id'::text, true), ''::text) IS NOT NULL) AND (EXISTS ( SELECT 1
   FROM public.control_panel_users u
  WHERE ((u.id = (NULLIF(current_setting('app.control_panel_user_id'::text, true), ''::text))::bigint) AND (u.is_platform_admin OR (EXISTS ( SELECT 1
           FROM public.control_panel_memberships m
          WHERE ((m.control_panel_user_id = u.id) AND (m.tenant_id = tenants.id))))))))));


--
-- Name: tenants tenants_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tenants_isolation ON public.tenants USING ((id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK (((id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint) OR (NULLIF(current_setting('app.allow_tenant_bootstrap'::text, true), ''::text) = '1'::text)));


--
-- Name: usage_samples; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.usage_samples ENABLE ROW LEVEL SECURITY;

--
-- Name: usage_samples usage_samples_isolation; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY usage_samples_isolation ON public.usage_samples USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::bigint));


--
-- Name: FUNCTION control_panel_create_tenant(p_actor_user_id bigint, p_tenant_name text, p_project_name text, p_key_hash bytea, p_key_label text); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.control_panel_create_tenant(p_actor_user_id bigint, p_tenant_name text, p_project_name text, p_key_hash bytea, p_key_label text) FROM PUBLIC;
GRANT ALL ON FUNCTION public.control_panel_create_tenant(p_actor_user_id bigint, p_tenant_name text, p_project_name text, p_key_hash bytea, p_key_label text) TO ggscale_app;


--
-- Name: FUNCTION player_account_linked_projects(p_account_id uuid); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) TO ggscale_app;


--
-- Name: FUNCTION player_invite_lookup(p_code_hash bytea); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.player_invite_lookup(p_code_hash bytea) FROM PUBLIC;
GRANT ALL ON FUNCTION public.player_invite_lookup(p_code_hash bytea) TO ggscale_app;


--
-- Name: FUNCTION project_join_context(p_project_id bigint); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.project_join_context(p_project_id bigint) FROM PUBLIC;
GRANT ALL ON FUNCTION public.project_join_context(p_project_id bigint) TO ggscale_app;


--
-- Name: FUNCTION project_player_tenant(p_player_id bigint); Type: ACL; Schema: public; Owner: -
--

REVOKE ALL ON FUNCTION public.project_player_tenant(p_player_id bigint) FROM PUBLIC;
GRANT ALL ON FUNCTION public.project_player_tenant(p_player_id bigint) TO ggscale_app;


--
-- Name: TABLE api_keys; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.api_keys TO ggscale_app;


--
-- Name: SEQUENCE api_keys_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.api_keys_id_seq TO ggscale_app;


--
-- Name: TABLE audit_log; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.audit_log TO ggscale_app;


--
-- Name: SEQUENCE audit_log_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.audit_log_id_seq TO ggscale_app;


--
-- Name: TABLE casbin_rule; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE ON TABLE public.casbin_rule TO ggscale_app;


--
-- Name: SEQUENCE casbin_rule_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.casbin_rule_id_seq TO ggscale_app;


--
-- Name: TABLE control_panel_invitations; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.control_panel_invitations TO ggscale_app;


--
-- Name: SEQUENCE control_panel_invitations_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.control_panel_invitations_id_seq TO ggscale_app;


--
-- Name: TABLE control_panel_memberships; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.control_panel_memberships TO ggscale_app;


--
-- Name: SEQUENCE control_panel_memberships_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.control_panel_memberships_id_seq TO ggscale_app;


--
-- Name: TABLE control_panel_sessions; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.control_panel_sessions TO ggscale_app;


--
-- Name: SEQUENCE control_panel_sessions_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.control_panel_sessions_id_seq TO ggscale_app;


--
-- Name: TABLE control_panel_trusted_devices; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.control_panel_trusted_devices TO ggscale_app;


--
-- Name: SEQUENCE control_panel_trusted_devices_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.control_panel_trusted_devices_id_seq TO ggscale_app;


--
-- Name: TABLE control_panel_user_totp; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.control_panel_user_totp TO ggscale_app;


--
-- Name: TABLE control_panel_user_totp_backup_codes; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.control_panel_user_totp_backup_codes TO ggscale_app;


--
-- Name: SEQUENCE control_panel_user_totp_backup_codes_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.control_panel_user_totp_backup_codes_id_seq TO ggscale_app;


--
-- Name: TABLE control_panel_users; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.control_panel_users TO ggscale_app;


--
-- Name: SEQUENCE control_panel_users_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.control_panel_users_id_seq TO ggscale_app;


--
-- Name: TABLE feature_grants; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.feature_grants TO ggscale_app;


--
-- Name: SEQUENCE feature_grants_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.feature_grants_id_seq TO ggscale_app;


--
-- Name: TABLE fleet_allocation_events; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.fleet_allocation_events TO ggscale_app;


--
-- Name: SEQUENCE fleet_allocation_events_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.fleet_allocation_events_id_seq TO ggscale_app;


--
-- Name: TABLE fleets; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.fleets TO ggscale_app;


--
-- Name: SEQUENCE fleets_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.fleets_id_seq TO ggscale_app;


--
-- Name: TABLE friend_edges; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.friend_edges TO ggscale_app;


--
-- Name: SEQUENCE friend_edges_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.friend_edges_id_seq TO ggscale_app;


--
-- Name: TABLE game_invite; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.game_invite TO ggscale_app;


--
-- Name: SEQUENCE game_invite_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.game_invite_id_seq TO ggscale_app;


--
-- Name: TABLE game_server_allocations; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.game_server_allocations TO ggscale_app;


--
-- Name: SEQUENCE game_server_allocations_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.game_server_allocations_id_seq TO ggscale_app;


--
-- Name: TABLE game_session; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.game_session TO ggscale_app;


--
-- Name: TABLE game_session_peer; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.game_session_peer TO ggscale_app;


--
-- Name: TABLE leaderboard_entries; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.leaderboard_entries TO ggscale_app;


--
-- Name: SEQUENCE leaderboard_entries_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.leaderboard_entries_id_seq TO ggscale_app;


--
-- Name: TABLE leaderboards; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.leaderboards TO ggscale_app;


--
-- Name: SEQUENCE leaderboards_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.leaderboards_id_seq TO ggscale_app;


--
-- Name: TABLE matchmaker_matches; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.matchmaker_matches TO ggscale_app;


--
-- Name: TABLE matchmaking_tickets; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.matchmaking_tickets TO ggscale_app;


--
-- Name: SEQUENCE matchmaking_tickets_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.matchmaking_tickets_id_seq TO ggscale_app;


--
-- Name: TABLE platform_audit_log; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.platform_audit_log TO ggscale_app;


--
-- Name: SEQUENCE platform_audit_log_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.platform_audit_log_id_seq TO ggscale_app;


--
-- Name: TABLE player_account_sessions; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.player_account_sessions TO ggscale_app;


--
-- Name: SEQUENCE player_account_sessions_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.player_account_sessions_id_seq TO ggscale_app;


--
-- Name: TABLE player_account_totp; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.player_account_totp TO ggscale_app;


--
-- Name: TABLE player_account_totp_backup_codes; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.player_account_totp_backup_codes TO ggscale_app;


--
-- Name: SEQUENCE player_account_totp_backup_codes_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.player_account_totp_backup_codes_id_seq TO ggscale_app;


--
-- Name: TABLE player_account_trusted_devices; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.player_account_trusted_devices TO ggscale_app;


--
-- Name: SEQUENCE player_account_trusted_devices_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.player_account_trusted_devices_id_seq TO ggscale_app;


--
-- Name: TABLE player_accounts; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.player_accounts TO ggscale_app;


--
-- Name: TABLE player_invitations; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.player_invitations TO ggscale_app;


--
-- Name: SEQUENCE player_invitations_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.player_invitations_id_seq TO ggscale_app;


--
-- Name: TABLE presence; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.presence TO ggscale_app;


--
-- Name: TABLE project_players; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.project_players TO ggscale_app;


--
-- Name: SEQUENCE project_players_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.project_players_id_seq TO ggscale_app;


--
-- Name: TABLE projects; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.projects TO ggscale_app;


--
-- Name: SEQUENCE projects_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.projects_id_seq TO ggscale_app;


--
-- Name: TABLE rate_limit_overrides; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.rate_limit_overrides TO ggscale_app;


--
-- Name: SEQUENCE rate_limit_overrides_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.rate_limit_overrides_id_seq TO ggscale_app;


--
-- Name: TABLE river_client; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.river_client TO ggscale_app;


--
-- Name: TABLE river_client_queue; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.river_client_queue TO ggscale_app;


--
-- Name: TABLE river_job; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.river_job TO ggscale_app;


--
-- Name: SEQUENCE river_job_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.river_job_id_seq TO ggscale_app;


--
-- Name: TABLE river_leader; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.river_leader TO ggscale_app;


--
-- Name: TABLE river_queue; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.river_queue TO ggscale_app;


--
-- Name: TABLE server_secrets; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.server_secrets TO ggscale_app;


--
-- Name: TABLE sessions; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.sessions TO ggscale_app;


--
-- Name: SEQUENCE sessions_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.sessions_id_seq TO ggscale_app;


--
-- Name: TABLE storage_objects; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.storage_objects TO ggscale_app;


--
-- Name: SEQUENCE storage_objects_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.storage_objects_id_seq TO ggscale_app;


--
-- Name: TABLE tenant_player_bans; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.tenant_player_bans TO ggscale_app;


--
-- Name: SEQUENCE tenant_player_bans_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.tenant_player_bans_id_seq TO ggscale_app;


--
-- Name: TABLE tenants; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.tenants TO ggscale_app;


--
-- Name: SEQUENCE tenants_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.tenants_id_seq TO ggscale_app;


--
-- Name: TABLE usage_samples; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.usage_samples TO ggscale_app;


--
-- Name: SEQUENCE usage_samples_id_seq; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,USAGE ON SEQUENCE public.usage_samples_id_seq TO ggscale_app;


--
-- Name: DEFAULT PRIVILEGES FOR SEQUENCES; Type: DEFAULT ACL; Schema: public; Owner: -
--

ALTER DEFAULT PRIVILEGES FOR ROLE ggscale IN SCHEMA public GRANT SELECT,USAGE ON SEQUENCES TO ggscale_app;


--
-- PostgreSQL database dump complete
--




--
-- Static Casbin policy (p-rules). Grouping (g) rows are written at runtime by
-- the application as tenants, API keys, and control-panel members are
-- provisioned, so only the static policy is seeded here.
--
INSERT INTO public.casbin_rule (ptype, v0, v1, v2, v3) VALUES
    ('p','role:analyst','*','project','read'),
    ('p','role:analyst','*','project:*:allocation','read'),
    ('p','role:analyst','*','project:*:matchmaker','read'),
    ('p','role:analyst','*','project:*:players','read'),
    ('p','role:api_client','*','auth','create'),
    ('p','role:api_client','*','profile','read'),
    ('p','role:api_fleet_runtime','*','player','verify'),
    ('p','role:api_fleet_runtime','*','project:*:allocation','read'),
    ('p','role:api_fleet_runtime','*','project:*:allocation','update'),
    ('p','role:api_server','*','leaderboard','submit'),
    ('p','role:api_server','*','player','verify'),
    ('p','role:developer','*','project','read'),
    ('p','role:developer','*','project:*:config','update'),
    ('p','role:developer','*','project:*:players','read'),
    ('p','role:fleet_operator','*','project:*:allocation','allocate'),
    ('p','role:fleet_operator','*','project:*:allocation','deallocate'),
    ('p','role:fleet_operator','*','project:*:allocation','read'),
    ('p','role:fleet_operator','*','project:*:fleet','manage'),
    ('p','role:fleet_operator','*','project:*:matchmaker','read'),
    ('p','role:platform_admin','*','control_panel_user','disable'),
    ('p','role:platform_admin','*','control_panel_user','read'),
    ('p','role:platform_admin','*','platform:plugins','read'),
    ('p','role:platform_admin','*','tenant','read'),
    ('p','role:platform_owner','*','*','*'),
    ('p','role:platform_support','*','control_panel_user','read'),
    ('p','role:platform_support','*','tenant','read'),
    ('p','role:player_standard','*','friends','manage'),
    ('p','role:player_standard','*','leaderboard','read'),
    ('p','role:player_standard','*','profile','read'),
    ('p','role:player_standard','*','profile','update'),
    ('p','role:player_standard','*','project:*:matchmaking:dedicated','create_ticket'),
    ('p','role:player_standard','*','project:*:relay','issue_credentials'),
    ('p','role:player_standard','*','realtime','connect'),
    ('p','role:player_standard','*','storage','manage'),
    ('p','role:player_verified','*','friends','manage'),
    ('p','role:player_verified','*','leaderboard','read'),
    ('p','role:player_verified','*','profile','read'),
    ('p','role:player_verified','*','profile','update'),
    ('p','role:player_verified','*','realtime','connect'),
    ('p','role:player_verified','*','storage','manage'),
    ('p','role:security_admin','*','api_key:secret','manage'),
    ('p','role:security_admin','*','audit','read'),
    ('p','role:security_admin','*','custom_token','manage'),
    ('p','role:security_admin','*','feature_request','create'),
    ('p','role:support','*','project:*:players','disable'),
    ('p','role:support','*','project:*:players','invite'),
    ('p','role:support','*','project:*:players','read'),
    ('p','role:tenant_admin','*','api_key:publishable','manage'),
    ('p','role:tenant_admin','*','audit','read'),
    ('p','role:tenant_admin','*','project','manage'),
    ('p','role:tenant_admin','*','project:*:leaderboard','manage'),
    ('p','role:tenant_admin','*','project:*:players','manage'),
    ('p','role:tenant_owner','*','api_key:*','manage'),
    ('p','role:tenant_owner','*','audit','read'),
    ('p','role:tenant_owner','*','project','manage'),
    ('p','role:tenant_owner','*','project:*:allocation','*'),
    ('p','role:tenant_owner','*','project:*:config','*'),
    ('p','role:tenant_owner','*','project:*:fleet','*'),
    ('p','role:tenant_owner','*','project:*:leaderboard','manage'),
    ('p','role:tenant_owner','*','project:*:matchmaker','*'),
    ('p','role:tenant_owner','*','project:*:matchmaking:dedicated','*'),
    ('p','role:tenant_owner','*','project:*:players','*'),
    ('p','role:tenant_owner','*','project:*:relay','*'),
    ('p','role:tenant_owner','*','team','manage'),
    ('p','role:tenant_owner','*','tenant','manage')

ON CONFLICT DO NOTHING;

-- Restore a normal search_path for subsequent migrations. The dump above zeroed
-- it at session scope (so every object had to be schema-qualified); without this
-- reset, later migrations on the same connection see no schema for unqualified
-- CREATE/DROP and fail with SQLSTATE 3F000.
SELECT pg_catalog.set_config('search_path', 'public', false);
