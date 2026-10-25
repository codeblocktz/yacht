-- The audit log is written here and read by audit.List, which composes its
-- filters at runtime — the hybrid sqlc.yaml describes.

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (owner_id, actor_kind, actor_id, actor_name, ip, user_agent, action, target_kind, target, detail)
VALUES (@owner_id, @actor_kind, @actor_id, @actor_name, @ip, @user_agent, @action, @target_kind, @target, @detail);
